package buildah

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"coopr/internal/oci"
)

func TestDirectionalComponentCacheReusesDefaultNetworkRunAcrossStores(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	base, _ := newLiveBusyBoxRegistry(t, ctx)
	componentResolver := localConfigComponentResolver(t, ctx, root, `extend
run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/proof"
`, false)
	resolver, err := oci.NewResolver(oci.Options{
		ComponentStoreDir: componentResolver.ComponentStoreDir(), TLSVerify: new(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, "from \""+base+"\"\ncomponent \"local:config\"\n")
	cacheDir := filepath.Join(root, "cache")
	build := func(name string, cacheFrom, cacheTo []CacheSpec) Result {
		t.Helper()
		baseDir := filepath.Join(root, name)
		result, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: StoreOptions{
				RunRoot: filepath.Join(baseDir, "run"), GraphRoot: filepath.Join(baseDir, "graph"), GraphDriverName: "vfs",
			},
			ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: Output{Path: filepath.Join(baseDir, "layout")},
			ComponentStoreDir: resolver.ComponentStoreDir(), CacheFrom: cacheFrom, CacheTo: cacheTo,
			TLSVerify:           new(false),
			SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	cacheSpec := []CacheSpec{{Transport: "oci-layout", Reference: cacheDir}}
	cold := build("cold", nil, cacheSpec)
	warm := build("warm", cacheSpec, nil)
	if cold.CacheStats.Misses != 2 || cold.CacheStats.Stored != 2 || warm.CacheStats.Hits != 1 || warm.CacheStats.Misses != 0 {
		t.Fatalf("networked component cache result: cold=%s %+v warm=%s %+v", cold.ManifestDigest, cold.CacheStats, warm.ManifestDigest, warm.CacheStats)
	}
	coldManifest, _ := readPlanImage(t, filepath.Join(root, "cold", "layout"))
	warmManifest, _ := readPlanImage(t, filepath.Join(root, "warm", "layout"))
	if len(coldManifest.Layers) == 0 || len(warmManifest.Layers) == 0 || coldManifest.Layers[len(coldManifest.Layers)-1].Digest != warmManifest.Layers[len(warmManifest.Layers)-1].Digest {
		t.Fatalf("networked component cache changed filesystem layers: cold=%v warm=%v", coldManifest.Layers, warmManifest.Layers)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "index.json")); err != nil {
		t.Fatalf("component cache layout: %v", err)
	}
}
