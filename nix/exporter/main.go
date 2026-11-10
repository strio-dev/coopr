// coopr-nix-export materializes the pinned upstream Nix image description as
// an OCI layout. It is a separate Nix integration tool, not part of Coopr core.
package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/nlewo/nix2container/nix"
	"github.com/nlewo/nix2container/types"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const descriptionLimit = 64 << 20

type exportStats struct {
	Layers       int   `json:"layers"`
	RawBytes     int64 `json:"rawBytes"`
	EncodedBytes int64 `json:"encodedBytes"`
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: coopr-nix-export IMAGE_JSON NEW_OCI_DIRECTORY")
		os.Exit(2)
	}
	image, err := readImage(os.Args[1])
	var stats exportStats
	if err == nil {
		stats, err = exportImage(ctx, image, os.Args[2])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "coopr Nix export:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(stats); err != nil {
		fmt.Fprintln(os.Stderr, "coopr Nix export statistics:", err)
		os.Exit(1)
	}
}

func readImage(path string) (image types.Image, retErr error) {
	info, err := os.Stat(path)
	if err != nil {
		return image, err
	}
	if !info.Mode().IsRegular() {
		return image, errors.New("image description must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return image, err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return image, errors.New("image description must be a regular file")
	}
	reader := &io.LimitedReader{R: file, N: descriptionLimit + 1}
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&image); err != nil {
		return image, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return image, errors.New("image description must contain one JSON object")
	}
	if reader.N <= 0 {
		return image, errors.New("image description exceeds 64 MiB")
	}
	if image.Version != types.ImageVersion || image.Arch != "amd64" && image.Arch != "arm64" {
		return image, errors.New("unsupported Nix image description version or architecture")
	}
	return image, nil
}

func exportImage(ctx context.Context, image types.Image, output string) (stats exportStats, retErr error) {
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	// Refuse existing paths. Cleanup owns only the newly created private output;
	// no user-provided layout or shared store is overwritten or pruned.
	if err := os.Mkdir(output, 0700); err != nil {
		return stats, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, os.RemoveAll(output))
		}
	}()
	if err := os.MkdirAll(filepath.Join(output, "blobs", "sha256"), 0755); err != nil {
		return stats, err
	}
	config, err := imageConfiguration(image)
	if err != nil {
		return stats, fmt.Errorf("upstream Nix image configuration: %w", err)
	}
	configuration, err := writeBlob(output, v1.MediaTypeImageConfig, config)
	if err != nil {
		return stats, err
	}
	manifest := v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest,
		Config: configuration, Layers: make([]v1.Descriptor, 0, len(image.Layers))}
	for index, layer := range image.Layers {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		descriptor, raw, err := writeLayer(ctx, output, layer)
		if err != nil {
			return stats, fmt.Errorf("layer %d: %w", index, err)
		}
		stats.Layers++
		stats.RawBytes += raw
		stats.EncodedBytes += descriptor.Size
		manifest.Layers = append(manifest.Layers, descriptor)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return stats, err
	}
	root, err := writeBlob(output, v1.MediaTypeImageManifest, data)
	if err != nil {
		return stats, err
	}
	root.Annotations = map[string]string{v1.AnnotationRefName: "image"}
	index := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []v1.Descriptor{root}}
	data, err = json.Marshal(index)
	if err != nil {
		return stats, err
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	if err := os.WriteFile(filepath.Join(output, "oci-layout"), []byte("{\"imageLayoutVersion\":\"1.0.0\"}\n"), 0644); err != nil {
		return stats, err
	}
	// Publish the root index only after all referenced blobs have been verified.
	if err := os.WriteFile(filepath.Join(output, "index.json"), data, 0644); err != nil {
		return stats, err
	}
	if err := os.Chmod(output, 0755); err != nil {
		return stats, err
	}
	return stats, nil
}

// imageConfiguration preserves upstream configuration bytes except for its
// generated zero-layer RootFS, which upstream leaves without an OCI type.
func imageConfiguration(image types.Image) ([]byte, error) {
	data, err := nix.GetConfigBlob(image)
	if err != nil || len(image.Layers) != 0 {
		return data, err
	}
	return canonicalEmptyRootFS(data)
}

func canonicalEmptyRootFS(data []byte) ([]byte, error) {
	var config map[string]json.RawMessage
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(config["rootfs"], &root); err != nil || root == nil {
		return nil, errors.New("empty Nix image has no generated rootfs object")
	}
	var kind string
	if err := json.Unmarshal(root["type"], &kind); err != nil || kind != "" && kind != "layers" {
		return nil, errors.New("empty Nix image has contradictory rootfs type")
	}
	var ids []string
	if err := json.Unmarshal(root["diff_ids"], &ids); err != nil || len(ids) != 0 {
		return nil, errors.New("empty Nix image declares filesystem layers")
	}
	root["type"] = json.RawMessage(`"layers"`)
	root["diff_ids"] = json.RawMessage(`[]`)
	normalized, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	config["rootfs"] = normalized
	return json.Marshal(config)
}

