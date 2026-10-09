package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"coopr/internal/cache"
	"coopr/internal/componentstore"
	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestBuildPlanInvokesComponentPreservingCopyLayer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless component invocation in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	resolver, _, _ := localComponentResolver(t, ctx, v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, "from \"scratch\"\ncomponent \"local:tool\" channel=\"stable\"\n")
	layout := filepath.Join(root, "layout")
	_, err := BuildPlan(ctx, plan, PlanOptions{
		Store: StoreOptions{
			RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
		},
		ContextDir: root, Isolation: "rootless", Output: Output{Path: layout}, Resolver: resolver,
		SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, image := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 || len(image.RootFS.DiffIDs) != 1 {
		t.Fatalf("component output layers=%d diffIDs=%d, want the component COPY layer", len(manifest.Layers), len(image.RootFS.DiffIDs))
	}
	if !reflect.DeepEqual(image.Config.Env, []string{"CHANNEL=stable"}) {
		t.Fatalf("component output env = %#v", image.Config.Env)
	}
}

func TestBuildPlanOverlapsIndependentComponentStages(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live parallel component coverage in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	var requests atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 2 {
			close(release)
		}
		select {
		case <-release:
			_, _ = response.Write([]byte("component proof\n"))
		case <-ctx.Done():
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(cancel)
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	resolver := localConfigComponentResolver(t, ctx, root, fmt.Sprintf(`extend
arg "branch"
add %q "/component-proof"
`, server.URL+"/${branch}"), false)
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, fmt.Sprintf(`
from %q as="left"
component "local:config" branch="left"
from %q as="right"
component "local:config" branch="right"
from "scratch"
copy "/component-proof" "/left" from="left"
copy "/component-proof" "/right" from="right"
`, base.reference, base.reference))
	if _, err := BuildPlan(ctx, plan, PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Resolver: resolver, Jobs: 2,
		NoCache:       true,
		Output:        Output{Path: filepath.Join(root, "layout")},
		SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
	}); err != nil {
		t.Fatalf("parallel component stages: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("component requests = %d, want 2 overlapping requests", got)
	}
}

func TestBuildPlanOverlapsIndependentBranchesInsideComponent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live component branch concurrency in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	resolver := localConfigComponentResolver(t, ctx, root, `extend as="left"
run "sleep 10; touch /left" network="none"
extend as="right"
run "sleep 10; touch /right" network="none"
from "left"
copy "/right" "/right" from="right"
`, false)
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, fmt.Sprintf("from %q\ncomponent \"local:config\"\n", base.reference))
	started := time.Now()
	if _, err := BuildPlan(ctx, plan, PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Resolver: resolver, Jobs: 2,
		Output:        Output{Path: filepath.Join(root, "layout")},
		SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
	}); err != nil {
		t.Fatalf("parallel component branches: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 19*time.Second {
		t.Fatalf("independent branches inside component ran serially: %s", elapsed)
	}
}

func TestComponentBranchesRespectJobsBound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live component job-bound coverage in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var active, maximum, requests atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		if requests.Add(1) == 2 {
			releaseOnce.Do(func() { close(release) })
		}
		select {
		case <-release:
		case <-ctx.Done():
			return
		}
		time.Sleep(300 * time.Millisecond)
		_, _ = response.Write([]byte("component branch\n"))
	}))
	defer server.Close()
	root := t.TempDir()
	resolver := localConfigComponentResolver(t, ctx, root, fmt.Sprintf(`extend as="a"
add %q "/a"
extend as="b"
add %q "/b"
extend as="c"
add %q "/c"
from "a"
copy "/b" "/b" from="b"
copy "/c" "/c" from="c"
`, server.URL+"/a", server.URL+"/b", server.URL+"/c"), false)
	policy := writeComponentTestPolicy(t, root)
	if _, err := BuildPlan(ctx, testPlan(t, "from \"scratch\"\ncomponent \"local:config\"\n"), PlanOptions{
		Store:      StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
		ContextDir: root, Isolation: "rootless", Runtime: "crun", Resolver: resolver, Jobs: 2,
		Output:        Output{Path: filepath.Join(root, "layout")},
		SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
	}); err != nil {
		t.Fatal(err)
	}
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum concurrent component branches = %d, want 2", got)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("component branch requests = %d, want 3", got)
	}
}

