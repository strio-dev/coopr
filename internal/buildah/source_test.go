package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"coopr/internal/oci"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
)

func TestResolveImageSourceReusesNativeSelectionFromBuildahStore(t *testing.T) {
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

	canonicalRef := "registry.example/team/base:latest"
	resolver, err := oci.NewResolver(oci.Options{})
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
	if err := privateStore.AddNames(imageID, []string{canonicalRef}); err != nil {
		t.Fatal(err)
	}
	rootData, err := content.FetchAll(ctx, source, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := privateStore.SetImageBigData(imageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+root.Digest.String(), rootData, func([]byte) (digest.Digest, error) { return root.Digest, nil }); err != nil {
		t.Fatal(err)
	}
	if err := oci.RecordStoredOrigin(ctx, privateStore, canonicalRef, oci.StoredSelection{Root: root, Manifest: amdManifest, ImageID: imageID, ConfigData: configData}); err != nil {
		t.Fatal(err)
	}
	if image, err := privateStore.Image(armConfig.Digest.Encoded()); err == nil {
		t.Fatalf("unselected arm64 image exists: %+v", image)
	}
	if _, err := oci.ResolveStoredImage(ctx, privateStore, canonicalRef, arm64); !errors.Is(err, storage.ErrImageUnknown) {
		t.Fatalf("unselected arm64 lookup: %v", err)
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
	selection, err := oci.ResolveStoredImage(ctx, privateStore, canonicalRef, amd64)
	if err != nil || selection.Root.Digest != root.Digest || selection.Selected.Digest != amdManifest.Digest {
		t.Fatalf("native selection = %+v, %v", selection, err)
	}
	tampered := amdManifest
	tampered.Digest = digest.FromString("different-manifest")
	if err := verifyStoredManifest(ctx, privateStore, system, oci.StoredSelection{Root: root, Manifest: tampered, ImageID: imageID, ConfigData: configData}); err == nil {
		t.Fatal("accepted mismatched native manifest")
	}

}

func TestResolveImageSourceRequiresResolverAndStore(t *testing.T) {
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	if _, err := ResolveImageSource(context.Background(), nil, "base:latest", platform, nil, nil); err == nil {
		t.Fatal("ResolveImageSource accepted a nil resolver")
	}
	resolver, err := oci.NewResolver(oci.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveImageSource(context.Background(), resolver, "base:latest", platform, nil, nil); err == nil {
		t.Fatal("ResolveImageSource accepted a nil containers/storage store")
	}
}

func TestSelectImageSourcePullNeverRejectsMissingLocalImage(t *testing.T) {
	resolver, err := oci.NewResolver(oci.Options{NativeStore: NativeStoreOptions(cacheTestStore(t.TempDir())), PullPolicy: string(oci.PullNever)})
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
	resolver, err := oci.NewResolver(oci.Options{PullPolicy: string(oci.PullNever)})
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
		PlainHTTPRegistries: []string{authority},
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

func TestResolveImageSourceSharedWrongPlatformPullPolicies(t *testing.T) {
	ctx := context.Background()
	var requests atomic.Int64
	registryHandler := registry.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); registryHandler.ServeHTTP(w, r) }))
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	reference := parsed.Host + "/base:latest"
	sourceDir := t.TempDir()
	source, err := orasoci.NewWithContext(ctx, sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	amd := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm := v1.Platform{OS: "linux", Architecture: "arm64"}
	amdManifest, _ := sourceTestImage(t, ctx, source, amd, "amd")
	armManifest, _ := sourceTestImage(t, ctx, source, arm, "arm")
	amdChild, armChild := amdManifest, armManifest
	amdChild.Platform = &amd
	armChild.Platform = &arm
	index := sourceTestIndex(t, ctx, source, amdChild, armChild)
	if err := source.Tag(ctx, index, "latest"); err != nil {
		t.Fatal(err)
	}
	repo, err := remote.NewRepository(reference)
	if err != nil {
		t.Fatal(err)
	}
	repo.PlainHTTP = true
	if _, err := oras.Copy(ctx, source, "latest", repo, "latest", oras.CopyOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []oci.PullPolicy{oci.PullMissing, oci.PullNewer, oci.PullNever} {
		t.Run(string(policy), func(t *testing.T) {
			root := t.TempDir()
			backend, err := storage.GetStore(storage.StoreOptions{GraphDriverName: "vfs", GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run")})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = backend.Shutdown(true) })
			policyFile := filepath.Join(root, "policy.json")
			if err := os.WriteFile(policyFile, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			system := &types.SystemContext{SignaturePolicyPath: policyFile, BigFilesTemporaryDir: root}
			id, err := ImportSelectedImage(ctx, backend, system, sourceDir, amdManifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.AddNames(id, []string{reference}); err != nil {
				t.Fatal(err)
			}
			resolver, err := oci.NewResolver(oci.Options{PullPolicy: string(policy), PlainHTTPRegistries: []string{parsed.Host}})
			if err != nil {
				t.Fatal(err)
			}
			data, err := content.FetchAll(ctx, source, armManifest)
			if err != nil {
				t.Fatal(err)
			}
			var manifest v1.Manifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			before := requests.Load()
			resolved, err := ResolveImageSource(ctx, resolver, reference, arm, backend, system)
			if policy == oci.PullNever {
				if !errors.Is(err, oci.ErrStoredPlatformUnavailable) || requests.Load() != before {
					t.Fatalf("never error=%v registry requests=%d", err, requests.Load()-before)
				}
				return
			}
			if err != nil || resolved.Selected.Digest != armManifest.Digest {
				t.Fatalf("resolved=%+v error=%v", resolved, err)
			}
			if requests.Load() == before {
				t.Fatal("permitted pull did not contact registry")
			}
			// Genuine stored-content corruption must still fail before remote contact.
			if err := backend.SetImageBigData(resolved.ImageID, manifest.Config.Digest.String(), []byte("invalid"), nil); err != nil {
				t.Fatal(err)
			}
			before = requests.Load()
			_, err = ResolveImageSource(ctx, resolver, reference, arm, backend, system)
			if err == nil || errors.Is(err, oci.ErrStoredPlatformUnavailable) || requests.Load() != before {
				t.Fatalf("corrupt error=%v registry requests=%d", err, requests.Load()-before)
			}
		})
	}
}
