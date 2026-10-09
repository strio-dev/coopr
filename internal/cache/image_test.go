package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestInstructionImageKeyInvalidatesEveryInput(t *testing.T) {
	base := ImageKey{
		Instruction: digest.FromString("instruction"), Parent: digest.FromString("parent"),
		Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Executor: "buildah-test", Format: "oci",
	}
	want, err := base.Digest()
	if err != nil {
		t.Fatal(err)
	}
	changes := []func(*ImageKey){
		func(key *ImageKey) { key.Instruction = digest.FromString("other instruction") },
		func(key *ImageKey) { key.Parent = digest.FromString("other parent") },
		func(key *ImageKey) { key.Platform.Architecture = "arm64" },
		func(key *ImageKey) { key.Executor = "other-executor" },
		func(key *ImageKey) { key.Format = "docker" },
	}
	for index, change := range changes {
		candidate := base
		change(&candidate)
		got, err := candidate.Digest()
		if err != nil || got == want {
			t.Fatalf("change %d did not invalidate image key: digest=%s err=%v", index, got, err)
		}
	}
}

func TestInstructionImageStorePreservesExactGraph(t *testing.T) {
	ctx := context.Background()
	staging := t.TempDir()
	sourcePath := filepath.Join(staging, "source")
	source, err := orasoci.New(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	layerData := []byte("exact layer")
	layer := oci.Descriptor(v1.MediaTypeImageLayer, layerData)
	layer.Annotations = map[string]string{"io.github.containers.zstd-chunked.manifest-checksum": digest.FromString("chunk table").String()}
	configData, _ := json.Marshal(v1.Image{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{layer.Digest}}})
	config := oci.Descriptor(v1.MediaTypeImageConfig, configData)
	manifestData, _ := json.Marshal(oci.VersionedManifest(config, []v1.Descriptor{layer}, ""))
	manifest := oci.Descriptor(v1.MediaTypeImageManifest, manifestData)
	for _, item := range []struct {
		desc v1.Descriptor
		data []byte
	}{{layer, layerData}, {config, configData}, {manifest, manifestData}} {
		if err := source.Push(ctx, item.desc, bytes.NewReader(item.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.Tag(ctx, manifest, manifest.Digest.String()); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(sourcePath, "blobs", "sha256", config.Digest.Encoded())); err != nil || len(raw) != len(configData) {
		t.Fatalf("stored config bytes = %d, %v; want %d", len(raw), err, len(configData))
	}
	store, err := NewLocalStore(ctx, filepath.Join(staging, "cache"), staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := ImageKey{Instruction: digest.FromString("instruction"), Parent: digest.FromString("parent"), Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Executor: "buildah-test", Format: "oci"}
	record := ImageRecord{Key: key, Image: manifest, RootMetadata: json.RawMessage(`{"mode":493}`)}
	if _, err := store.PutImage(ctx, key, record, sourcePath); err != nil {
		t.Fatal(err)
	}
	got, layout, err := store.LookupImage(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(layout) }()
	if !sameDescriptor(got.Image, manifest) || !bytes.Equal(got.RootMetadata, record.RootMetadata) {
		t.Fatalf("record = %+v, want %+v", got, record)
	}
	restored, err := orasoci.New(layout)
	if err != nil {
		t.Fatal(err)
	}
	for _, desc := range []v1.Descriptor{manifest, config, layer} {
		stream, err := restored.Fetch(ctx, desc)
		if err != nil {
			t.Fatalf("restored graph lacks %s: %v", desc.Digest, err)
		}
		_ = stream.Close()
	}
	wrong := key
	wrong.Parent = digest.FromString("different-parent")
	if _, _, err := store.LookupImage(ctx, wrong); err != ErrMiss {
		t.Fatalf("different key lookup error = %v, want cache miss", err)
	}
	// Existing destination blobs do not bypass source integrity verification.
	configPath := filepath.Join(sourcePath, "blobs", "sha256", config.Digest.Encoded())
	if err := os.Chmod(configPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, bytes.Repeat([]byte("x"), len(configData)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutImage(ctx, key, record, sourcePath); err == nil {
		t.Fatal("existing cache destination hid corrupt source config")
	}
	if err := os.WriteFile(configPath, configData, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(sourcePath, "blobs", "sha256", layer.Digest.Encoded())); err != nil {
		t.Fatal(err)
	}
	broken, err := NewLocalStore(ctx, filepath.Join(staging, "broken-cache"), staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broken.PutImage(ctx, key, record, sourcePath); err == nil {
		t.Fatal("PutImage accepted an incomplete image graph")
	}
}

func TestInstructionImageLayerAnnotationsKeepLocalBlobRestrictions(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name      string
		change    func(*v1.Descriptor)
		wantError bool
	}{
		{name: "compression annotations", change: func(*v1.Descriptor) {}},
		{name: "external URLs", change: func(d *v1.Descriptor) { d.URLs = []string{"https://example.invalid/layer"} }, wantError: true},
		{name: "inline data", change: func(d *v1.Descriptor) { d.Data = []byte("inline") }, wantError: true},
		{name: "platform", change: func(d *v1.Descriptor) { d.Platform = &v1.Platform{OS: "linux", Architecture: "amd64"} }, wantError: true},
		{name: "artifact type", change: func(d *v1.Descriptor) { d.ArtifactType = "application/example" }, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			target, err := orasoci.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			layer := oci.Descriptor(v1.MediaTypeImageLayerZstd, []byte("layer"))
			layer.Annotations = map[string]string{"io.github.containers.zstd-chunked.manifest-position": "0:1:1:1"}
			test.change(&layer)
			config := oci.Descriptor(v1.MediaTypeImageConfig, []byte("{}"))
			data, err := json.Marshal(oci.VersionedManifest(config, []v1.Descriptor{layer}, ""))
			if err != nil {
				t.Fatal(err)
			}
			root := oci.Descriptor(v1.MediaTypeImageManifest, data)
			if err := target.Push(ctx, root, bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}
			descriptors, err := imageGraphDescriptors(ctx, target, root)
			if (err != nil) != test.wantError {
				t.Fatalf("graph validation error=%v wantError=%v", err, test.wantError)
			}
			if !test.wantError && descriptors[1].Annotations["io.github.containers.zstd-chunked.manifest-position"] != "0:1:1:1" {
				t.Fatal("layer annotations dropped")
			}
		})
	}
}
