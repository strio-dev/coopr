package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nlewo/nix2container/nix"
	"github.com/nlewo/nix2container/types"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func fixtureImage(t *testing.T) types.Image {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "tool"), []byte("closure bytes\n"), 0751); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tool", filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	layers, err := nix.NewLayers([]string{root}, 1, nil,
		[]types.RewritePath{{Path: root, Regex: root, Repl: ""}}, "", nil, "",
		v1.History{CreatedBy: "upstream-nix-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	created := time.Unix(1, 0).UTC()
	return types.Image{Version: types.ImageVersion, Arch: "amd64", Created: &created, Layers: layers,
		ImageConfig: v1.ImageConfig{User: "37:42", Entrypoint: []string{"/tool"}, Cmd: []string{"--check"},
			Env: []string{"PATH=/", "LITERAL=$value\nsecond line"}, WorkingDir: "/work",
			Labels: map[string]string{"fixture": "retained"}, StopSignal: "SIGINT", ArgsEscaped: true,
			ExposedPorts: map[string]struct{}{"1234/tcp": {}}, Volumes: map[string]struct{}{"/data": {}}}}
}

func blobBytes(t *testing.T, output string, d v1.Descriptor) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(output, "blobs", d.Digest.Algorithm().String(), d.Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) != d.Size || digest.FromBytes(data) != d.Digest {
		t.Fatalf("invalid OCI blob %s", d.Digest)
	}
	return data
}

func inspectOutput(t *testing.T, output string) (v1.Descriptor, v1.Manifest) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(output, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 1 || index.Manifests[0].Annotations[v1.AnnotationRefName] != "image" {
		t.Fatalf("invalid root index %+v", index)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(blobBytes(t, output, index.Manifests[0]), &manifest); err != nil {
		t.Fatal(err)
	}
	return index.Manifests[0], manifest
}

func TestExportReusesUpstreamConfigurationAndLayerBytes(t *testing.T) {
	image := fixtureImage(t)
	output := filepath.Join(t.TempDir(), "image")
	stats, err := exportImage(t.Context(), image, output)
	if err != nil {
		t.Fatal(err)
	}
	root, manifest := inspectOutput(t, output)
	config, err := nix.GetConfigBlob(image)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(blobBytes(t, output, manifest.Config), config) {
		t.Fatal("upstream image configuration changed")
	}
	if len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != v1.MediaTypeImageLayerGzip {
		t.Fatalf("unexpected exported layers %+v", manifest.Layers)
	}
	encoded := blobBytes(t, output, manifest.Layers[0])
	reader, err := gzip.NewReader(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	if err := errors.Join(err, reader.Close()); err != nil {
		t.Fatal(err)
	}
	upstream, _, err := nix.LayerGetBlob(image.Layers[0])
	if err != nil {
		t.Fatal(err)
	}
	want, err := io.ReadAll(upstream)
	if err := errors.Join(err, upstream.Close()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, want) || digest.FromBytes(raw).String() != image.Layers[0].DiffIDs {
		t.Fatal("upstream Nix tar bytes or DiffID changed")
	}
	if stats.Layers != 1 || stats.RawBytes != int64(len(raw)) || stats.EncodedBytes != int64(len(encoded)) {
		t.Fatalf("invalid byte accounting %+v", stats)
	}
	second := filepath.Join(t.TempDir(), "second")
	if _, err := exportImage(t.Context(), image, second); err != nil {
		t.Fatal(err)
	}
	secondRoot, secondManifest := inspectOutput(t, second)
	if !reflect.DeepEqual(root, secondRoot) || !reflect.DeepEqual(manifest, secondManifest) {
		t.Fatal("same immutable inputs produced different OCI bytes")
	}
}

func TestExportRejectsChangedLayerContentAndCleansOwnOutput(t *testing.T) {
	for _, change := range []string{"digest", "size", "oversized", "media-type", "diff-id", "missing"} {
		t.Run(change, func(t *testing.T) {
			image := fixtureImage(t)
			switch change {
			case "digest":
				image.Layers[0].Digest = digest.FromString("different").String()
				image.Layers[0].DiffIDs = image.Layers[0].Digest
			case "size":
				image.Layers[0].Size++
			case "oversized":
				image.Layers[0].Size--
			case "media-type":
				image.Layers[0].MediaType = v1.MediaTypeImageLayerGzip
			case "diff-id":
				image.Layers[0].DiffIDs = digest.FromString("different").String()
			case "missing":
				image.Layers[0].Paths[0].Path += "/missing"
			}
			output := filepath.Join(t.TempDir(), "image")
			if _, err := exportImage(t.Context(), image, output); err == nil {
				t.Fatal("invalid upstream input accepted")
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial output retained: %v", err)
			}
		})
	}
}

func TestExportPreservesExistingOutputAndEarlyCancellation(t *testing.T) {
	image := fixtureImage(t)
	output := t.TempDir()
	sentinel := filepath.Join(output, "user-data")
	if err := os.WriteFile(sentinel, []byte("preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := exportImage(t.Context(), image, output); err == nil {
		t.Fatal("existing output accepted")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "preserved" {
		t.Fatalf("existing user data changed: %q %v", data, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	missing := filepath.Join(t.TempDir(), "not-created")
	if _, err := exportImage(ctx, image, missing); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled export created output")
	}
}

func TestReadImageRejectsUnknownOrTrailingData(t *testing.T) {
	image := fixtureImage(t)
	data, err := json.Marshal(image)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string][]byte{
		"unknown":  []byte(strings.Replace(string(data), `"version":1`, `"version":1,"lost-field":true`, 1)),
		"trailing": append(append([]byte{}, data...), []byte("\n{}")...),
		"null":     []byte("null"),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "image.json")
			if err := os.WriteFile(path, value, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readImage(path); err == nil {
				t.Fatal("unsupported metadata accepted")
			}
		})
	}
}

func TestExporterSignalProcess(t *testing.T) {
	if os.Getenv("COOPR_NIX_EXPORT_HELPER") == "1" {
		os.Args = []string{"coopr-nix-export", os.Getenv("COOPR_NIX_EXPORT_IMAGE"), os.Getenv("COOPR_NIX_EXPORT_OUTPUT")}
		main()
		return
	}
	root := t.TempDir()
	path := filepath.Join(root, "large.tar")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(file.Truncate(256<<20), file.Close()); err != nil {
		t.Fatal(err)
	}
	// Cancellation is checked while streaming, before content verification.
	image := types.Image{Version: types.ImageVersion, Arch: "amd64", Layers: []types.Layer{{
		MediaType: v1.MediaTypeImageLayer, LayerPath: path, Size: 256 << 20,
		Digest: digest.FromString("not-reached").String(), DiffIDs: digest.FromString("not-reached").String(),
	}}}
	data, err := json.Marshal(image)
	if err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(root, "image.json")
	if err := os.WriteFile(metadata, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"materialized", "upstream-paths"} {
		if source == "upstream-paths" {
			image.Layers[0].LayerPath = ""
			image.Layers[0].Paths = types.Paths{{Path: path}}
			data, err := json.Marshal(image)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(metadata, data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		for _, signal := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
			t.Run(source+"/"+signal.String(), func(t *testing.T) {
				output := filepath.Join(t.TempDir(), "image")
				cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestExporterSignalProcess$")
				cmd.Env = append(os.Environ(), "COOPR_NIX_EXPORT_HELPER=1", "COOPR_NIX_EXPORT_IMAGE="+metadata, "COOPR_NIX_EXPORT_OUTPUT="+output)
				var logs bytes.Buffer
				cmd.Stdout, cmd.Stderr = &logs, &logs
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				deadline := time.Now().Add(5 * time.Second)
				for {
					files, _ := filepath.Glob(filepath.Join(output, "blobs", "sha256", ".layer-*"))
					if len(files) != 0 {
						break
					}
					select {
					case err := <-done:
						t.Fatalf("exporter exited before streaming: %v %s", err, &logs)
					default:
					}
					if time.Now().After(deadline) {
						_ = cmd.Process.Kill()
						<-done
						t.Fatal("exporter did not reach streaming")
					}
					time.Sleep(time.Millisecond)
				}
				if err := cmd.Process.Signal(signal); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("canceled exporter succeeded")
					}
				case <-time.After(5 * time.Second):
					_ = cmd.Process.Kill()
					<-done
					t.Fatal("exporter did not cancel")
				}
				if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("canceled export retained partial output: %v", err)
				}
			})
		}
	}
}

