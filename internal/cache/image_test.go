package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"oras.land/oras-go/v2/content"

	"github.com/strio-dev/coopr/internal/oci"

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
	fixture, key, descriptors, _ := imageLookupFixture(t, time.Now())
	_, sourcePath, err := fixture.LookupImage(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sourcePath) })
	manifest, config, layer := descriptors[0], descriptors[1], descriptors[2]
	configData, err := os.ReadFile(filepath.Join(sourcePath, "blobs", "sha256", config.Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	store, err := NewLocalStore(ctx, filepath.Join(staging, "cache"), staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := orasoci.New(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	baseManifest := oci.VersionedManifest(config, []v1.Descriptor{layer}, "")
	baseManifest.Annotations = map[string]string{"test": "base-image"}
	base := pushGraphTestJSON(t, source, v1.MediaTypeImageManifest, baseManifest)
	record := ImageRecord{Key: key, Image: manifest, BaseImage: &base, RootMetadata: json.RawMessage(`{"mode":493}`)}
	root, err := store.PutImage(ctx, key, record, sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	data, err := fetchVerified(ctx, store.target, root, maxImageRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if root.MediaType != v1.MediaTypeImageIndex || index.ArtifactType != ImageArtifactType || len(index.Manifests) != 3 || !content.Equal(index.Manifests[1], manifest) || !content.Equal(index.Manifests[2], base) {
		t.Fatalf("cache index does not retain exact image/base members: %+v", index)
	}
	data, err = fetchVerified(ctx, store.target, index.Manifests[0], maxImageRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	var metadata v1.Manifest
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.ArtifactType != ImageArtifactType || metadata.Config.MediaType != ImageConfigMediaType || metadata.Layers == nil || len(metadata.Layers) != 0 {
		t.Fatalf("metadata artifact does not contain only its record config: %+v", metadata)
	}
	var captured ImageRecord
	data, err = fetchVerified(ctx, store.target, metadata.Config, maxImageRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &captured); err != nil {
		t.Fatal(err)
	}
	if captured.Version != ImageRecordVersion || !content.Equal(captured.Image, manifest) || captured.BaseImage == nil || !content.Equal(*captured.BaseImage, base) || !bytes.Equal(captured.RootMetadata, record.RootMetadata) {
		t.Fatalf("metadata record changed: %+v", captured)
	}
	got, layout, err := store.LookupImage(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(layout) }()
	if !content.Equal(got.Image, manifest) || got.BaseImage == nil || !content.Equal(*got.BaseImage, base) || !bytes.Equal(got.RootMetadata, record.RootMetadata) {
		t.Fatalf("record = %+v, want %+v", got, record)
	}
	restored, err := orasoci.New(layout)
	if err != nil {
		t.Fatal(err)
	}
	for _, desc := range []v1.Descriptor{manifest, config, layer, base} {
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

//nolint:staticcheck // Existing nondistributable image and base layers must survive cache round trips.
func TestInstructionImageStoreRetainsNondistributableImageAndBase(t *testing.T) {
	ctx := t.Context()
	fixture, sourcePath, foreignLayers := graphPublicationForeignFixture(t, v1.MediaTypeImageLayerNonDistributableGzip, false)
	image, base := fixture.Results[1].Image, fixture.Results[0].Image
	source, err := orasoci.New(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	want := map[digest.Digest][]byte{}
	payloads := map[digest.Digest]v1.Descriptor{}
	for _, root := range []v1.Descriptor{image, base} {
		descriptors, err := imageGraphDescriptors(ctx, source, root)
		if err != nil {
			t.Fatal(err)
		}
		for _, descriptor := range descriptors {
			data, err := fetchVerified(ctx, source, descriptor, maxImageRecordBytes)
			if err != nil {
				t.Fatal(err)
			}
			payloads[descriptor.Digest], want[descriptor.Digest] = descriptor, data
		}
	}
	cacheRoot := t.TempDir()
	store, err := NewLocalStore(ctx, cacheRoot, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	key := ImageKey{Instruction: digest.FromString("foreign-image"), Parent: digest.FromString("parent"), Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Executor: "test", Format: "oci"}
	record := ImageRecord{Key: key, Image: image, BaseImage: &base, CreatedAt: time.Now(), RootMetadata: json.RawMessage(`{"mode":493}`)}
	if _, err := store.PutImage(ctx, key, record, sourcePath); err != nil {
		t.Fatal(err)
	}
	// A new reader can restore the complete closure after the producer's
	// source disappears; neither preexisting payloads nor its open store help.
	if err := os.RemoveAll(sourcePath); err != nil {
		t.Fatal(err)
	}
	target, err := orasoci.New(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	counted := &countedCacheTarget{Target: target, fetches: map[digest.Digest]int{}}
	reader := &OCIStore{target: counted, stagingDir: t.TempDir()}
	got, layout, err := reader.LookupImage(ctx, key)
	if err != nil {
		t.Fatal("successful publication did not restore the foreign image and base", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(layout) })
	if !content.Equal(got.Image, image) || got.BaseImage == nil || !content.Equal(*got.BaseImage, base) || !bytes.Equal(got.RootMetadata, record.RootMetadata) {
		t.Fatalf("restored record changed: %+v", got)
	}
	restored, err := orasoci.New(layout)
	if err != nil {
		t.Fatal(err)
	}
	for _, descriptor := range []v1.Descriptor{image, base} {
		if _, err := fetchVerified(ctx, restored, descriptor, maxImageRecordBytes); err != nil {
			t.Fatal("restored image manifest is missing", descriptor.Digest, err)
		}
	}
	for _, descriptor := range payloads {
		data, err := fetchVerified(ctx, restored, descriptor, maxImageRecordBytes)
		if err != nil || !bytes.Equal(data, want[descriptor.Digest]) {
			t.Fatal("restored image payload differs", descriptor.Digest, err)
		}
		if counted.fetches[descriptor.Digest] != 1 {
			t.Errorf("image/base payload %s fetched %d times; want once", descriptor.Digest, counted.fetches[descriptor.Digest])
		}
	}
	for _, descriptor := range foreignLayers {
		if _, err := fetchVerified(ctx, target, descriptor, maxImageRecordBytes); err != nil {
			t.Fatal("cache publication omitted a nondistributable payload", descriptor.Digest, err)
		}
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
