package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/oci"
	registryserver "github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
	"oras.land/oras-go/v2/registry/remote"
)

func TestPublishPlanPartialPackageCacheMissRestoresSelectedBaseAcrossStores(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah package cache")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	server := httptest.NewServer(registryserver.New())
	t.Cleanup(server.Close)
	authority := strings.TrimPrefix(server.URL, "http://")
	repository := authority + "/coopr/partial-base"
	oldManifest := pushPackageCacheBase(t, ctx, repository, "latest", "old selection\n")

	root := t.TempDir()
	resolver, err := oci.NewResolver(oci.Options{
		TLSVerify: new(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Separate native stores resolve mutable tags independently. Pin the input
	// selected for this publication so the partial miss must restore that base
	// even after the registry tag moves.
	reference := repository + "@" + oldManifest.Digest.String()
	cacheDir := filepath.Join(root, "cache")
	build := func(name, revision string) (map[string]oci.Package, StoreOptions) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "revision"), []byte(revision), 0o600); err != nil {
			t.Fatal(err)
		}
		plan := testPublicationPlan(t, `
from "`+reference+`" as="producer"
package as="cached"
copy "/variant" "/cached-variant" from="producer"
package as="rebuilt"
copy "/variant" "/rebuilt-variant" from="producer"
copy "revision" "/revision"
extend
copy "/cached-variant" "/cached-variant" from="cached"
copy "/rebuilt-variant" "/rebuilt-variant" from="rebuilt"
`)
		store := StoreOptions{
			RunRoot: filepath.Join(root, name, "run"), GraphRoot: filepath.Join(root, name, "graph"), GraphDriverName: "vfs",
		}
		cachedPath := filepath.Join(root, name, "cached.tar")
		rebuiltPath := filepath.Join(root, name, "rebuilt.tar")
		packages, err := PublishPlan(ctx, plan, PublicationOptions{
			PlanOptions: PlanOptions{
				Store: store, ContextDir: root, Isolation: "rootless", Resolver: resolver,
				CacheLocalDir: cacheDir,
				SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
			},
			PackagePaths: map[string]string{"cached": cachedPath, "rebuilt": rebuiltPath},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertPackageTarFile(t, cachedPath, "cached-variant", "old selection\n")
		assertPackageTarFile(t, rebuiltPath, "rebuilt-variant", "old selection\n")
		assertPackageTarFile(t, rebuiltPath, "revision", revision)
		return packages, store
	}

	first, _ := build("first", "one")
	redirectedManifest := pushPackageCacheBase(t, ctx, repository, "latest", "redirected tag\n")
	if redirectedManifest.Digest == oldManifest.Digest {
		t.Fatal("test registry tag did not move")
	}
	second, secondStore := build("second", "two")
	if first["cached"].Descriptor.Digest != second["cached"].Descriptor.Digest {
		t.Fatal("unchanged package payload changed across native stores")
	}
	if first["rebuilt"].Descriptor.Digest == second["rebuilt"].Descriptor.Digest {
		t.Fatal("changed revision did not rebuild its package payload")
	}

	selection, found, err := nativeFixtureSelection(ctx, secondStore, reference, v1.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH})
	if err != nil || !found {
		t.Fatalf("lookup selected base after rebuild: found=%v err=%v", found, err)
	}
	if selection.Manifest.Digest != oldManifest.Digest {
		t.Fatalf("partial miss redirected mutable base: selected=%s old=%s redirected=%s", selection.Manifest.Digest, oldManifest.Digest, redirectedManifest.Digest)
	}
	lease, err := acquireStore(secondStore)
	if err != nil {
		t.Fatal(err)
	}
	_, imageErr := lease.store.Image(selection.ImageID)
	closeErr := lease.Close()
	if imageErr != nil || closeErr != nil {
		t.Fatalf("restored selected base in fresh store: image=%v close=%v", imageErr, closeErr)
	}
}

func pushPackageCacheBase(t *testing.T, ctx context.Context, repositoryName, tag, variant string) v1.Descriptor {
	t.Helper()
	var layerBuffer bytes.Buffer
	layerWriter := tar.NewWriter(&layerBuffer)
	if err := layerWriter.WriteHeader(&tar.Header{Name: "variant", Mode: 0o644, Size: int64(len(variant))}); err != nil {
		t.Fatal(err)
	}
	if _, err := layerWriter.Write([]byte(variant)); err != nil {
		t.Fatal(err)
	}
	if err := layerWriter.Close(); err != nil {
		t.Fatal(err)
	}
	layerData := layerBuffer.Bytes()
	layer := descriptor(v1.MediaTypeImageLayer, layerData)
	platform := v1.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	configData, err := json.Marshal(v1.Image{
		Platform: platform,
		RootFS:   v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{digest.FromBytes(layerData)}},
		History:  []v1.History{{CreatedBy: "coopr partial package-cache test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := descriptor(v1.MediaTypeImageConfig, configData)
	manifestData, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest,
		Config: config, Layers: []v1.Descriptor{layer},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := descriptor(v1.MediaTypeImageManifest, manifestData)
	repository, err := remote.NewRepository(repositoryName)
	if err != nil {
		t.Fatal(err)
	}
	repository.PlainHTTP = true
	for _, blob := range []struct {
		descriptor v1.Descriptor
		data       []byte
	}{{layer, layerData}, {config, configData}} {
		if err := repository.Push(ctx, blob.descriptor, bytes.NewReader(blob.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.PushReference(ctx, manifest, bytes.NewReader(manifestData), tag); err != nil {
		t.Fatal(err)
	}
	return manifest
}
