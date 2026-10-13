package buildah

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuildPlanNoCacheCopyAppliesWithoutProbe(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "source"), []byte("uncached payload\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, `from "scratch" {
 copy "source" "/new-parent/renamed"
}
`)
	var probes, applies atomic.Int32
	observer := copyAddDigestObservation(func(dryRun bool) {
		if dryRun {
			probes.Add(1)
		} else {
			applies.Add(1)
		}
	})
	copyAddDigestObserver.Store(&observer)
	t.Cleanup(func() { copyAddDigestObserver.Store(nil) })
	layout := filepath.Join(root, "output")
	if _, err := BuildPlan(ctx, plan, PlanOptions{
		Store: cacheTestStore(root), ContextDir: contextDir, Isolation: "rootless",
		NoCache: true, Output: Output{Path: layout},
	}); err != nil {
		t.Fatal(err)
	}
	if probes.Load() != 0 || applies.Load() != 1 {
		t.Fatalf("no-cache COPY calls: probes=%d applies=%d; want 0, 1", probes.Load(), applies.Load())
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("layers = %d; want 1", len(manifest.Layers))
	}
	layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	if got := readLayerFile(t, layer, "new-parent/renamed"); got != "uncached payload\n" {
		t.Fatalf("COPY payload = %q", got)
	}
}