func writeBlob(output, mediaType string, data []byte) (v1.Descriptor, error) {
	d := v1.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
	return d, os.WriteFile(filepath.Join(output, "blobs", "sha256", d.Digest.Encoded()), data, 0644)
}

func writeLayer(ctx context.Context, output string, layer types.Layer) (_ v1.Descriptor, rawSize int64, retErr error) {
	// The buildImage interface produces upstream raw Nix layers, including its
	// ownership/path rewrites. Final Coopr images never pass through this model.
	if layer.MediaType != v1.MediaTypeImageLayer || layer.Size < 0 {
		return v1.Descriptor{}, 0, errors.New("expected an upstream raw Nix layer")
	}
	if layer.LayerPath != "" {
		info, err := os.Stat(layer.LayerPath)
		if err != nil {
			return v1.Descriptor{}, 0, err
		}
		if !info.Mode().IsRegular() {
			return v1.Descriptor{}, 0, errors.New("materialized Nix layer must be a regular file")
		}
	}
	expected, err := digest.Parse(layer.Digest)
	if err != nil || expected.Algorithm() != digest.SHA256 || layer.DiffIDs != layer.Digest {
		return v1.Descriptor{}, 0, errors.New("invalid raw Nix layer digest or DiffID")
	}
	reader, _, err := nix.LayerGetBlob(layer)
	if err != nil {
		return v1.Descriptor{}, 0, err
	}
	if reader == nil {
		return v1.Descriptor{}, 0, errors.New("upstream Nix layer has no file view")
	}
	// The pinned upstream returns os.File or io.PipeReader. Both support
	// closing a blocked Read. Join the cancellation callback before cleanup,
	// and close once so cancellation cannot race a second file close.
	closeReader := sync.OnceValue(reader.Close)
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(closed)
		_ = closeReader()
	})
	defer func() {
		if !stop() {
			<-closed
		}
		retErr = errors.Join(retErr, closeReader())
		if ctx.Err() != nil {
			retErr = errors.Join(retErr, ctx.Err())
		}
	}()
	file, err := os.CreateTemp(filepath.Join(output, "blobs", "sha256"), ".layer-*")
	if err != nil {
		return v1.Descriptor{}, 0, err
	}
	path := file.Name()
	defer func() { _ = os.Remove(path) }()
	compressed := digest.Canonical.Digester()
	counter := &countWriter{writer: io.MultiWriter(file, compressed.Hash())}
	encoder := gzip.NewWriter(counter)
	raw := digest.Canonical.Digester()
	// Bound reads by the declared size, then probe one extra byte without
	// overflowing size+1. A corrupt source cannot force an unbounded copy.
	rawSize, copyErr := io.Copy(io.MultiWriter(encoder, raw.Hash()), io.LimitReader(reader, layer.Size))
	if copyErr == nil && rawSize == layer.Size {
		var extra [1]byte
		n, err := io.ReadFull(reader, extra[:])
		if n != 0 {
			copyErr = errors.New("raw Nix layer exceeds its declared size")
		} else if !errors.Is(err, io.EOF) {
			copyErr = err
		}
	}
	closeErr := errors.Join(encoder.Close(), file.Close())
	if err := errors.Join(copyErr, closeErr, ctx.Err()); err != nil {
		return v1.Descriptor{}, rawSize, err
	}
	if rawSize != layer.Size || raw.Digest() != expected {
		return v1.Descriptor{}, rawSize, errors.New("raw Nix layer content differs from its declared size or digest")
	}
	descriptor := v1.Descriptor{MediaType: v1.MediaTypeImageLayerGzip, Digest: compressed.Digest(), Size: counter.bytes}
	if err := os.Chmod(path, 0644); err != nil {
		return v1.Descriptor{}, rawSize, err
	}
	if err := os.Rename(path, filepath.Join(output, "blobs", "sha256", descriptor.Digest.Encoded())); err != nil {
		return v1.Descriptor{}, rawSize, err
	}
	return descriptor, rawSize, nil
}

type countWriter struct {
	writer io.Writer
	bytes  int64
}

func (writer *countWriter) Write(data []byte) (int, error) {
	n, err := writer.writer.Write(data)
	writer.bytes += int64(n)
	return n, err
}
