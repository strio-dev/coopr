package build

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
)

func TestAllPlatformsDiscoversPartiallyCachedRegistryIndexOffline(t *testing.T) {
	ctx := context.Background()
	store := nativeBuildTestStore(t.TempDir())
	const reference = "registry.invalid/team/base:latest"
	variants := make([]oci.ImageVariant, 0, 3)
	configs, manifests := map[string][]byte{}, map[string][]byte{}
	for _, arch := range []string{"amd64", "arm64", "ppc64le"} {
		platform := v1.Platform{OS: "linux", Architecture: arch}
		config, err := json.Marshal(v1.Image{Platform: platform, RootFS: v1.RootFS{Type: "layers"}})
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: oci.Descriptor(v1.MediaTypeImageConfig, config), Layers: []v1.Descriptor{}})
		if err != nil {
			t.Fatal(err)
		}
		configs[arch], manifests[arch] = config, manifest
		variants = append(variants, oci.ImageVariant{Manifest: oci.Descriptor(v1.MediaTypeImageManifest, manifest), Platform: platform})
	}
	root, data, err := oci.ImageIndexDescriptor(variants, "oci")
	if err != nil {
		t.Fatal(err)
	}
	err = buildah.WithStore(store, func(backend storage.Store) error {
		// Two pulled platforms retain the original three-platform registry
		// index in native metadata. The third child is deliberately absent.
		for _, variant := range variants[:2] {
			config := configs[variant.Platform.Architecture]
			manifest := manifests[variant.Platform.Architecture]
			id := digest.FromBytes(config).Encoded()
			name := "registry.invalid/team/base@" + variant.Manifest.Digest.String()
			if _, err := backend.CreateImage(id, []string{name}, "", "", nil); err != nil {
				return err
			}
			for key, bytes := range map[string][]byte{digest.FromBytes(config).String(): config, storage.ImageDigestManifestBigDataNamePrefix: manifest, storage.ImageDigestManifestBigDataNamePrefix + "-" + variant.Manifest.Digest.String(): manifest, storage.ImageDigestManifestBigDataNamePrefix + "-" + root.Digest.String(): data} {
				if err := backend.SetImageBigData(id, key, bytes, func(bytes []byte) (digest.Digest, error) { return digest.FromBytes(bytes), nil }); err != nil {
					return err
				}
			}
			if err := backend.AddNames(id, []string{reference}); err != nil {
				return err
			}
			if err := oci.RecordStoredOrigin(ctx, backend, reference, oci.StoredSelection{Root: root, Manifest: variant.Manifest, ImageID: id, ConfigData: config}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	def, err := definition.Parse(strings.NewReader("from \"" + reference + "\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, resident, found, err := testStoredImageIndex(ctx, store, reference)
	if err != nil || !found {
		t.Fatalf("partial native index: found=%t err=%v", found, err)
	}
	if _, _, _, err := appendManifest(ctx, reference, "oci", store, resident); err == nil || !strings.Contains(err.Error(), "unavailable instances") {
		t.Fatalf("append incomplete native list: %v", err)
	}
	after, afterData, _, found, err := testStoredImageIndex(ctx, store, reference)
	if err != nil || !found || after.Digest != root.Digest || string(afterData) != string(data) {
		t.Fatalf("failed append changed partial index: %s, %t, %v", after.Digest, found, err)
	}
	got, err := discoverBuildPlatforms(ctx, def, planner.Options{Mode: planner.Build}, Options{PullPolicy: "never", BuildStore: store}, "")
	if err != nil || !slices.Equal(got, []string{"linux/amd64", "linux/arm64"}) {
		t.Fatalf("offline resident index platforms = %v, %v", got, err)
	}
}