func TestComponentParallelBranchesHonorCacheMountSharing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live component cache-mount locking in short mode")
	}
	for _, test := range []struct {
		sharing, assertion string
	}{
		{sharing: "locked", assertion: "/bin/busybox test ! -e /cache/overlap"},
		{sharing: "shared", assertion: "/bin/busybox test -e /cache/overlap"},
	} {
		t.Run(test.sharing, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			root := t.TempDir()
			cacheSuffix := digest.FromString(root + test.sharing).Encoded()[:16]
			store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
			base := newLiveBusyBoxStorage(t, ctx, root, store)
			critical := "if /bin/busybox mkdir /cache/held 2>/dev/null; then /bin/busybox sleep 6; /bin/busybox rmdir /cache/held; else /bin/busybox touch /cache/overlap; fi"
			source := fmt.Sprintf(`extend as="init"
run "/bin/busybox rm -rf /cache/held /cache/overlap" network="none" {
  mount "cache" target="/cache" id="component-critical-%s" sharing=%q
}
from "init" as="left"
run %q network="none" {
  mount "cache" target="/cache" id="component-critical-%s" sharing=%q
}
run "touch /left" network="none"
from "init" as="right"
run %q network="none" {
  mount "cache" target="/cache" id="component-critical-%s" sharing=%q
}
run "touch /right" network="none"
from "left"
copy "/right" "/right" from="right"
run %q network="none" {
  mount "cache" target="/cache" id="component-critical-%s" sharing=%q
}
`, cacheSuffix, test.sharing, critical, cacheSuffix, test.sharing, critical, cacheSuffix, test.sharing, test.assertion, cacheSuffix, test.sharing)
			resolver := localConfigComponentResolver(t, ctx, root, source, false)
			_, err := ResolveComponentPlan(ctx, ComponentPlanRequest{
				Resolver: resolver, Reference: "local:config", Platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH},
				ResolveBase: func(context.Context, string, v1.Platform) (ResolvedImageSource, error) {
					return ResolvedImageSource{}, errors.New("unexpected external base")
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			policy := writeComponentTestPolicy(t, root)
			plan := testPlan(t, fmt.Sprintf("from %q\ncomponent \"local:config\"\n", base.reference))
			if _, err := BuildPlan(ctx, plan, PlanOptions{
				Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Resolver: resolver, Jobs: 2,
				NoCache:       true,
				Output:        Output{Path: filepath.Join(root, "layout")},
				SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
			}); err != nil {
				t.Fatalf("component %s cache branches: %v", test.sharing, err)
			}
		})
	}
}

func TestCallerAndComponentCanSequenceSameLockedCacheMount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live component cache-mount locking in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	cacheID := "component-caller-" + digest.FromString(root).Encoded()[:16]
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	componentSource := fmt.Sprintf(`extend
run "/bin/busybox touch /cache/from-component" network="none" {
  mount "cache" target="/cache" id=%q sharing="locked"
}
`, cacheID)
	resolver := localConfigComponentResolver(t, ctx, root, componentSource, false)
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, fmt.Sprintf(`from %q
component "local:config"
run "/bin/busybox test -e /cache/from-component" network="none" {
  mount "cache" target="/cache" id=%q sharing="locked"
}
`, base.reference, cacheID))
	if _, err := BuildPlan(ctx, plan, PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Resolver: resolver, Jobs: 2,
		NoCache:       true,
		Output:        Output{Path: filepath.Join(root, "layout")},
		SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
	}); err != nil {
		t.Fatalf("caller and component using the same locked cache mount: %v", err)
	}
}

