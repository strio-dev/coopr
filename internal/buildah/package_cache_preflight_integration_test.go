package buildah

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"coopr/internal/oci"
	"coopr/internal/planner"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestPublishDefinitionPackageCacheHitSkipsProducersAndBaseLayers(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah package cache")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	_, upstreamAuthority := newLiveBusyBoxRegistry(t, ctx)
	resolver, err := oci.NewResolver(oci.Options{PlainHTTPRegistries: []string{upstreamAuthority}})
	if err != nil {
		t.Fatal(err)
	}
	base, err := resolver.ResolveRemoteImage(ctx, upstreamAuthority+"/coopr/busybox:latest", v1.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := url.Parse("http://" + upstreamAuthority)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var layerFetches atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/blobs/") && !strings.HasSuffix(request.URL.Path, "/"+base.Config.Digest.String()) {
			layerFetches.Add(1)
		}
		proxy.ServeHTTP(w, request)
	}))
	t.Cleanup(server.Close)
	authority := strings.TrimPrefix(server.URL, "http://")
	reference := authority + "/coopr/busybox:latest"
	cacheDir := filepath.Join(root, "cache")
	componentStoreDir := filepath.Join(root, "components")
	policy := writeComponentTestPolicy(t, root)
	component := parseWorkerDefinition(t, `
from "`+reference+`" as="producer"
package as="from-bundle"
copy "/bin/busybox" "/from-busybox" from="producer"
package as="copy-bundle"
copy "/bin/busybox" "/copy-busybox" from="`+reference+`"
extend
copy "/from-busybox" "/from-busybox" from="from-bundle"
copy "/copy-busybox" "/copy-busybox" from="copy-bundle"
`)
	build := func(name string) (string, StoreOptions) {
		storeRoot := filepath.Join(root, name)
		store := StoreOptions{
			RunRoot: filepath.Join(storeRoot, "run"), GraphRoot: filepath.Join(storeRoot, "graph"), GraphDriverName: "vfs",
		}
		result, err := PublishDefinitionSupervised(ctx, component, planner.Options{
			Mode: planner.Publish, Platform: runtime.GOOS + "/" + runtime.GOARCH,
		}, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output:            Output{Path: filepath.Join(storeRoot, "component")},
			ComponentStoreDir: componentStoreDir, CacheLocalDir: cacheDir,
			PlainHTTPRegistries: []string{authority}, SignaturePolicyPath: policy,
			Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result.Root.Digest.String(), store
	}
	first, _ := build("first")
	if layerFetches.Load() == 0 {
		t.Fatal("cold package production did not fetch the base layer")
	}
	layerFetches.Store(0)
	second, fresh := build("second")
	if first != second {
		t.Fatalf("cached component digest changed: first=%s second=%s", first, second)
	}
	if fetches := layerFetches.Load(); fetches != 0 {
		t.Fatalf("portable package-cache hit fetched %d base layers", fetches)
	}
	// Planning may open native storage to resolve input metadata. A complete
	// package-cache hit must avoid materializing bases or executing producers.
	lease, err := acquireStore(fresh)
	if err != nil {
		t.Fatal(err)
	}
	images, imageErr := lease.store.Images()
	containers, containerErr := lease.store.Containers()
	closeErr := lease.Close()
	if err := errors.Join(imageErr, containerErr, closeErr); err != nil {
		t.Fatal(err)
	}
	if len(images) != 0 || len(containers) != 0 {
		t.Fatalf("portable package-cache hit executed into fresh store: images=%d containers=%d", len(images), len(containers))
	}
}
