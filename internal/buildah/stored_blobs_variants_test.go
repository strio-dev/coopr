package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	"go.podman.io/image/v5/pkg/compression"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

type retainedBlobStore struct {
	storage.Store
	writes atomic.Int64
	open   func(string, string) (io.ReadCloser, error)
}

func (store *retainedBlobStore) LayerBigData(id, key string) (io.ReadCloser, error) {
	if store.open != nil {
		return store.open(id, key)
	}
	return store.Store.LayerBigData(id, key)
}

func (store *retainedBlobStore) SetLayerBigData(id, key string, data io.Reader) error {
	store.writes.Add(1)
	return store.Store.SetLayerBigData(id, key, data)
}

type retainedBlobReader struct {
	io.Reader
	readErr, closeErr error
}

func (reader retainedBlobReader) Read(data []byte) (int, error) {
	if reader.readErr != nil {
		return 0, reader.readErr
	}
	return reader.Reader.Read(data)
}

func (reader retainedBlobReader) Close() error { return reader.closeErr }

func TestRetainStoredLayoutLayersPreservesCompressionVariants(t *testing.T) {
	if testing.Short() {
		t.Skip("requires live rootless native storage")
	}
	ctx := context.Background()
	fixture := newRawDockerFixture(t)
	var original v1.Manifest
	if err := json.Unmarshal(fixture.manifestData, &original); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	lease, err := acquireStore(StoreOptions{GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"), GraphDriverName: "vfs"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}()
	sys := &types.SystemContext{SignaturePolicyPath: writeComponentTestPolicy(t, root), BigFilesTemporaryDir: root}
	var firstManifest digest.Digest
	var firstLayer digest.Digest
	var firstID string
	type variant struct {
		layout string
		root   digest.Digest
	}
	var variants []variant
	for _, alg := range []compression.Algorithm{compression.Gzip, compression.Zstd} {
		dir := filepath.Join(root, alg.Name())
		layout, err := orasoci.NewWithContext(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		config := oci.Descriptor(v1.MediaTypeImageConfig, fixture.configData)
		if err := layout.Push(ctx, config, bytes.NewReader(fixture.configData)); err != nil {
			t.Fatal(err)
		}
		var compressed bytes.Buffer
		writer, err := compression.CompressStream(&compressed, alg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(fixture.blobs[original.Layers[0].Digest]); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		media := v1.MediaTypeImageLayerGzip
		if alg.Name() == "zstd" {
			media = v1.MediaTypeImageLayerZstd
		}
		layer := oci.Descriptor(media, compressed.Bytes())
		if err := layout.Push(ctx, layer, bytes.NewReader(compressed.Bytes())); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{layer}})
		if err != nil {
			t.Fatal(err)
		}
		member := oci.Descriptor(v1.MediaTypeImageManifest, data)
		if err := layout.Push(ctx, member, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		if err := layout.Tag(ctx, member, "variant"); err != nil {
			t.Fatal(err)
		}
		variants = append(variants, variant{dir, member.Digest})
		id, err := ImportSelectedImage(ctx, lease.store, sys, dir, member)
		if err != nil {
			t.Fatal(err)
		}
		if err := retainStoredLayoutLayers(lease.store, id, dir, member.Digest); err != nil {
			t.Fatal(err)
		}
		retained, err := retainedLayerIDs(lease.store, id)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("variant=%s id=%s manifest=%s retained=%v", alg.Name(), id, member.Digest, retained)
		if firstID == "" {
			firstID = id
			firstManifest = member.Digest
			firstLayer = layer.Digest
			exported, err := exportStoredImageVariantRaw(ctx, lease.store, id, Output{Path: filepath.Join(root, "first-before")}, sys, &firstManifest)
			if err != nil {
				t.Fatal(err)
			}
			if exported.ManifestDigest != firstManifest.String() {
				t.Fatalf("baseline export changed digest: %s != %s", exported.ManifestDigest, firstManifest)
			}
		} else if id != firstID {
			t.Fatalf("expected same config image ID %s got %s", firstID, id)
		}
	}
	retained, err := retainedLayerIDs(lease.store, firstID)
	if err != nil {
		t.Fatal(err)
	}
	if retained[firstLayer] == "" {
		t.Fatal("gzip representation missing after zstd retention")
	}
	exported, err := exportStoredImageVariantRaw(ctx, lease.store, firstID, Output{Path: filepath.Join(root, "first-after")}, sys, &firstManifest)
	if err != nil {
		t.Fatal(err)
	}
	if exported.ManifestDigest != firstManifest.String() {
		t.Fatalf("earlier exact manifest changed after second retention: got=%s want=%s", exported.ManifestDigest, firstManifest)
	}
	t.Run("repair retained bytes", func(t *testing.T) {
		payload, err := os.ReadFile(filepath.Join(variants[0].layout, "blobs", "sha256", firstLayer.Encoded()))
		if err != nil {
			t.Fatal(err)
		}
		layerID := retained[firstLayer]
		for _, fault := range []string{"missing", "corrupt", "truncated", "trailing"} {
			t.Run(fault, func(t *testing.T) {
				counted := &retainedBlobStore{Store: lease.store}
				if fault == "missing" {
					counted.open = func(string, string) (io.ReadCloser, error) { return nil, os.ErrNotExist }
				} else {
					bad := bytes.Repeat([]byte("x"), len(payload))
					switch fault {
					case "truncated":
						bad = payload[:len(payload)-1]
					case "trailing":
						bad = append(append([]byte(nil), payload...), 'x')
					}
					if err := lease.store.SetLayerBigData(layerID, firstLayer.String(), bytes.NewReader(bad)); err != nil {
						t.Fatal(err)
					}
				}
				if err := retainStoredLayoutLayers(counted, firstID, variants[0].layout, variants[0].root); err != nil || counted.writes.Load() != 1 {
					t.Fatal("retained bytes were not repaired exactly once", counted.writes.Load(), err)
				}
				stream, err := lease.store.LayerBigData(layerID, firstLayer.String())
				if err != nil {
					t.Fatal(err)
				}
				got, readErr := content.ReadAll(stream, oci.Descriptor(v1.MediaTypeImageLayerGzip, payload))
				if err := errors.Join(readErr, stream.Close()); err != nil || !bytes.Equal(got, payload) {
					t.Fatal("repair changed exact compressed bytes", err)
				}
			})
		}
		failure := errors.New("injected retained blob I/O failure")
		for _, fault := range []string{"open", "read", "close", "corrupt and close"} {
			t.Run(fault, func(t *testing.T) {
				counted := &retainedBlobStore{Store: lease.store, open: func(string, string) (io.ReadCloser, error) {
					if fault == "open" {
						return nil, failure
					}
					reader := retainedBlobReader{Reader: bytes.NewReader(payload), closeErr: failure}
					switch fault {
					case "read":
						reader.readErr, reader.closeErr = failure, nil
					case "corrupt and close":
						reader.Reader = bytes.NewReader(bytes.Repeat([]byte("x"), len(payload)))
					}
					return reader, nil
				}}
				if err := retainStoredLayoutLayers(counted, firstID, variants[0].layout, variants[0].root); !errors.Is(err, failure) || counted.writes.Load() != 0 {
					t.Fatal("retained blob I/O failure was hidden or overwritten", counted.writes.Load(), err)
				}
			})
		}
	})
	t.Run("concurrent merges", func(t *testing.T) {
		if err := lease.store.SetImageBigData(firstID, retainedLayerMapBigData, []byte(`{}`), nil); err != nil {
			t.Fatal(err)
		}
		ready := make(chan struct{})
		results := make(chan error, len(variants))
		counted := &retainedBlobStore{Store: lease.store}
		for _, item := range variants {
			go func() { <-ready; results <- retainStoredLayoutLayers(counted, firstID, item.layout, item.root) }()
		}
		close(ready)
		for range variants {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		if writes := counted.writes.Load(); writes != 0 {
			t.Fatalf("valid retained compression variants were rewritten %d times", writes)
		}
		retained, err := retainedLayerIDs(lease.store, firstID)
		if err != nil {
			t.Fatal(err)
		}
		if len(retained) != len(variants) {
			t.Fatalf("concurrent retention kept %d variants, want %d", len(retained), len(variants))
		}

		for _, item := range variants {
			selected := item.root
			exported, err := exportStoredImageVariantRaw(ctx, lease.store, firstID, Output{Path: filepath.Join(root, "concurrent-"+selected.Encoded())}, sys, &selected)
			if err != nil {
				t.Fatal(err)
			}
			if exported.ManifestDigest != selected.String() {
				t.Fatalf("concurrent selected export = %s, want %s", exported.ManifestDigest, selected)
			}
		}
	})
	t.Run("invalid existing map", func(t *testing.T) {
		for _, data := range []string{`{`, `{"not-a-digest":"layer"}`} {
			if err := lease.store.SetImageBigData(firstID, retainedLayerMapBigData, []byte(data), nil); err != nil {
				t.Fatal(err)
			}
			if err := retainStoredLayoutLayers(lease.store, firstID, variants[0].layout, variants[0].root); err == nil || !strings.Contains(err.Error(), "read exact stored layer blobs") {
				t.Fatalf("invalid retained map error = %v", err)
			}
			stored, err := lease.store.ImageBigData(firstID, retainedLayerMapBigData)
			if err != nil {
				t.Fatal(err)
			}
			if string(stored) != data {
				t.Fatal("invalid existing metadata silently overwritten")
			}
		}
	})

}
