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

func TestPublishFileInputPackageCacheDefersImageLayers(t *testing.T) {
	for _, operation := range []string{"copy", "add"} {
		t.Run(operation, func(t *testing.T) {
			testPublishFileInputPackageCacheDefersImageLayers(t, operation)
		})
	}
}

func testPublishFileInputPackageCacheDefersImageLayers(t *testing.T, operation string) {
	if testing.Short() {
		t.Skip("skipping live rootless Buildah publication")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	_, upstreamAuthority := newLiveBusyBoxRegistry(t, ctx)
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	base, err := resolver.ResolveRemoteImage(ctx, upstreamAuthority+"/coopr/busybox:latest", platform)
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := url.Parse("http://" + upstreamAuthority)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var layerFetches atomic.Int64
	var denyLayers atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") && !strings.HasSuffix(r.URL.Path, "/"+base.Config.Digest.String()) {
			layerFetches.Add(1)
			if denyLayers.Load() {
				http.Error(w, "source layers unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	reference := strings.TrimPrefix(server.URL, "http://") + "/coopr/busybox:latest"
	component := parseWorkerDefinition(t, `
package as="bundle" {
 `+operation+` "/bin/busybox" "/busybox" from="`+reference+`"
}
extend {
 copy "/busybox" "/busybox" from="bundle"
}
`)
	policy := writeComponentTestPolicy(t, root)
	options := planner.Options{Mode: planner.Publish, Platform: runtime.GOOS + "/" + runtime.GOARCH}
	// The only external image occurs in the file input, so FROM cannot select it first.
	denyLayers.Store(true)
	planningStore := cacheTestStore(filepath.Join(root, "planning"))
	_, _, selected, err := planDefinitionInWorker(ctx, planWorkerRequest{
		Mode: "publish", Definition: component, PlannerOptions: &options, Store: planningStore,
		ResultPath: filepath.Join(root, "planning-result.json"), ContextDir: root,
		TLSVerify: new(false), SignaturePolicyPath: policy, RetrySet: true,
	})
	if err != nil {
		t.Fatalf("metadata selection with unavailable layers: %v", err)
	}
	selection := selected.all[ResolvedBaseKey{Reference: reference, Platform: options.Platform}]
	if !selection.Remote || selection.Artifact || selection.Selected.Digest != base.Selected.Digest {
		t.Fatalf("metadata selection = %+v", selection)
	}
	if got := layerFetches.Load(); got != 0 {
		t.Fatalf("metadata selection fetched %d layers", got)
	}
	build := func(name string) (string, StoreOptions) {
		t.Helper()
		store := cacheTestStore(filepath.Join(root, name))
		result, err := PublishDefinitionSupervised(ctx, component, options, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output:            Output{Path: filepath.Join(root, name, "component")},
			ComponentStoreDir: filepath.Join(root, "components"), CacheLocalDir: filepath.Join(root, "cache"),
			TLSVerify: new(false), SignaturePolicyPath: policy, RetrySet: true,
			Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result.Root.Digest.String(), store
	}
	denyLayers.Store(false)
	first, _ := build("cold")
	if layerFetches.Load() == 0 {
		t.Fatal("cold package did not materialize image layers")
	}
	layerFetches.Store(0)
	denyLayers.Store(true)
	second, fresh := build("warm")
	if first != second {
		t.Fatalf("cached component changed: %s != %s", first, second)
	}
	if got := layerFetches.Load(); got != 0 {
		t.Fatalf("cached package fetched %d unavailable layers", got)
	}
	lease, err := acquireStore(fresh)
	if err != nil {
		t.Fatal(err)
	}
	images, imageErr := lease.store.Images()
	containers, containerErr := lease.store.Containers()
	if err := errors.Join(imageErr, containerErr, lease.Close()); err != nil {
		t.Fatal(err)
	}
	if len(images) != 0 || len(containers) != 0 {
		t.Fatalf("cache hit materialized images=%d containers=%d", len(images), len(containers))
	}
}

func TestPublishFileInputStillClassifiesStoredArtifact(t *testing.T) {
	ctx := context.Background()
	store, system := artifactSourceTestStore(t)
	reference := "registry.example/publication/files:latest"
	artifact := artifactSourceTestAdd(t, ctx, store, reference, map[string]string{"payload": "artifact"})
	component := parseWorkerDefinition(t, `
package as="bundle" { copy "payload" "/payload" from="`+reference+`" }
extend { copy "/payload" "/payload" from="bundle" }
`)
	options := planner.Options{Mode: planner.Publish, Platform: runtime.GOOS + "/" + runtime.GOARCH}
	_, _, selected, err := planDefinitionInWorker(ctx, planWorkerRequest{
		Mode: "publish", Definition: component, PlannerOptions: &options,
		Store:      StoreOptions{GraphRoot: store.GraphRoot(), RunRoot: store.RunRoot(), GraphDriverName: "vfs"},
		PullPolicy: "never", SignaturePolicyPath: system.SignaturePolicyPath,
		ResultPath: filepath.Join(t.TempDir(), "result.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	selection := selected.all[ResolvedBaseKey{Reference: reference, Platform: options.Platform}]
	if !selection.Artifact || selection.ImageID != "" || selection.Selected.Digest != artifact.Digest {
		t.Fatalf("artifact classification = %+v", selection)
	}
}

func TestPublishFileInputRejectsSourcePolicy(t *testing.T) {
	root := t.TempDir()
	policy := filepath.Join(root, "source-policy.json")
	if err := os.WriteFile(policy, []byte(`{"rules":[{"action":"DENY","selector":{"identifier":"docker-image://registry.invalid/publication/files:latest"}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	component := parseWorkerDefinition(t, `
package as="bundle" { copy "/payload" "/payload" from="registry.invalid/publication/files:latest" }
extend { copy "/payload" "/payload" from="bundle" }
`)
	options := planner.Options{Mode: planner.Publish, Platform: runtime.GOOS + "/" + runtime.GOARCH}
	_, _, _, err := planDefinitionInWorker(t.Context(), planWorkerRequest{
		Mode: "publish", Definition: component, PlannerOptions: &options,
		Store: cacheTestStore(root), ResultPath: filepath.Join(root, "result.json"),
		SourcePolicyFile: policy, PullPolicy: "never",
	})
	if err == nil || !strings.Contains(err.Error(), "source policy denied image") {
		t.Fatalf("publication file source policy error = %v", err)
	}
}
