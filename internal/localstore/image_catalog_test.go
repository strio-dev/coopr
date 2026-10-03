package localstore

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestCopySelectedImageExcludesOtherPlatforms(t *testing.T) {
	ctx := context.Background()
	source, err := orasoci.NewWithContext(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	amd := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm := v1.Platform{OS: "linux", Architecture: "arm64"}
	amdManifest := catalogTestImage(t, ctx, source, amd, "amd")
	armManifest := catalogTestImage(t, ctx, source, arm, "arm")
	amdChild, armChild := amdManifest, armManifest
	amdChild.Platform, armChild.Platform = &amd, &arm
	root := catalogTestIndex(t, ctx, source, amdChild, armChild)
	dir := filepath.Join(t.TempDir(), "image")
	if err := CopySelectedImage(ctx, dir, source, root, amdManifest); err != nil {
		t.Fatal(err)
	}
	target, err := orasoci.NewWithContext(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		descriptor v1.Descriptor
		want       bool
	}{
		{root, true}, {amdManifest, true}, {armManifest, false},
	} {
		exists, err := target.Exists(ctx, item.descriptor)
		if err != nil || exists != item.want {
			t.Fatalf("descriptor %s exists = %t, err %v; want %t", item.descriptor.Digest, exists, err, item.want)
		}
	}
}

func catalogTestImage(t *testing.T, ctx context.Context, store *orasoci.Store, platform v1.Platform, payload string) v1.Descriptor {
	t.Helper()
	configData, err := json.Marshal(v1.Image{Platform: platform, RootFS: v1.RootFS{Type: "layers"}, Config: v1.ImageConfig{Env: []string{"PAYLOAD=" + payload}}})
	if err != nil {
		t.Fatal(err)
	}
	config := testDescriptor(v1.MediaTypeImageConfig, configData)
	layerData := []byte(payload)
	layer := testDescriptor(v1.MediaTypeImageLayer, layerData)
	for _, item := range []struct {
		desc v1.Descriptor
		data []byte
	}{{config, configData}, {layer, layerData}} {
		if err := store.Push(ctx, item.desc, bytes.NewReader(item.data)); err != nil {
			t.Fatal(err)
		}
	}
	manifestData, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    config,
		Layers:    []v1.Descriptor{layer},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := testDescriptor(v1.MediaTypeImageManifest, manifestData)
	if err := store.Push(ctx, manifest, bytes.NewReader(manifestData)); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func catalogTestIndex(t *testing.T, ctx context.Context, store *orasoci.Store, manifests ...v1.Descriptor) v1.Descriptor {
	t.Helper()
	data, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: manifests})
	if err != nil {
		t.Fatal(err)
	}
	root := v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: digest.FromBytes(data), Size: int64(len(data))}
	if err := store.Push(ctx, root, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	return root
}
