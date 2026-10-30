package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	"go.podman.io/image/v5/pkg/compression"
	"go.podman.io/image/v5/types"
	orasoci "oras.land/oras-go/v2/content/oci"
)

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
	t.Run("concurrent merges", func(t *testing.T) {
		if err := lease.store.SetImageBigData(firstID, retainedLayerMapBigData, []byte(`{}`), nil); err != nil {
			t.Fatal(err)
		}
		ready := make(chan struct{})
		results := make(chan error, len(variants))
		for _, item := range variants {
			go func() { <-ready; results <- retainStoredLayoutLayers(lease.store, firstID, item.layout, item.root) }()
		}
		close(ready)
		for range variants {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
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
