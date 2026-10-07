package buildah

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"coopr/internal/oci"
	"github.com/google/go-containerregistry/pkg/registry"
	"go.podman.io/image/v5/types"
)

func TestPublishPlanReusesRegistryPackageCacheAcrossBuildStores(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah package cache")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "payload"), []byte("registry package\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	resolver, err := oci.NewResolver(oci.Options{PlainHTTP: true, ComponentStoreDir: filepath.Join(root, "components")})
	if err != nil {
		t.Fatal(err)
	}
	plan := testPublicationPlan(t, `
package as="payload"
copy "payload" "/payload"
extend
copy "/payload" "/payload" from="payload"
`)
	repository := strings.TrimPrefix(server.URL, "http://") + "/coopr/package-cache"
	build := func(name string, noCache bool) (oci.Package, StoreOptions) {
		store := StoreOptions{RunRoot: filepath.Join(root, name, "run"), GraphRoot: filepath.Join(root, name, "graph"), GraphDriverName: "vfs"}
		path := filepath.Join(root, name, "payload.tar")
		packages, err := PublishPlan(ctx, plan, PublicationOptions{
			PlanOptions: PlanOptions{
				Store: store, ContextDir: contextDir, Isolation: "rootless", Runtime: "crun",
				Resolver: resolver, CacheRepository: repository, NoCache: noCache,
			},
			PackagePaths: map[string]string{"payload": path},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertPackageTarFile(t, path, "payload", "registry package\n")
		return packages["payload"], store
	}
	first, _ := build("first", false)
	second, secondStore := build("second", false)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("registry package cache changed package: first=%+v second=%+v", first, second)
	}
	lease, err := acquireStore(secondStore)
	if err != nil {
		t.Fatal(err)
	}
	images, imageErr := lease.store.Images()
	closeErr := lease.Close()
	if imageErr != nil || closeErr != nil {
		t.Fatalf("inspect second build store: %v", errors.Join(imageErr, closeErr))
	}
	if len(images) != 0 {
		t.Fatalf("warm registry package cache executed production into second store: %d images", len(images))
	}
	refreshed, refreshedStore := build("refreshed", true)
	if first.Descriptor.MediaType != refreshed.Descriptor.MediaType || first.Descriptor.Digest != refreshed.Descriptor.Digest || first.Descriptor.Size != refreshed.Descriptor.Size {
		t.Fatalf("NoCache package payload changed: first=%+v refreshed=%+v", first.Descriptor, refreshed.Descriptor)
	}
	lease, err = acquireStore(refreshedStore)
	if err != nil {
		t.Fatal(err)
	}
	images, imageErr = lease.store.Images()
	closeErr = lease.Close()
	if imageErr != nil || closeErr != nil {
		t.Fatalf("inspect NoCache build store: %v", errors.Join(imageErr, closeErr))
	}
	if len(images) == 0 {
		t.Fatal("NoCache package build did not execute production")
	}
	third, thirdStore := build("third", false)
	if !reflect.DeepEqual(refreshed, third) {
		t.Fatalf("refreshed package cache changed package: refreshed=%+v third=%+v", refreshed, third)
	}
	lease, err = acquireStore(thirdStore)
	if err != nil {
		t.Fatal(err)
	}
	images, imageErr = lease.store.Images()
	closeErr = lease.Close()
	if imageErr != nil || closeErr != nil {
		t.Fatalf("inspect post-refresh build store: %v", errors.Join(imageErr, closeErr))
	}
	if len(images) != 0 {
		t.Fatalf("post-refresh package cache build executed production: %d images", len(images))
	}
}

func TestPublishPlanReusesNetworkedPackageFromRegistryCache(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah package cache")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	base, host := newLiveBusyBoxRegistry(t, ctx)
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := oci.NewResolver(oci.Options{
		PlainHTTPRegistries: []string{host}, ComponentStoreDir: filepath.Join(root, "components"),
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := testPublicationPlan(t, `
from "`+base+`" as="producer"
run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/payload"
package as="payload"
copy "/payload" "/payload" from="producer"
extend
copy "/payload" "/payload" from="payload"
`)
	build := func(name string) (oci.Package, StoreOptions) {
		t.Helper()
		store := StoreOptions{RunRoot: filepath.Join(root, name, "run"), GraphRoot: filepath.Join(root, name, "graph"), GraphDriverName: "vfs"}
		path := filepath.Join(root, name, "payload.tar")
		packages, err := PublishPlan(ctx, plan, PublicationOptions{
			PlanOptions: PlanOptions{
				Store: store, ContextDir: contextDir, Isolation: "rootless", Runtime: "crun", Resolver: resolver,
				CacheRepository: host + "/coopr/networked-package-cache",
				SystemContext:   &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
			},
			PackagePaths: map[string]string{"payload": path},
		})
		if err != nil {
			t.Fatal(err)
		}
		return packages["payload"], store
	}
	first, _ := build("first")
	second, secondStore := build("second")
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("networked package cache changed package: first=%+v second=%+v", first, second)
	}
	lease, err := acquireStore(secondStore)
	if err != nil {
		t.Fatal(err)
	}
	images, imageErr := lease.store.Images()
	closeErr := lease.Close()
	if imageErr != nil || closeErr != nil {
		t.Fatalf("inspect networked package warm store: %v", errors.Join(imageErr, closeErr))
	}
	if len(images) != 0 {
		t.Fatalf("networked package cache hit executed producer into fresh store: %d images", len(images))
	}
}