func TestEmptyImageUsesCanonicalRootFSWithoutChangingConfiguration(t *testing.T) {
	image := fixtureImage(t)
	image.Layers = nil
	output := filepath.Join(t.TempDir(), "empty")
	stats, err := exportImage(t.Context(), image, output)
	if err != nil {
		t.Fatal(err)
	}
	_, manifest := inspectOutput(t, output)
	if manifest.Layers == nil || len(manifest.Layers) != 0 || stats != (exportStats{}) {
		t.Fatalf("empty image acquired layers: %+v %+v", manifest, stats)
	}
	var actual, original map[string]any
	if err := json.Unmarshal(blobBytes(t, output, manifest.Config), &actual); err != nil {
		t.Fatal(err)
	}
	upstream, err := nix.GetConfigBlob(image)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(upstream, &original); err != nil {
		t.Fatal(err)
	}
	root := actual["rootfs"].(map[string]any)
	if root["type"] != "layers" || !reflect.DeepEqual(root["diff_ids"], []any{}) {
		t.Fatalf("invalid rootfs: %+v", root)
	}
	delete(actual, "rootfs")
	delete(original, "rootfs")
	if !reflect.DeepEqual(actual, original) {
		t.Fatal("empty-image repair changed authored configuration")
	}
}

func TestEmptyImageRejectsContradictoryRootFS(t *testing.T) {
	for _, root := range []string{`null`, `{}`, `{"type":"other","diff_ids":[]}`, `{"type":"layers","diff_ids":["sha256:bad"]}`} {
		if _, err := canonicalEmptyRootFS([]byte(`{"rootfs":` + root + `}`)); err == nil {
			t.Fatalf("accepted %s", root)
		}
	}
}
