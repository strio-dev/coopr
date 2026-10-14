package oci

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"encoding/json"
	"github.com/containerd/platforms"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	nativemanifest "go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/types"
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
	if got, err := resolver.ImagePlatforms(ctx, name+":duplicate"); err != nil || !reflect.DeepEqual(got, []v1.Platform{amd64}) {
		t.Fatalf("duplicate platform err=%v", err)
	}
	badLayout, badManifest := imageIndexTestLayout(t, ctx, *unsupported.Platform, "bad", false)
	if _, err := LayoutImagePlatforms(ctx, badLayout, badManifest.Digest.String()); err == nil || !strings.Contains(err.Error(), "no runnable") {
		t.Fatalf("unsupported single manifest err=%v", err)
	}
}

func TestImagePlatformsUsesNativeIndexSelection(t *testing.T) {
	ctx := context.Background()
	for _, variant := range []string{"", "v3"} {
		t.Run("descriptor-variant-"+variant, func(t *testing.T) {
			_, repo, resolver, name := testRepository(t, registry.New())
			configPlatform := v1.Platform{OS: "linux", Architecture: "amd64"}
			selectedPlatform := configPlatform
			selectedPlatform.Variant = variant
			var members []v1.Descriptor
			for _, marker := range []string{"first", "second"} {
				child, _ := imageManifest(t, repo, configPlatform, marker)
				child.Platform = &selectedPlatform
				members = append(members, child)
			}
			root := pushIndex(t, repo, "variants", members...)
			raw, err := fetchMetadata(ctx, repo, root)
			if err != nil {
				t.Fatal(err)
			}
			list, err := nativemanifest.ListFromBlob(raw, root.MediaType)
			if err != nil {
				t.Fatal(err)
			}
			chosen, err := list.ChooseInstance(&types.SystemContext{OSChoice: "linux", ArchitectureChoice: "amd64", VariantChoice: variant})
			if err != nil {
				t.Fatal(err)
			}
			got, err := resolver.ImagePlatforms(ctx, name+":variants")
			if err != nil || !reflect.DeepEqual(got, []v1.Platform{selectedPlatform}) {
				t.Fatalf("platforms=%+v error=%v", got, err)
			}
			resolved, err := resolver.Resolve(ctx, name+":variants", selectedPlatform, Image)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Selected.Digest != chosen {
				t.Fatalf("selected %s; native selected %s", resolved.Selected.Digest, chosen)
			}
			var config v1.Image
			if err := json.Unmarshal(resolved.ConfigData, &config); err != nil {
				t.Fatal(err)
			}
			if config.Variant != "" {
				t.Fatal("rewrote image configuration")
			}
		})
	}
}