func TestBuildPlanReplansFromCompletedComponentConfiguration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live component-stage configuration inheritance in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	localConfigComponentResolver(t, ctx, root, `extend
env COMPONENT_VALUE="ready"
onbuild { env TRIGGERED="$COMPONENT_VALUE" }
onbuild { copy "/payload" "/inherited" from="helper" }
`, false)
	policy := writeComponentTestPolicy(t, root)
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("component-onbuild\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	def, err := definition.Parse(strings.NewReader(`from "scratch" as="helper"
copy "payload" "/payload"
from "scratch" as="base"
component "local:config"
from "base" as="final"
env RESULT="$COMPONENT_VALUE"
`))
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := planDefinitionWithExternalBases(ctx, def, planner.Options{
		Mode: planner.Build, Target: "final", Platform: "linux/" + runtime.GOARCH,
	}, func(_ context.Context, reference string, _ v1.Platform) (ResolvedImageSource, error) {
		return ResolvedImageSource{}, fmt.Errorf("unexpected external base %q", reference)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Stages) != 2 || plan.Stages[1].DynamicBaseStage != "1" {
		t.Fatalf("initial component descendant was not deferred: %+v", plan.Stages)
	}

	cacheDir := filepath.Join(root, "cache")
	var cold v1.Image
	for attempt := range 2 {
		layout := filepath.Join(root, fmt.Sprintf("dynamic-layout-%d", attempt))
		result, err := BuildDefinitionSupervised(ctx, def, planner.Options{
			Mode: planner.Build, Target: "final", Platform: "linux/" + runtime.GOARCH,
		}, SupervisedPlanOptions{
			Store: StoreOptions{
				RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
			},
			ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
			ComponentStoreDir: filepath.Join(root, "components"),
			CacheLocalDir:     cacheDir, Pull: false, SignaturePolicyPath: policy,
			Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, layout)
		want := []string{"COMPONENT_VALUE=ready", "TRIGGERED=ready", "RESULT=ready"}
		if !reflect.DeepEqual(image.Config.Env, want) {
			t.Fatalf("build %d inherited config env=%v, want %v", attempt+1, image.Config.Env, want)
		}
		last := manifest.Layers[len(manifest.Layers)-1]
		if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "inherited"); got != "component-onbuild\n" {
			t.Fatalf("build %d inherited dormant-stage copy = %q", attempt+1, got)
		}
		if attempt == 0 {
			cold = image
			continue
		}
		if !reflect.DeepEqual(image.Config, cold.Config) {
			t.Fatalf("warm component descendant cache/config stats=%+v cold=%+v warm=%+v", result.CacheStats, cold.Config, image.Config)
		}
	}
}

func TestBuildPlanReusesPortableFilesystemComponentCache(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless component cache in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	resolver, _, _ := localComponentResolver(t, ctx, v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, "from \"scratch\"\ncomponent \"local:tool\" channel=\"stable\"\n")
	cacheDir := filepath.Join(root, "cache")
	build := func(layout, storeName string, noCache bool) (Result, v1.Manifest, v1.Image) {
		t.Helper()
		options := componentTestOptions(root, layout, resolver, policy)
		options.CacheLocalDir = cacheDir
		options.NoCache = noCache
		if storeName != "" {
			options.Store.RunRoot = filepath.Join(root, storeName, "run")
			options.Store.GraphRoot = filepath.Join(root, storeName, "graph")
		}
		result, err := BuildPlan(ctx, plan, options)
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, layout)
		return result, manifest, image
	}
	cold, coldManifest, coldImage := build(filepath.Join(root, "cold"), "", false)
	warm, warmManifest, warmImage := build(filepath.Join(root, "warm"), "", false)
	refreshed, _, _ := build(filepath.Join(root, "refreshed"), "", true)
	crossStore, crossManifest, _ := build(filepath.Join(root, "cross-store"), "second-store", false)
	if cold.CacheStats.Misses < 1 || cold.CacheStats.Stored < 1 || cold.CacheStats.Hits != 0 {
		t.Fatalf("cold component cache stats = %+v", cold.CacheStats)
	}
	if warm.CacheStats.Hits != 1 || warm.CacheStats.Misses != 0 {
		t.Fatalf("warm component cache stats = %+v", warm.CacheStats)
	}
	if refreshed.CacheStats.Hits != 0 || refreshed.CacheStats.Misses != 0 || refreshed.CacheStats.Stored < 1 {
		t.Fatalf("NoCache component cache stats = %+v", refreshed.CacheStats)
	}
	if crossStore.CacheStats.Hits != 1 || len(crossManifest.Layers) != 1 {
		t.Fatalf("independent store cache stats = %+v, layers=%d", crossStore.CacheStats, len(crossManifest.Layers))
	}
	different := testPlan(t, "from \"scratch\"\ncomponent \"local:tool\" channel=\"other\"\n")
	differentOptions := componentTestOptions(root, filepath.Join(root, "different-parameter"), resolver, policy)
	differentOptions.CacheLocalDir = cacheDir
	differentResult, err := BuildPlan(ctx, different, differentOptions)
	if err != nil {
		t.Fatal(err)
	}
	_, differentImage := readPlanImage(t, differentOptions.Output.Path)
	if differentResult.CacheStats.Misses < 1 || differentResult.CacheStats.Hits != 0 || !reflect.DeepEqual(differentImage.Config.Env, []string{"CHANNEL=other"}) {
		t.Fatalf("different parameter reused cached output: stats=%+v env=%v", differentResult.CacheStats, differentImage.Config.Env)
	}
	if len(coldManifest.Layers) != 1 || len(warmManifest.Layers) != 1 ||
		coldManifest.Layers[0].Digest != warmManifest.Layers[0].Digest ||
		!reflect.DeepEqual(coldImage.Config, warmImage.Config) || !reflect.DeepEqual(coldImage.RootFS, warmImage.RootFS) {
		t.Fatalf("cached component changed output: cold layers=%v config=%+v rootfs=%+v, warm layers=%v config=%+v rootfs=%+v", coldManifest.Layers, coldImage.Config, coldImage.RootFS, warmManifest.Layers, warmImage.Config, warmImage.RootFS)
	}
	if got := readLayerFile(t, filepath.Join(root, "warm", "blobs", "sha256", warmManifest.Layers[0].Digest.Encoded()), "artifact"); got != "package payload\n" {
		t.Fatalf("cached component file = %q", got)
	}
}

func TestPublishedCompatibilityAllowListsSurviveColdAndWarmComponentCache(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live compatibility allow-list cache coverage in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	componentSource := fmt.Sprintf(`
extend {
  architecture %q %q
  package-manager "missing-manager" "sh"
}
env COMPATIBILITY_LIST="yes"
`, differentArchitecture(runtime.GOARCH), runtime.GOARCH)
	resolver := localConfigComponentResolver(t, ctx, root, componentSource, false)
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, "from \""+base.reference+"\"\ncomponent \"local:config\"\n")
	cacheDir := filepath.Join(root, "cache")
	for index, name := range []string{"cold", "warm"} {
		options := componentTestOptions(root, filepath.Join(root, name), resolver, policy)
		options.CacheLocalDir = cacheDir
		result, err := BuildPlan(ctx, plan, options)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 && (result.CacheStats.Misses != 1 || result.CacheStats.Stored != 1 || result.CacheStats.Hits != 0) {
			t.Fatalf("cold compatibility-list cache stats = %+v", result.CacheStats)
		}
		if index == 1 && (result.CacheStats.Hits != 1 || result.CacheStats.Misses != 0) {
			t.Fatalf("warm compatibility-list cache stats = %+v", result.CacheStats)
		}
		_, image := readPlanImage(t, options.Output.Path)
		if !slices.Contains(image.Config.Env, "COMPATIBILITY_LIST=yes") {
			t.Fatalf("%s compatibility-list component env = %v", name, image.Config.Env)
		}
	}
}

func TestComponentCacheReusesNonemptyCaller(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live nonempty caller cache in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	storeOptions := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, storeOptions)
	resolver := localConfigComponentResolver(t, ctx, root, "extend\nenv CACHE_BASE=\"yes\"\n", false)
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, "from \""+base.reference+"\"\ncomponent \"local:config\"\n")
	cacheDir := filepath.Join(root, "cache")
	var coldManifest v1.Manifest
	var coldImage v1.Image
	for index, name := range []string{"cold", "warm"} {
		options := componentTestOptions(root, filepath.Join(root, name), resolver, policy)
		options.CacheLocalDir = cacheDir
		result, err := BuildPlan(ctx, plan, options)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 && (result.CacheStats.Misses != 1 || result.CacheStats.Stored != 1) ||
			index == 1 && result.CacheStats.Hits != 1 {
			t.Fatalf("%s nonempty caller cache stats: %+v", name, result.CacheStats)
		}
		manifest, image := readPlanImage(t, options.Output.Path)
		if !reflect.DeepEqual(image.Config.Env, []string{"PATH=/bin", "CACHE_BASE=yes"}) {
			t.Fatalf("%s component output env = %v", name, image.Config.Env)
		}
		if index == 0 {
			coldManifest, coldImage = manifest, image
		} else if len(manifest.Layers) == 0 || !reflect.DeepEqual(coldManifest.Layers, manifest.Layers) ||
			!reflect.DeepEqual(coldImage.RootFS, image.RootFS) {
			t.Fatalf("cached nonempty caller changed base layers: cold=%v warm=%v", coldManifest.Layers, manifest.Layers)
		}
	}
}

func TestBuildPlanInvokesComponentWithPackageStageMount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live package-backed component invocation in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	resolver, _, _ := localComponentResolverWithDefinition(t, ctx, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, `
package as="pkg"
extend
run "cat /package/artifact >/proof" network="none" {
  mount "bind" from="pkg" source="/" target="/package"
}
`)
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, "from \""+base.reference+"\"\ncomponent \"local:tool\"\n")
	layout := filepath.Join(root, "layout")
	_, err := BuildPlan(ctx, plan, PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout},
		Resolver: resolver, SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	last := manifest.Layers[len(manifest.Layers)-1]
	if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof"); got != "package payload\n" {
		t.Fatalf("component package mount result = %q", got)
	}
}

func TestBuildPlanInvokesConfigOnlyComponentFromScratch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless component invocation in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	resolver := localConfigComponentResolver(t, ctx, root, "extend\nenv CONFIG_ONLY=\"yes\"\n", false)
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, "from \"scratch\"\ncomponent \"local:config\"\n")
	layout := filepath.Join(root, "layout")
	_, err := BuildPlan(ctx, plan, componentTestOptions(root, layout, resolver, policy))
	if err != nil {
		t.Fatal(err)
	}
	manifest, image := readPlanImage(t, layout)
	if len(manifest.Layers) != 0 || len(image.RootFS.DiffIDs) != 0 {
		t.Fatalf("config-only component layers=%d diffIDs=%d, want no synthetic filesystem layer", len(manifest.Layers), len(image.RootFS.DiffIDs))
	}
	if !reflect.DeepEqual(image.Config.Env, []string{"CONFIG_ONLY=yes"}) {
		t.Fatalf("config-only component env = %#v", image.Config.Env)
	}
}

func TestBuildPlanReusesPortableConfigOnlyComponentCache(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless component cache in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	resolver := localConfigComponentResolver(t, ctx, root, "extend\nenv CACHE_PROOF=\"yes\"\n", false)
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, "from \"scratch\"\ncomponent \"local:config\"\n")
	cacheDir := filepath.Join(root, "cache")
	build := func(layout string) (Result, v1.Manifest, v1.Image) {
		t.Helper()
		options := componentTestOptions(root, layout, resolver, policy)
		options.CacheLocalDir = cacheDir
		result, err := BuildPlan(ctx, plan, options)
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, layout)
		return result, manifest, image
	}
	cold, coldManifest, coldImage := build(filepath.Join(root, "cold"))
	warm, warmManifest, warmImage := build(filepath.Join(root, "warm"))
	if cold.CacheStats.Misses < 1 || cold.CacheStats.Stored < 1 || cold.CacheStats.Hits != 0 {
		t.Fatalf("cold component cache stats = %+v", cold.CacheStats)
	}
	if warm.CacheStats.Hits != 1 || warm.CacheStats.Misses != 0 {
		t.Fatalf("warm component cache stats = %+v", warm.CacheStats)
	}
	if len(coldManifest.Layers) != 0 || len(warmManifest.Layers) != 0 || !reflect.DeepEqual(coldImage.Config.Env, warmImage.Config.Env) {
		t.Fatalf("cached component changed output: cold layers=%d env=%v, warm layers=%d env=%v", len(coldManifest.Layers), coldImage.Config.Env, len(warmManifest.Layers), warmImage.Config.Env)
	}
	indexData, err := os.ReadFile(filepath.Join(cacheDir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		t.Fatal(err)
	}
	var artifact v1.Descriptor
	for _, descriptor := range index.Manifests {
		if strings.HasPrefix(descriptor.Annotations[v1.AnnotationRefName], "instruction-") {
			if artifact.Digest != "" {
				t.Fatal("multiple tagged cache records")
			}
			artifact = descriptor
		}
	}
	if artifact.Digest == "" {
		t.Fatal("missing tagged component image cache record")
	}
	manifestPath := filepath.Join(cacheDir, "blobs", "sha256", artifact.Digest.Encoded())
	artifactData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var cacheManifest v1.Manifest
	if err := json.Unmarshal(artifactData, &cacheManifest); err != nil {
		t.Fatal(err)
	}
	if cacheManifest.ArtifactType != cache.ImageArtifactType || len(cacheManifest.Layers) != 2 {
		t.Fatalf("component record graphs=%+v", cacheManifest)
	}

	if err := os.WriteFile(manifestPath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	writable := componentTestOptions(root, filepath.Join(root, "rejected"), resolver, policy)
	writable.CacheLocalDir = cacheDir
	if _, err := BuildPlan(ctx, plan, writable); err == nil || !strings.Contains(err.Error(), "open component cache") {
		t.Fatalf("corrupt writable cache error = %v", err)
	}
	if _, err := os.Stat(writable.Output.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupt writable cache published an output: %v", err)
	}
	readOnly := componentTestOptions(root, filepath.Join(root, "fallback"), resolver, policy)
	readOnly.CacheFrom = []CacheSpec{{Transport: "oci-layout", Reference: cacheDir}}
	fallback, err := BuildPlan(ctx, plan, readOnly)
	if err != nil {
		t.Fatal(err)
	}
	_, fallbackImage := readPlanImage(t, readOnly.Output.Path)
	if fallback.CacheStats.Hits != 0 || fallback.CacheStats.Errors == 0 || !reflect.DeepEqual(fallbackImage.Config.Env, coldImage.Config.Env) {
		t.Fatalf("corrupt cache affected required component: stats=%+v env=%v", fallback.CacheStats, fallbackImage.Config.Env)
	}
}

func TestSupervisedBuildReusesComponentCacheAcrossWorkers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless component cache in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	_ = localConfigComponentResolver(t, ctx, root, "extend\nenv WORKER_CACHE=\"yes\"\n", false)
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, "from \"scratch\"\ncomponent \"local:config\"\n")
	build := func(layout string) Result {
		t.Helper()
		result, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: StoreOptions{
				RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
			},
			ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
			ComponentStoreDir: filepath.Join(root, "components"),
			CacheLocalDir:     filepath.Join(root, "cache"), SignaturePolicyPath: policy,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	cold := build(filepath.Join(root, "cold"))
	warm := build(filepath.Join(root, "warm"))
	if cold.CacheStats.Misses < 1 || cold.CacheStats.Stored < 1 || warm.CacheStats.Hits != 1 {
		t.Fatalf("supervised cache stats: cold=%+v warm=%+v", cold.CacheStats, warm.CacheStats)
	}
	_, image := readPlanImage(t, filepath.Join(root, "warm"))
	if !reflect.DeepEqual(image.Config.Env, []string{"WORKER_CACHE=yes"}) {
		t.Fatalf("cached worker image env = %v", image.Config.Env)
	}
}

func TestBuildPlanReusesRegistryComponentCacheAcrossStores(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless registry cache in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	componentResolver, _, _ := localComponentResolver(t, ctx, v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
	server := httptest.NewServer(registryserver.New())
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	resolver, err := oci.NewResolver(oci.Options{
		ComponentStoreDir: componentResolver.ComponentStoreDir(),
		TLSVerify:         new(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, "from \"scratch\"\ncomponent \"local:tool\" channel=\"stable\"\n")
	localCache := filepath.Join(root, "local-cache")
	registryWritable := true
	build := func(layout, name string, useLocal bool) Result {
		t.Helper()
		options := componentTestOptions(root, layout, resolver, policy)
		options.Store.RunRoot = filepath.Join(root, name, "run")
		options.Store.GraphRoot = filepath.Join(root, name, "graph")
		if registryWritable {
			options.CacheRepository = host + "/coopr/cache"
		} else {
			options.CacheFrom = []CacheSpec{{Transport: "registry", Reference: host + "/coopr/cache"}}
		}
		if useLocal {
			options.CacheLocalDir = localCache
		}
		result, err := BuildPlan(ctx, plan, options)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	cold := build(filepath.Join(root, "cold"), "first-store", false)
	warm := build(filepath.Join(root, "warm"), "second-store", true)
	if cold.CacheStats.Misses < 1 || cold.CacheStats.Stored < 1 || warm.CacheStats.Hits != 1 || warm.CacheStats.Stored != 1 {
		t.Fatalf("registry cache stats: cold=%+v warm=%+v", cold.CacheStats, warm.CacheStats)
	}
	if _, err := os.Stat(filepath.Join(localCache, "index.json")); err != nil {
		t.Fatalf("registry hit did not seed the local cache: %v", err)
	}
	manifest, image := readPlanImage(t, filepath.Join(root, "warm"))
	if !reflect.DeepEqual(image.Config.Env, []string{"CHANNEL=stable"}) || len(manifest.Layers) != 1 {
		t.Fatalf("cached registry image env = %v", image.Config.Env)
	}
	if got := readLayerFile(t, filepath.Join(root, "warm", "blobs", "sha256", manifest.Layers[0].Digest.Encoded()), "artifact"); got != "package payload\n" {
		t.Fatalf("cached registry filesystem = %q", got)
	}
	server.Close()
	registryWritable = false
	offline := build(filepath.Join(root, "offline"), "third-store", true)
	if offline.CacheStats.Hits != 1 {
		t.Fatalf("offline build did not use seeded local cache: %+v", offline.CacheStats)
	}
	unavailable := build(filepath.Join(root, "unavailable"), "fourth-store", false)
	_, unavailableImage := readPlanImage(t, filepath.Join(root, "unavailable"))
	if unavailable.CacheStats.Hits != 0 || unavailable.CacheStats.Errors == 0 || !reflect.DeepEqual(unavailableImage.Config.Env, []string{"CHANNEL=stable"}) {
		t.Fatalf("unavailable optional registry changed build: stats=%+v env=%v", unavailable.CacheStats, unavailableImage.Config.Env)
	}
}

func TestBuildPlanUsesComponentShellForFollowingRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live component RUN ordering test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	resolver := localConfigComponentResolver(t, ctx, root, "extend\nshell \"/bin/sh\" \"-c\" \"printf component-shell >/shell-selected\"\n", false)
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, "from \""+base.reference+"\"\ncomponent \"local:config\"\nrun \"printf default-shell >/default-shell\" network=\"none\"\n")
	layout := filepath.Join(root, "layout")
	options := componentTestOptions(root, layout, resolver, policy)
	options.Store = store
	options.Runtime = "crun"
	_, err := BuildPlan(ctx, plan, options)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	last := manifest.Layers[len(manifest.Layers)-1]
	if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "shell-selected"); got != "component-shell" {
		t.Fatalf("component-selected shell proof = %q", got)
	}
}

func TestBuildPlanAppliesNetworkOptionsToComponentRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live component network build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	component := "extend\nrun \"found=; while read ip name rest; do if test x$ip = x127.0.0.43 && test x$name = xcomponent.test; then found=yes; fi; done </etc/hosts; test x$found = xyes || exit 1; printf component-host >/component-proof\"\n"
	resolver := localConfigComponentResolver(t, ctx, root, component, false)
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, "from \""+base.reference+"\"\ncomponent \"local:config\"\n")
	layout := filepath.Join(root, "layout")
	options := componentTestOptions(root, layout, resolver, policy)
	options.Store = store
	options.Runtime = "crun"
	options.Network = "host"
	options.AddHosts = []string{"Component.Test:127.0.0.43"}
	if _, err := BuildPlan(ctx, plan, options); err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	last := manifest.Layers[len(manifest.Layers)-1]
	if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "component-proof"); got != "component-host" {
		t.Fatalf("component custom host proof = %q", got)
	}
}

func TestBuildPlanInvokesNestedComponentsAndRejectsCycles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live nested component invocation in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	resolver := localConfigComponentResolverWithDefinitions(t, ctx, root, []componentDefinitionFixture{
		{name: "inner", source: "extend\nenv INNER=\"yes\"\n"},
		{name: "outer", source: "extend\ncomponent \"local:inner\"\nenv OUTER=\"yes\"\n"},
		{name: "cycle", source: "extend\ncomponent \"local:cycle\"\n"},
	}, false)
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, "from \"scratch\"\ncomponent \"local:outer\"\n")
	layout := filepath.Join(root, "nested-layout")
	if _, err := BuildPlan(ctx, plan, componentTestOptions(root, layout, resolver, policy)); err != nil {
		t.Fatal(err)
	}
	manifest, image := readPlanImage(t, layout)
	if len(manifest.Layers) != 0 || !reflect.DeepEqual(image.Config.Env, []string{"INNER=yes", "OUTER=yes"}) {
		t.Fatalf("nested component layers=%d env=%#v", len(manifest.Layers), image.Config.Env)
	}

	cycle := testPlan(t, "from \"scratch\"\ncomponent \"local:cycle\"\n")
	cycleLayout := filepath.Join(root, "cycle-layout")
	if _, err := BuildPlan(ctx, cycle, componentTestOptions(root, cycleLayout, resolver, policy)); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("self-recursive component error = %v, want cycle", err)
	}
	if _, err := os.Stat(cycleLayout); !os.IsNotExist(err) {
		t.Fatalf("failed cycle left OCI output: %v", err)
	}
}

type componentDefinitionFixture struct {
	name   string
	source string
}

func localConfigComponentResolver(t *testing.T, ctx context.Context, root, source string, pull bool) *oci.Resolver {
	return localConfigComponentResolverWithDefinitions(t, ctx, root, []componentDefinitionFixture{{name: "config", source: source}}, pull)
}

func localConfigComponentResolverWithDefinitions(t *testing.T, ctx context.Context, root string, fixtures []componentDefinitionFixture, pull bool) *oci.Resolver {
	t.Helper()
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	componentDir := filepath.Join(root, "components")
	for _, fixture := range fixtures {
		def, err := definition.Parse(strings.NewReader(fixture.source))
		if err != nil {
			t.Fatal(err)
		}
		publication, err := planner.Create(def, planner.Options{Mode: planner.Publish, Platform: platform.OS + "/" + platform.Architecture})
		if err != nil {
			t.Fatal(err)
		}
		layout := filepath.Join(root, "component-layout-"+fixture.name)
		descriptor, err := oci.WriteComponentLayout(ctx, layout, oci.ComponentMetadata{
			Version: oci.ComponentVersion, Platform: platform, Component: *publication.Component,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		sourceStore, err := orasoci.NewWithContext(ctx, layout)
		if err != nil {
			t.Fatal(err)
		}
		if err := componentstore.Put(ctx, componentDir, sourceStore, descriptor, fixture.name); err != nil {
			t.Fatal(err)
		}
	}
	resolver, err := oci.NewResolver(oci.Options{
		ComponentStoreDir: componentDir, Pull: pull,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func writeComponentTestPolicy(t *testing.T, root string) string {
	t.Helper()
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return policy
}

func componentTestOptions(root, layout string, resolver *oci.Resolver, policy string) PlanOptions {
	return PlanOptions{
		Store: StoreOptions{
			RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
		},
		ContextDir: root, Isolation: "rootless", Output: Output{Path: layout}, Resolver: resolver,
		SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
	}
}
