package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestImportSelectedImageCanBeOpenedByBuildahWithPullNever(t *testing.T) {
	ctx := context.Background()
	layout := t.TempDir()
	source, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	configData, err := json.Marshal(v1.Image{
		Platform: v1.Platform{OS: "linux", Architecture: "amd64"},
		RootFS:   v1.RootFS{Type: "layers"},
		Config:   v1.ImageConfig{Env: []string{"COOPR_SELECTED=amd64"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := descriptor(v1.MediaTypeImageConfig, configData)
	if err := source.Push(ctx, config, bytes.NewReader(configData)); err != nil {
		t.Fatal(err)
	}
	manifestData, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    config,
	})
	if err != nil {
		t.Fatal(err)
	}
	selected := descriptor(v1.MediaTypeImageManifest, manifestData)
	if err := source.Push(ctx, selected, bytes.NewReader(manifestData)); err != nil {
		t.Fatal(err)
	}
	other := selected
	other.Digest = digest.FromString("unavailable-arm64-manifest")
	indexData, err := json.Marshal(v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{
			{MediaType: other.MediaType, Digest: other.Digest, Size: other.Size, Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}},
			{MediaType: selected.MediaType, Digest: selected.Digest, Size: selected.Size, Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	indexRoot := descriptor(v1.MediaTypeImageIndex, indexData)
	if err := source.Push(ctx, indexRoot, bytes.NewReader(indexData)); err != nil {
		t.Fatal(err)
	}
	if err := source.Tag(ctx, indexRoot, indexRoot.Digest.String()); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	store, err := storage.GetStore(storage.StoreOptions{
		GraphDriverName: "vfs",
		GraphRoot:       filepath.Join(root, "graph"),
		RunRoot:         filepath.Join(root, "run"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.Shutdown(true); err != nil {
			t.Errorf("shutdown test store: %v", err)
		}
	})
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root}
	imageID, err := ImportSelectedImage(ctx, store, system, layout, selected)
	if err != nil {
		t.Fatal(err)
	}
	if imageID != config.Digest.Encoded() {
		t.Fatalf("image ID = %q, want %q", imageID, config.Digest.Encoded())
	}
	stored, err := store.Image(imageID)
	if err != nil || stored.ID != imageID {
		t.Fatalf("stored image = %+v, %v", stored, err)
	}
	if testing.Short() {
		return
	}
	builder, err := upstream.NewBuilder(ctx, store, upstream.BuilderOptions{
		FromImage:     imageID,
		PullPolicy:    define.PullNever,
		Isolation:     define.IsolationChroot,
		Format:        define.OCIv1ImageManifest,
		SystemContext: system,
	})
	if err != nil {
		t.Fatal(err)
	}
	if builder.FromImageID != imageID {
		t.Fatalf("Buildah base image ID = %q, want %q", builder.FromImageID, imageID)
	}
	if err := builder.Delete(); err != nil {
		t.Fatal(err)
	}
}

func TestImportSelectedImageRejectsBlobThatChanged(t *testing.T) {
	data := []byte(`{"schemaVersion":2}`)
	selected := descriptor(v1.MediaTypeImageManifest, data)
	layout := t.TempDir()
	path := filepath.Join(layout, "blobs", selected.Digest.Algorithm().String(), selected.Digest.Encoded())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store, err := storage.GetStore(storage.StoreOptions{GraphDriverName: "vfs", GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = store.Shutdown(true) })
	if _, err := ImportSelectedImage(context.Background(), store, nil, layout, selected); err == nil {
		t.Fatal("ImportSelectedImage accepted changed manifest content")
	}
}

func TestReadVerifiedLayoutBlobRejectsOversizedMetadataBeforeRead(t *testing.T) {
	descriptor := v1.Descriptor{
		MediaType: v1.MediaTypeImageManifest,
		Digest:    digest.FromString("not-present"),
		Size:      maxImageMetadataSize + 1,
	}
	_, err := readVerifiedLayoutBlob(t.TempDir(), descriptor)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized metadata error = %v, want size limit", err)
	}
}

func descriptor(mediaType string, data []byte) v1.Descriptor {
	return v1.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
}
