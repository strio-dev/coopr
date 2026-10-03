package buildah

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"coopr/internal/planner"
)

func TestReadyGraphStagesLaunchDownstreamAsSoonAsDependencyCompletes(t *testing.T) {
	stages := []planner.Stage{
		{ID: "a0"},
		{ID: "a1", Dependencies: []string{"a0"}},
		{ID: "b0"},
		{ID: "b1", Dependencies: []string{"b0"}},
		{ID: "final", Dependencies: []string{"a1", "b1"}},
	}
	running := map[string]bool{}
	completed := map[string]bool{}
	ready, err := readyGraphStages(stages, completed, running, 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{0, 2}; !reflect.DeepEqual(ready, want) {
		t.Fatalf("initial ready = %v, want %v", ready, want)
	}
	running["a0"], running["b0"] = true, true
	delete(running, "a0")
	completed["a0"] = true
	ready, err = readyGraphStages(stages, completed, running, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{1}; !reflect.DeepEqual(ready, want) {
		t.Fatalf("ready after a0 = %v, want a1 while b0 is still running", ready)
	}
}

func TestGraphStageLabelIncludesStableIdentityAndPlatform(t *testing.T) {
	if got, want := graphStageLabel(planner.Stage{ID: "stage-0", Name: "build", Platform: "linux/amd64"}), "build (stage-0) [linux/amd64]"; got != want {
		t.Fatalf("stage label = %q, want %q", got, want)
	}
	if got, want := graphStageLabel(planner.Stage{ID: "stage-1", Platform: "linux/arm64"}), "stage-1 [linux/arm64]"; got != want {
		t.Fatalf("unnamed stage label = %q, want %q", got, want)
	}
}

func TestLockedRunCacheMountIDs(t *testing.T) {
	ids := lockedRunCacheMountIDs([]RunMount{
		{Type: "cache", Properties: map[string]string{"id": "z", "sharing": "locked"}},
		{Type: "cache", Properties: map[string]string{"id": "a", "sharing": "locked"}},
		{Type: "cache", Properties: map[string]string{"id": "ignored", "sharing": "shared"}},
		{Type: "bind", Properties: map[string]string{"id": "ignored", "sharing": "locked"}},
		{Type: "cache", Properties: map[string]string{"id": "a", "sharing": "locked"}},
	})
	if want := []string{"a", "z"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("locked cache IDs = %v, want %v", ids, want)
	}
}

func TestRunCacheLocksBlockUntilCurrentRunReleases(t *testing.T) {
	root := t.TempDir()
	mounts := []RunMount{{Type: "cache", Properties: map[string]string{"id": "critical", "sharing": "locked"}}}
	first, err := lockRunCacheMounts(context.Background(), root, mounts)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan *runCacheLocks, 1)
	errs := make(chan error, 1)
	go func() {
		second, err := lockRunCacheMounts(context.Background(), root, mounts)
		if err != nil {
			errs <- err
			return
		}
		acquired <- second
	}()
	select {
	case second := <-acquired:
		_ = second.close()
		t.Fatal("second RUN acquired locked cache mount before the first RUN released it")
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	select {
	case second := <-acquired:
		if err := second.close(); err != nil {
			t.Fatal(err)
		}
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("second RUN did not acquire locked cache mount after release")
	}
}

func TestRunCacheLockWaitStopsOnCancellation(t *testing.T) {
	root := t.TempDir()
	mounts := []RunMount{{Type: "cache", Properties: map[string]string{"id": "critical", "sharing": "locked"}}}
	first, err := lockRunCacheMounts(context.Background(), root, mounts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	locks, err := lockRunCacheMounts(ctx, root, mounts)
	if locks != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cache lock = %v, %v; want nil, context.Canceled", locks, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancelled cache lock returned after %s", elapsed)
	}
}

func TestRunCacheLockDoesNotAcquireAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	locks, err := lockRunCacheMounts(ctx, t.TempDir(), []RunMount{{
		Type: "cache", Properties: map[string]string{"id": "free", "sharing": "locked"},
	}})
	if locks != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled free cache lock = %v, %v; want nil, context.Canceled", locks, err)
	}
}

func TestReadyGraphStagesValidatesGraphAndJobLimit(t *testing.T) {
	if ready, err := readyGraphStages([]planner.Stage{{ID: "a"}}, nil, nil, 0); err != nil || ready != nil {
		t.Fatalf("zero capacity ready=%v err=%v", ready, err)
	}
	if _, err := readyGraphStages([]planner.Stage{{ID: "a"}, {ID: "a"}}, nil, nil, 1); err == nil {
		t.Fatal("duplicate stage ID was accepted")
	}
	if _, err := readyGraphStages([]planner.Stage{{ID: "a", Dependencies: []string{"missing"}}}, nil, nil, 1); err == nil {
		t.Fatal("missing dependency was accepted")
	}
}

func TestStageAliasesExcludeNamesDeclaredLater(t *testing.T) {
	stages := []planner.Stage{{ID: "0", Name: "base"}, {ID: "1", Source: "future"}, {ID: "2", Name: "future"}}
	aliases := stageAliases(stages)
	if aliases[1]["base"] != "0" || aliases[1]["future"] != "" || aliases[2]["future"] != "" {
		t.Fatalf("stage-local aliases = %+v", aliases)
	}
}
