package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestResolveLayoutImageSelectsRequestedPlatformByTagAndDigest(t *testing.T) {
	ctx := context.Background()
	layout := filepath.Join(t.TempDir(), "layout")
	store, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	amd64 := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := v1.Platform{OS: "linux", Architecture: "arm64"}
	amdManifest, _ := layoutTestImage(t, ctx, store, amd64, "amd64")
	armManifest, armConfig := layoutTestImage(t, ctx, store, arm64, "arm64")
	amdManifest.Platform, armManifest.Platform = &amd64, &arm64
	indexData, err := json.Marshal(v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{amdManifest, armManifest},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := Descriptor(v1.MediaTypeImageIndex, indexData)
	if err := store.Push(ctx, root, bytes.NewReader(indexData)); err != nil {
		t.Fatal(err)
	}
	if err := store.Tag(ctx, root, "tools"); err != nil {
		t.Fatal(err)
	}

	for _, selector := range []string{"tools", root.Digest.String()} {
		resolved, err := ResolveLayoutImage(ctx, layout, selector, arm64)
		if err != nil {
			t.Fatalf("resolve %q: %v", selector, err)
		}
		if resolved.Root.Digest != root.Digest || resolved.Selected.Digest != armManifest.Digest || resolved.Config.Digest != armConfig.Digest {
			t.Fatalf("resolve %q selected %+v", selector, resolved)
		}
		if resolved.Source() == nil || resolved.Reference == "" {
			t.Fatalf("resolve %q discarded source or reference", selector)
		}
	}
	if _, err := ResolveLayoutImage(ctx, layout, "tools", v1.Platform{OS: "linux", Architecture: "riscv64"}); err == nil {
		t.Fatal("ResolveLayoutImage accepted a platform absent from the index")
	}
}

func TestResolveLayoutImageRejectsMissingOrNonDirectoryLayout(t *testing.T) {
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	if _, err := ResolveLayoutImage(context.Background(), filepath.Join(t.TempDir(), "missing"), "latest", platform); err == nil {
		t.Fatal("ResolveLayoutImage created a missing layout")
	}
	file := filepath.Join(t.TempDir(), "layout")
	if err := os.WriteFile(file, []byte("not a layout"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveLayoutImage(context.Background(), file, "latest", platform); err == nil {
		t.Fatal("ResolveLayoutImage accepted a regular file")
	}
}

func layoutTestImage(t *testing.T, ctx context.Context, store *orasoci.Store, platform v1.Platform, marker string) (v1.Descriptor, v1.Descriptor) {
	t.Helper()
	configData, err := json.Marshal(v1.Image{
		Platform: platform, RootFS: v1.RootFS{Type: "layers"},
		Config: v1.ImageConfig{Env: []string{"CONTEXT=" + marker}},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := Descriptor(v1.MediaTypeImageConfig, configData)
	if err := store.Push(ctx, config, bytes.NewReader(configData)); err != nil {
		t.Fatal(err)
	}
	manifestData, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest,
		Config: config,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := Descriptor(v1.MediaTypeImageManifest, manifestData)
	if err := store.Push(ctx, manifest, bytes.NewReader(manifestData)); err != nil {
		t.Fatal(err)
	}
	return manifest, config
}

var _ content.ReadOnlyStorage = (*orasoci.Store)(nil)
