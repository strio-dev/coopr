package buildah

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestRunMountCacheHistoryReusesEquivalentImageFilesystems(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "payload"), []byte("immutable-input"), 0600); err != nil {
		t.Fatal(err)
	}
	store := cacheTestStore(filepath.Join(root, "store"))
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := writeComponentTestPolicy(t, root)
	options := func(layout string) SupervisedPlanOptions {
		return SupervisedPlanOptions{Store: store, ContextDir: contextDir, Runtime: "crun", Isolation: "rootless", PullPolicy: "never", SignaturePolicyPath: policy, Output: Output{Path: layout}, Stdout: io.Discard, Stderr: io.Discard}
	}
	seed, err := BuildPlanSupervised(ctx, testPlan(t, fmt.Sprintf(`from %q { copy "payload" "/bin/payload" }`, base.reference)), options(filepath.Join(root, "seed")))
	if err != nil {
		t.Fatal(err)
	}
	var sources []Result
	var roots []v1.RootFS
	for _, name := range []string{"first", "second"} {
		layout := filepath.Join(root, name)
		result, err := BuildPlanSupervised(ctx, testPlan(t, fmt.Sprintf(`from %q { env "SOURCE_CONFIG" %q }`, "containers-storage:"+seed.ImageID, name)), options(layout))
		if err != nil {
			t.Fatal(err)
		}
		_, image := readPlanImage(t, layout)
		sources = append(sources, result)
		roots = append(roots, image.RootFS)
	}
	if sources[0].ImageID == sources[1].ImageID {
		t.Fatal("fixture did not produce distinct source image handles")
	}
	if roots[0].Type != roots[1].Type || !slices.Equal(roots[0].DiffIDs, roots[1].DiffIDs) {
		t.Fatal("fixture filesystem changed with source-only image config")
	}
	for index, source := range sources {
		var log, stdout strings.Builder
		layout := filepath.Join(root, fmt.Sprintf("consumer-%d", index))
		opts := options(layout)
		opts.CacheLocalDir = filepath.Join(root, "results")
		opts.Stderr = &log
		opts.Stdout = &stdout
		plan := testPlan(t, fmt.Sprintf(`from %q {
   run "cat /input >/proof && echo COOPR_VIEW_RAN" network="none" {
    mount "bind" from=%q source="/bin/payload" target="/input"
   }
  }`, base.reference, "containers-storage:"+source.ImageID))
		if _, err := BuildPlanSupervised(ctx, plan, opts); err != nil {
			t.Fatalf("consumer %d: %v\n%s", index, err, log.String())
		}
		if index == 0 && !strings.Contains(stdout.String(), "COOPR_VIEW_RAN") {
			t.Fatal("cold consumer did not execute")
		}
		if index == 1 && (!strings.Contains(log.String(), "Using cache") || strings.Contains(stdout.String(), "COOPR_VIEW_RAN")) {
			t.Fatalf("equivalent source filesystem was reexecuted:\nstdout=%s\nstderr=%s", stdout.String(), log.String())
		}
		manifest, image := readPlanImage(t, layout)
		last := manifest.Layers[len(manifest.Layers)-1]
		if proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof"); proof != "immutable-input" {
			t.Fatalf("consumer proof = %q", proof)
		}
		if len(image.History) == 0 || !strings.Contains(image.History[len(image.History)-1].CreatedBy, "RUN") {
			t.Fatal("cached instruction history was lost")
		}
	}
}
