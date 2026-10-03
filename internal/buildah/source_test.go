package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"coopr/internal/imagecatalog"
	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestResolveImageSourceReusesCatalogSelectionFromBuildahStore(t *testing.T) {
	ctx := context.Background()
	sourceDir := t.TempDir()
	source, err := orasoci.NewWithContext(ctx, sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	amd64 := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := v1.Platform{OS: "linux", Architecture: "arm64"}
	amdManifest, amdConfig := sourceTestImage(t, ctx, source, amd64, "amd64")
	armManifest, armConfig := sourceTestImage(t, ctx, source, arm64, "arm64")
	amdChild, armChild := amdManifest, armManifest
	amdChild.Platform, armChild.Platform = &amd64, &arm64
	root := sourceTestIndex(t, ctx, source, amdChild, armChild)

	imageStoreDir := filepath.Join(t.TempDir(), "images")
	canonicalRef := "registry.example/team/base:latest"
	resolver, err := oci.NewResolver(oci.Options{ImageStoreDir: imageStoreDir})
	if err != nil {
		t.Fatal(err)
	}
	privateRoot := t.TempDir()
	privateStore, err := storage.GetStore(storage.StoreOptions{
		GraphDriverName: "vfs",
		GraphRoot:       filepath.Join(privateRoot, "graph"),
		RunRoot:         filepath.Join(privateRoot, "run"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := privateStore.Shutdown(true); err != nil {
			t.Errorf("shutdown private store: %v", err)
		}
	})
	policy := filepath.Join(privateRoot, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: privateRoot}
	imageID, err := ImportSelectedImage(ctx, privateStore, system, sourceDir, amdManifest)
	if err != nil {
		t.Fatal(err)
	}
	configData, err := content.FetchAll(ctx, source, amdConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := imagecatalog.Commit(ctx, imageStoreDir, canonicalRef, amd64, imagecatalog.Selection{
		Root: root, Manifest: amdManifest, ImageID: imageID, ConfigData: configData,
	}); err != nil {
		t.Fatal(err)
	}
	if image, err := privateStore.Image(armConfig.Digest.Encoded()); err == nil {
		t.Fatalf("unselected arm64 image exists in storage: %+v", image)
	}
	if _, found, err := imagecatalog.Lookup(ctx, imageStoreDir, canonicalRef, arm64); err != nil || found {
		t.Fatalf("unselected arm64 catalog lookup found=%v, err=%v", found, err)
	}

	resolvedSource, err := ResolveImageSource(ctx, resolver, canonicalRef, amd64, privateStore, system)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedSource.Selected.Digest != amdManifest.Digest || resolvedSource.Selected.MediaType != amdManifest.MediaType || resolvedSource.Selected.Size != amdManifest.Size {
		t.Fatalf("selected descriptor = %+v, want %+v", resolvedSource.Selected, amdManifest)
	}
	if resolvedSource.ImageID != amdConfig.Digest.Encoded() {
		t.Fatalf("image ID = %q, want %q", resolvedSource.ImageID, amdConfig.Digest.Encoded())
	}
	if len(resolvedSource.ConfigData) == 0 {
		t.Fatal("resolved OCI config bytes were discarded")
	}
	stored, err := privateStore.Image(resolvedSource.ImageID)
	if err != nil || stored.ID != resolvedSource.ImageID {
		t.Fatalf("stored image = %+v, %v", stored, err)
	}
	selection, found, err := imagecatalog.Lookup(ctx, imageStoreDir, canonicalRef, amd64)
	if err != nil || !found || selection.Root.Digest != root.Digest || selection.Manifest.Digest != amdManifest.Digest || selection.ImageID != amdConfig.Digest.Encoded() {
		t.Fatalf("source catalog lookup = (%+v, %v, %v)", selection, found, err)
	}
	tampered := amdManifest
	tampered.Digest = digest.FromString("different-manifest")
	if err := imagecatalog.Commit(ctx, imageStoreDir, canonicalRef, amd64, imagecatalog.Selection{
		Root: root, Manifest: tampered, ImageID: imageID, ConfigData: configData,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveImageSource(ctx, resolver, canonicalRef, amd64, privateStore, system); err == nil {
		t.Fatal("accepted catalog manifest that differs from stored image")
	}
}

func TestResolveImageSourceRequiresResolverAndStore(t *testing.T) {
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	if _, err := ResolveImageSource(context.Background(), nil, "base:latest", platform, nil, nil); err == nil {
		t.Fatal("ResolveImageSource accepted a nil resolver")
	}
	resolver, err := oci.NewResolver(oci.Options{ImageStoreDir: filepath.Join(t.TempDir(), "images")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveImageSource(context.Background(), resolver, "base:latest", platform, nil, nil); err == nil {
		t.Fatal("ResolveImageSource accepted a nil containers/storage store")
	}
}

func TestSelectImageSourcePullNeverRejectsMissingLocalImage(t *testing.T) {
	resolver, err := oci.NewResolver(oci.Options{ImageStoreDir: t.TempDir(), PullPolicy: string(oci.PullNever)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = SelectImageSource(context.Background(), resolver, "registry.invalid/coopr/missing:latest", v1.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "pull policy is never") {
		t.Fatalf("missing local image error = %v", err)
	}
}

func TestResolveImageSourceRefreshesAuthoritativeSharedStoreName(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	backend, err := storage.GetStore(storage.StoreOptions{GraphDriverName: "vfs", GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = backend.Shutdown(true) })
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	makeStored := func(marker string) (string, v1.Descriptor) {
		dir := filepath.Join(root, marker)
		source, err := orasoci.NewWithContext(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		manifest, _ := sourceTestImage(t, ctx, source, platform, marker)
		if err := source.Tag(ctx, manifest, manifest.Digest.String()); err != nil {
			t.Fatal(err)
		}
		id, err := ImportSelectedImage(ctx, backend, system, dir, manifest)
		if err != nil {
			t.Fatal(err)
		}
		return id, manifest
	}
	firstID, firstManifest := makeStored("first")
	if err := backend.AddNames(firstID, []string{"localhost/shared-base:latest"}); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(root, "catalog")
	resolver, err := oci.NewResolver(oci.Options{ImageStoreDir: catalog, PullPolicy: string(oci.PullNever), NativeStoreShared: true})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveImageSource(ctx, resolver, "shared-base:latest", platform, backend, system)
	if err != nil || resolved.ImageID != firstID || resolved.Selected.Digest != firstManifest.Digest {
		t.Fatalf("first shared resolve = %+v, %v", resolved, err)
	}
	secondID, secondManifest := makeStored("second")
	if err := backend.AddNames(secondID, []string{"localhost/shared-base:latest"}); err != nil {
		t.Fatal(err)
	}
	resolved, err = ResolveImageSource(ctx, resolver, "shared-base:latest", platform, backend, system)
	if err != nil || resolved.ImageID != secondID || resolved.Selected.Digest != secondManifest.Digest {
		t.Fatalf("retagged shared resolve = %+v, %v", resolved, err)
	}
	if err := backend.RemoveNames(secondID, []string{"localhost/shared-base:latest"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveImageSource(ctx, resolver, "shared-base:latest", platform, backend, system); err == nil || !strings.Contains(err.Error(), "pull policy is never") {
		t.Fatalf("removed shared name resolve = %v", err)
	}
	if _, found, err := imagecatalog.Lookup(ctx, catalog, "shared-base:latest", platform); err != nil || found {
		t.Fatalf("stale catalog remains: found=%t err=%v", found, err)
	}
}

func TestResolveImageSourceEnforcesRegistryScopedPolicy(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for the live registry policy test")
	}
	ctx := context.Background()
	reference, authority := newLiveBusyBoxRegistry(t, ctx)
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
			t.Errorf("shutdown private store: %v", err)
		}
	})
	policy := filepath.Join(root, "policy.json")
	policyData := fmt.Sprintf(`{"default":[{"type":"insecureAcceptAnything"}],"transports":{"docker":{"%s/coopr/busybox":[{"type":"reject"}]}}}`, authority)
	if err := os.WriteFile(policy, []byte(policyData), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := oci.NewResolver(oci.Options{
		ImageStoreDir:       filepath.Join(root, "images"),
		PlainHTTPRegistries: []string{authority},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveImageSource(ctx, resolver, reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, store, &types.SystemContext{
		SignaturePolicyPath:  policy,
		BigFilesTemporaryDir: root,
	})
	if err == nil || !strings.Contains(err.Error(), "Source image rejected") {
		t.Fatalf("registry-scoped reject policy error = %v", err)
	}
}

func TestResolveImageSourcePullsRegistryDirectlyIntoBuildahStore(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for the live registry pull test")
	}
	ctx := context.Background()
	reference, authority := newLiveBusyBoxRegistry(t, ctx)
	root := t.TempDir()
	store, err := storage.GetStore(storage.StoreOptions{
		GraphDriverName: "vfs", GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = store.Shutdown(true) })
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := oci.NewResolver(oci.Options{
		ImageStoreDir: filepath.Join(root, "images"), PlainHTTPRegistries: []string{authority},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveImageSource(ctx, resolver, reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, store, &types.SystemContext{
		SignaturePolicyPath: policy, BigFilesTemporaryDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Remote || resolved.ImageID == "" {
		t.Fatalf("direct native pull result = %+v", resolved)
	}
	if image, err := store.Image(resolved.ImageID); err != nil || image == nil {
		t.Fatalf("pulled image %q missing from Buildah store: %+v, %v", resolved.ImageID, image, err)
	}
}

func sourceTestImage(t *testing.T, ctx context.Context, store *orasoci.Store, platform v1.Platform, marker string) (v1.Descriptor, v1.Descriptor) {
	t.Helper()
	configData, err := json.Marshal(v1.Image{
		Platform: platform,
		RootFS:   v1.RootFS{Type: "layers"},
		Config:   v1.ImageConfig{Env: []string{"SOURCE=" + marker}},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := descriptor(v1.MediaTypeImageConfig, configData)
	if err := store.Push(ctx, config, bytes.NewReader(configData)); err != nil {
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
	manifest := descriptor(v1.MediaTypeImageManifest, manifestData)
	if err := store.Push(ctx, manifest, bytes.NewReader(manifestData)); err != nil {
		t.Fatal(err)
	}
	return manifest, config
}

func sourceTestIndex(t *testing.T, ctx context.Context, store *orasoci.Store, manifests ...v1.Descriptor) v1.Descriptor {
	t.Helper()
	data, err := json.Marshal(v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex,
		Manifests: manifests,
	})
	if err != nil {
		t.Fatal(err)
	}
	root := descriptor(v1.MediaTypeImageIndex, data)
	if err := store.Push(ctx, root, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	return root
}
