package buildah

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/strio-dev/coopr/internal/oci"
	"go.podman.io/image/v5/types"
)

func TestPublishPlanLiveHostReadsDoNotReuseAggregatePackage(t *testing.T) {
	for _, volume := range []bool{false, true} {
		t.Run(fmt.Sprintf("native-volume=%v", volume), func(t *testing.T) {
			testPublishPlanLiveHostReads(t, volume)
		})
	}
}

func testPublishPlanLiveHostReads(t *testing.T, volume bool) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir, live := filepath.Join(root, "context"), filepath.Join(root, "live")
	for _, dir := range []string{contextDir, live} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	store := cacheTestStore(filepath.Join(root, "store"))
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := writeComponentTestPolicy(t, root)
	resolver, err := oci.NewResolver(oci.Options{PullPolicy: "never", NativeStore: NativeStoreOptions(store), ComponentStoreDir: filepath.Join(root, "components"), SignaturePolicyPath: policy})
	if err != nil {
		t.Fatal(err)
	}
	plan := testPublicationPlan(t, fmt.Sprintf(`from %q as="producer" {
    run "cat /live/value >/proof; od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/nonce" network="none" { mount "bind" target="/live" readonly=#true relabel="shared"; }
}
package as="bundle" { copy "/proof" "/proof" from="producer"; copy "/nonce" "/nonce" from="producer"; }
extend { copy "/proof" "/proof" from="bundle"; }
`, base.reference))
	operation := &plan.Stages[0].Operations[0]
	operation.HostMounts = map[int]string{0: live}
	delete(operation.MountContexts, 0)
	var controls RunControls
	if volume {
		operation.HostMounts, operation.MountContexts, operation.Children = nil, nil, nil
		controls, err = ParseRunControls(RunControlInput{Volumes: []string{live + ":/live:ro,z"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	var previousNonce string
	for attempt, value := range []string{"first", "later", "later"} {
		if err := os.WriteFile(filepath.Join(live, "value"), []byte(value), 0o640); err != nil {
			t.Fatal(err)
		}
		fixed := time.Unix(1700000000, 0)
		if err := os.Chtimes(filepath.Join(live, "value"), fixed, fixed); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(root, fmt.Sprintf("bundle-%d.tar", attempt))
		if _, err := PublishPlan(ctx, plan, PublicationOptions{PlanOptions: PlanOptions{
			Store: store, ContextDir: contextDir, Isolation: "rootless", Runtime: "crun", Resolver: resolver, CacheLocalDir: filepath.Join(root, "cache"), SystemContext: &types.SystemContext{SignaturePolicyPath: policy}, RunControls: controls,
		}, PackagePaths: map[string]string{"bundle": output}}); err != nil {
			t.Fatal(err)
		}
		assertPackageTarFile(t, output, "proof", value)
		nonce := readLayerFile(t, output, "nonce")
		if nonce == "" || nonce == previousNonce {
			t.Fatalf("live input producer did not execute on attempt %d: nonce %q", attempt, nonce)
		}
		previousNonce = nonce
	}
}

func TestNativeHostVolumeDoesNotReuseWholeComponent(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	live := filepath.Join(f.root, "live")
	if err := os.Mkdir(live, 0o700); err != nil {
		t.Fatal(err)
	}
	var err error
	f.options.RunControls, err = ParseRunControls(RunControlInput{Volumes: []string{live + ":/live:ro,z"}})
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, "read.coopr", `extend {
    run "cat /live/value >/proof; od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/nonce" network="none"
}
`)
	var previousNonce string
	for attempt, value := range []string{"first", "later", "later"} {
		path := filepath.Join(live, "value")
		if err := os.WriteFile(path, []byte(value), 0o640); err != nil {
			t.Fatal(err)
		}
		fixed := time.Unix(1700000000, 0)
		if err := os.Chtimes(path, fixed, fixed); err != nil {
			t.Fatal(err)
		}
		_, layout := f.build(t, fmt.Sprintf("component-%d", attempt), fmt.Sprintf(`from %q { component "./read.coopr"; }`, base.reference))
		if proof := localComponentLastFile(t, layout, "proof"); proof != value {
			t.Fatalf("component host read = %q, want %q", proof, value)
		}
		nonce := localComponentLastFile(t, layout, "nonce")
		if nonce == "" || nonce == previousNonce {
			t.Fatalf("component host RUN did not execute on attempt %d: %q", attempt, nonce)
		}
		previousNonce = nonce
	}
	f.write(t, "metadata.coopr", `extend { env cache_control="retained"; }`)
	for attempt := range 2 {
		result, layout := f.build(t, fmt.Sprintf("metadata-%d", attempt), fmt.Sprintf(`from %q { component "./metadata.coopr"; }`, base.reference))
		_, image := readPlanImage(t, layout)
		found := false
		for _, value := range image.Config.Env {
			found = found || value == "cache_control=retained"
		}
		if !found || attempt == 1 && result.CacheStats.Hits == 0 {
			t.Fatalf("unused volume disabled metadata-only component reuse: env=%v cache=%+v", image.Config.Env, result.CacheStats)
		}
	}
}
