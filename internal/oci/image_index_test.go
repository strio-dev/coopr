package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestAssembleImageIndexCopiesTwoCompleteGraphs(t *testing.T) {
	ctx := context.Background()
	amd64 := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	amdLayout, amdManifest := imageIndexTestLayout(t, ctx, amd64, "amd64", false)
	armLayout, armManifest := imageIndexTestLayout(t, ctx, arm64, "arm64", false)
	amdManifest.Annotations = map[string]string{"example.test/variant": "amd64"}

	output := filepath.Join(t.TempDir(), "multi")
	root, indexData, err := AssembleImageIndex(ctx, output, []ImageVariant{
		{Layout: amdLayout, Manifest: amdManifest, Platform: amd64},
		{Layout: armLayout, Manifest: armManifest, Platform: arm64},
	}, "oci")
	if err != nil {
		t.Fatal(err)
	}
	if root.MediaType != v1.MediaTypeImageIndex || !sameDescriptor(root, Descriptor(v1.MediaTypeImageIndex, indexData)) {
		t.Fatalf("root = %+v", root)
	}
	storedRoot, err := LayoutRoot(output)
	if err != nil || !sameDescriptor(storedRoot, root) {
		t.Fatalf("layout root = %+v, err = %v", storedRoot, err)
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
	target, err := orasoci.NewWithContext(ctx, output)
	if err != nil {
		t.Fatal(err)
	}
	for _, manifest := range index.Manifests {
		if err := verifyArchiveGraph(ctx, target, manifest); err != nil {
			t.Fatalf("copied graph %s: %v", manifest.Digest, err)
		}
	}
}

func TestAssembleImageIndexWritesDockerManifestList(t *testing.T) {
	ctx := context.Background()
	amd64 := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := v1.Platform{OS: "linux", Architecture: "arm64"}
	amdLayout, amdManifest := imageIndexTestLayout(t, ctx, amd64, "amd64", true)
	armLayout, armManifest := imageIndexTestLayout(t, ctx, arm64, "arm64", true)
	root, data, err := AssembleImageIndex(ctx, filepath.Join(t.TempDir(), "multi"), []ImageVariant{
		{Layout: amdLayout, Manifest: amdManifest, Platform: amd64},
		{Layout: armLayout, Manifest: armManifest, Platform: arm64},
	}, "docker")
	if err != nil {
		t.Fatal(err)
	}
	if root.MediaType != dockerIndexType || !bytes.Contains(data, []byte(dockerIndexType)) {
		t.Fatalf("Docker root = %+v, data = %s", root, data)
	}
}

func TestAssembleImageIndexRejectsDuplicateNormalizedPlatform(t *testing.T) {
	_, _, err := AssembleImageIndex(context.Background(), filepath.Join(t.TempDir(), "multi"), []ImageVariant{
		{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}},
		{Platform: v1.Platform{OS: "linux", Architecture: "x86_64"}},
	}, "oci")
	if err == nil || !strings.Contains(err.Error(), "duplicate image platform") {
		t.Fatalf("duplicate platform error = %v", err)
	}
}

func TestAssembleImageIndexRejectsMismatchedOrBrokenChild(t *testing.T) {
	ctx := context.Background()
	amd64 := v1.Platform{OS: "linux", Architecture: "amd64"}
	validLayout, validManifest := imageIndexTestLayout(t, ctx, amd64, "valid", false)
	_, otherManifest := imageIndexTestLayout(t, ctx, amd64, "other", false)

	t.Run("layout root", func(t *testing.T) {
		_, _, err := AssembleImageIndex(ctx, filepath.Join(t.TempDir(), "multi"), []ImageVariant{{
			Layout: validLayout, Manifest: otherManifest, Platform: amd64,
		}}, "oci")
		if err == nil || !strings.Contains(err.Error(), "layout root differs") {
			t.Fatalf("layout root mismatch error = %v", err)
		}
	})

	t.Run("config platform", func(t *testing.T) {
		_, _, err := AssembleImageIndex(ctx, filepath.Join(t.TempDir(), "multi"), []ImageVariant{{
			Layout: validLayout, Manifest: validManifest,
			Platform: v1.Platform{OS: "linux", Architecture: "arm64"},
		}}, "oci")
		if err == nil || !strings.Contains(err.Error(), "image config platform") {
			t.Fatalf("platform mismatch error = %v", err)
		}
	})

	t.Run("missing config", func(t *testing.T) {
		layout, manifest := brokenImageIndexTestLayout(t, ctx)
		_, _, err := AssembleImageIndex(ctx, filepath.Join(t.TempDir(), "multi"), []ImageVariant{{
			Layout: layout, Manifest: manifest, Platform: amd64,
		}}, "oci")
		if err == nil || !strings.Contains(err.Error(), "invalid image graph") {
			t.Fatalf("broken graph error = %v", err)
		}
	})
}

func TestRestoreImageIndexPreservesExactBytesAndRejectsChangedChild(t *testing.T) {
	ctx := context.Background()
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	layout, manifest := imageIndexTestLayout(t, ctx, platform, "restore", false)
	child := manifest
	child.Platform = platformPointer(platform)
	data := []byte(`{"schemaVersion":2,"mediaType":"` + v1.MediaTypeImageIndex + `","manifests":[` + string(mustJSON(t, child)) + `]}`)
	root := Descriptor(v1.MediaTypeImageIndex, data)
	output := filepath.Join(t.TempDir(), "restored")
	variants := []ImageVariant{{Layout: layout, Manifest: manifest, Platform: platform}}
	if err := RestoreImageIndex(ctx, output, root, data, variants); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(output, "blobs", "sha256", root.Digest.Encoded()))
	if err != nil || !bytes.Equal(stored, data) {
		t.Fatalf("stored exact index: err=%v data=%s", err, stored)
	}

	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	index.Manifests[0].Platform.Architecture = "arm64"
	changed := mustJSON(t, index)
	changedRoot := Descriptor(v1.MediaTypeImageIndex, changed)
	if err := RestoreImageIndex(ctx, filepath.Join(t.TempDir(), "changed"), changedRoot, changed, variants); err == nil || !strings.Contains(err.Error(), "does not match an exact image variant") {
		t.Fatalf("changed child error = %v", err)
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

func brokenImageIndexTestLayout(t *testing.T, ctx context.Context) (string, v1.Descriptor) {
	t.Helper()
	layout := filepath.Join(t.TempDir(), "layout")
	store, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	missing := Descriptor(v1.MediaTypeImageConfig, []byte("missing"))
	manifestData := mustJSON(t, v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest,
		Config: missing,
	})
	manifest := Descriptor(v1.MediaTypeImageManifest, manifestData)
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
