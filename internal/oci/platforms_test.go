package oci

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/containerd/platforms"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestImagePlatformsReadsRegistryAndLayoutMetadata(t *testing.T) {
	ctx := context.Background()
	amd64 := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := v1.Platform{OS: "linux", Architecture: "arm64"}
	amdLayout, amdManifest := imageIndexTestLayout(t, ctx, amd64, "amd", false)
	armLayout, armManifest := imageIndexTestLayout(t, ctx, arm64, "arm", false)
	indexLayout := filepath.Join(t.TempDir(), "index")
	root, _, err := AssembleImageIndex(ctx, indexLayout, []ImageVariant{{Layout: armLayout, Manifest: armManifest, Platform: arm64}, {Layout: amdLayout, Manifest: amdManifest, Platform: amd64}}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _, resolver, name := testRepository(t, registry.New())
	if _, err := resolver.PublishLayout(ctx, name+":multi", indexLayout, root); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func() ([]v1.Platform, error){
		func() ([]v1.Platform, error) { return resolver.ImagePlatforms(ctx, name+":multi") },
		func() ([]v1.Platform, error) { return LayoutImagePlatforms(ctx, indexLayout, root.Digest.String()) },
	} {
		got, err := read()
		if err != nil || !reflect.DeepEqual(got, []v1.Platform{amd64, arm64}) {
			t.Fatalf("platforms=%+v err=%v", got, err)
		}
	}
	got, err := LayoutImagePlatforms(ctx, armLayout, armManifest.Digest.String())
	if err != nil || len(got) != 1 || platforms.Format(got[0]) != "linux/arm64" {
		t.Fatalf("single foreign platform=%+v err=%v", got, err)
	}
}

func TestImagePlatformsSkipsAuxiliaryAndUnsupportedArchitectures(t *testing.T) {
	ctx := context.Background()
	_, repo, resolver, name := testRepository(t, registry.New())
	amd64 := v1.Platform{OS: "linux", Architecture: "amd64"}
	layout, manifest := imageIndexTestLayout(t, ctx, amd64, "runnable", false)
	if _, err := resolver.PublishLayout(ctx, name+":child", layout, manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Platform = &amd64
	aux := manifest
	aux.Platform = &v1.Platform{OS: "unknown", Architecture: "unknown"}
	unsupported := manifest
	unsupported.Platform = &v1.Platform{OS: "linux", Architecture: "imaginaryarch"}
	pushIndex(t, repo, "mixed", manifest, aux, unsupported)
	got, err := resolver.ImagePlatforms(ctx, name+":mixed")
	if err != nil || !reflect.DeepEqual(got, []v1.Platform{amd64}) {
		t.Fatalf("filtered platforms=%+v err=%v", got, err)
	}
	duplicate := manifest
	duplicate.Platform = &v1.Platform{OS: "linux", Architecture: "x86_64"}
	pushIndex(t, repo, "duplicate", manifest, duplicate)
	if _, err := resolver.ImagePlatforms(ctx, name+":duplicate"); err == nil || !strings.Contains(err.Error(), "duplicate platform") {
		t.Fatalf("duplicate platform err=%v", err)
	}
	badLayout, badManifest := imageIndexTestLayout(t, ctx, *unsupported.Platform, "bad", false)
	if _, err := LayoutImagePlatforms(ctx, badLayout, badManifest.Digest.String()); err == nil || !strings.Contains(err.Error(), "no runnable") {
		t.Fatalf("unsupported single manifest err=%v", err)
	}
}
