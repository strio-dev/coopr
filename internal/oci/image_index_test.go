package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestImageIndexDescriptorPreservesOrderedPlatformMetadata(t *testing.T) {
	ctx := context.Background()
	amd64 := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	_, amdManifest := imageIndexTestLayout(t, ctx, amd64, "amd64", false)
	_, armManifest := imageIndexTestLayout(t, ctx, arm64, "arm64", false)
	amdManifest.Annotations = map[string]string{"example.test/variant": "amd64"}

	root, indexData, err := ImageIndexDescriptor([]ImageVariant{
		{Manifest: amdManifest, Platform: amd64},
		{Manifest: armManifest, Platform: arm64},
	}, "oci")
	if err != nil {
		t.Fatal(err)
	}
	if root.MediaType != v1.MediaTypeImageIndex || !content.Equal(root, Descriptor(v1.MediaTypeImageIndex, indexData)) {
		t.Fatalf("root = %+v", root)
	}
	var index v1.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		t.Fatal(err)
	}
	wantAMD := amdManifest
	wantAMD.Platform = platformPointer(amd64)
	wantARM := armManifest
	wantARM.Platform = platformPointer(normalizeImagePlatform(arm64))
	if len(index.Manifests) != 2 || !reflect.DeepEqual(index.Manifests[0], wantAMD) || !reflect.DeepEqual(index.Manifests[1], wantARM) {
		t.Fatalf("index manifests = %+v", index.Manifests)
	}
}

func TestImageIndexDescriptorWritesDockerManifestList(t *testing.T) {
	ctx := context.Background()
	amd64 := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := v1.Platform{OS: "linux", Architecture: "arm64"}
	_, amdManifest := imageIndexTestLayout(t, ctx, amd64, "amd64", true)
	_, armManifest := imageIndexTestLayout(t, ctx, arm64, "arm64", true)
	root, data, err := ImageIndexDescriptor([]ImageVariant{
		{Manifest: amdManifest, Platform: amd64},
		{Manifest: armManifest, Platform: arm64},
	}, "docker")
	if err != nil {
		t.Fatal(err)
	}
	if root.MediaType != dockerIndexType || !bytes.Contains(data, []byte(dockerIndexType)) {
		t.Fatalf("Docker root = %+v, data = %s", root, data)
	}
}

func TestImageIndexDescriptorRejectsDuplicateNormalizedPlatform(t *testing.T) {
	_, _, err := ImageIndexDescriptor([]ImageVariant{
		{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}},
		{Platform: v1.Platform{OS: "linux", Architecture: "x86_64"}},
	}, "oci")
	if err == nil || !strings.Contains(err.Error(), "duplicate image platform") {
		t.Fatalf("duplicate platform error = %v", err)
	}
}

func imageIndexTestLayout(t *testing.T, ctx context.Context, platform v1.Platform, marker string, docker bool) (string, v1.Descriptor) {
	t.Helper()
	layout := filepath.Join(t.TempDir(), "layout")
	store, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	layerData := []byte("layer-" + marker)
	layer := Descriptor(v1.MediaTypeImageLayer, layerData)
	if err := store.Push(ctx, layer, bytes.NewReader(layerData)); err != nil {
		t.Fatal(err)
	}
	configType := v1.MediaTypeImageConfig
	manifestType := v1.MediaTypeImageManifest
	if docker {
		configType = dockerConfigType
		manifestType = dockerManifestType
	}
	configData := mustJSON(t, v1.Image{Platform: platform, RootFS: v1.RootFS{Type: "layers"}})
	config := Descriptor(configType, configData)
	if err := store.Push(ctx, config, bytes.NewReader(configData)); err != nil {
		t.Fatal(err)
	}
	manifestData := mustJSON(t, v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: manifestType,
		Config: config, Layers: []v1.Descriptor{layer},
	})
	manifest := Descriptor(manifestType, manifestData)
	if err := store.Push(ctx, manifest, bytes.NewReader(manifestData)); err != nil {
		t.Fatal(err)
	}
	return layout, manifest
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

var _ content.ReadOnlyStorage = (*orasoci.Store)(nil)

func TestPlatformKeyUsesNativeFeatureSet(t *testing.T) {
	plain := v1.Platform{OS: "linux", Architecture: "amd64", OSFeatures: []string{"x"}}
	repeated := plain
	repeated.OSFeatures = []string{"x", "", "x"}
	if platformKey(plain) != platformKey(repeated) {
		t.Fatal("duplicate and empty feature entries must use the native platform identity")
	}
}
