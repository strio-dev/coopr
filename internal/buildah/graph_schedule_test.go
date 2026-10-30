package buildah

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/strio-dev/coopr/internal/planner"
)

func TestGraphDrainPreservesSiblingCompletionFailures(t *testing.T) {
	stageFailure := WorkerFailure(errors.New("RUN failed"))
	stageFailure.Code = 42
	cleanupFailure := errors.New("remove sibling builder failed")
	incomplete := errors.Join(context.Canceled, ErrWorkerWaitIncomplete, cleanupFailure)
	stages := []planner.Stage{{ID: "sibling"}, {ID: "failed"}}
	for _, ctxErr := range []error{nil, context.Canceled, context.DeadlineExceeded} {
		t.Run(fmt.Sprintf("caller cancellation %v", ctxErr), func(t *testing.T) {
			err := graphDrainError(ctxErr, stages, "failed", stageFailure, map[string]error{
				"failed": stageFailure, "sibling": incomplete,
			})
			for _, want := range []error{stageFailure, context.Canceled, ErrWorkerWaitIncomplete, cleanupFailure} {
				if !errors.Is(err, want) {
					t.Errorf("graph drain lost %v: %v", want, err)
				}
			}
			if ctxErr != nil {
				return
			}
			data, marshalErr := json.Marshal(WorkerFailure(err))
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			var relayed workerError
			if err := json.Unmarshal(data, &relayed); err != nil {
				t.Fatal(err)
			}
			if relayed.ExitCode() != 42 || !errors.Is(&relayed, ErrWorkerWaitIncomplete) {
				t.Fatalf("graph failure relay lost RUN status or incomplete wait: %s", data)
			}
		})
	}
}

func TestGraphDrainKeepsSourceOrderAndSingleFailureDiagnostic(t *testing.T) {
	first := WorkerFailure(errors.New("first failed"))
	first.Code = 17
	second := WorkerFailure(errors.New("second failed"))
	second.Code = 42
	stages := []planner.Stage{{ID: "first"}, {ID: "second"}}
	combined := graphDrainError(nil, stages, "second", second, map[string]error{"first": first, "second": second})
	if WorkerFailure(combined).ExitCode() != 17 || !errors.Is(combined, second) {
		t.Fatalf("graph drain lost source order or second failure: %v", combined)
	}
	if got := graphDrainError(nil, stages, "second", second, map[string]error{"second": second}); got.Error() != second.Error() {
		t.Fatalf("single failure diagnostic = %q, want %q", got, second)
	}
	global := errors.New("graph failed")
	combined = graphDrainError(nil, stages, "", global, map[string]error{"": global, "first": context.Canceled})
	if !errors.Is(combined, global) || !errors.Is(combined, context.Canceled) {
		t.Fatalf("graph drain lost graph or cancelled-stage failure: %v", combined)
	}
}

func TestGraphDrainPreservesCoordinatorStatusAfterCancelledCompletion(t *testing.T) {
	coordinator := WorkerFailure(ErrWorkerWaitIncomplete)
	coordinator.Code = 42
	transport := WorkerFailure(errors.New("worker transport failed"))
	transport.Code = 1
	stages := []planner.Stage{{ID: "sibling"}, {ID: "requested"}}
	failures := map[string]error{
		"sibling":   errors.Join(context.Canceled, transport),
		"requested": errors.Join(coordinator, context.Canceled),
	}
	for _, ctxErr := range []error{nil, context.Canceled, context.DeadlineExceeded} {
		err := graphDrainError(ctxErr, stages, "requested", coordinator, failures)
		if failure := WorkerFailure(err); failure.ExitCode() != 42 || !failure.WaitIncomplete {
			t.Fatalf("draining cancelled completions replaced coordinator status: %+v", failure)
		}
		if !errors.Is(err, coordinator) || !errors.Is(err, transport) || !errors.Is(err, context.Canceled) {
			t.Fatalf("coordinator status took precedence by discarding diagnostics: %v", err)
		}
	}
	earlier := WorkerFailure(errors.New("earlier substantive failure"))
	earlier.Code = 17
	failures["sibling"] = earlier
	if got := WorkerFailure(graphDrainError(nil, stages, "requested", coordinator, failures)); got.ExitCode() != 17 || !got.WaitIncomplete {
		t.Fatalf("original coordinator displaced an earlier substantive failure: %+v", got)
	}
	if got := graphDrainError(nil, stages[1:], "requested", nil, failures); !errors.Is(got, coordinator) {
		t.Fatalf("missing original primary discarded completed stage failure: %v", got)
	}
}

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

func TestRunCacheLockCancellationWhileContended(t *testing.T) {
	root := t.TempDir()
	mounts := []RunMount{{Type: "cache", Properties: map[string]string{"id": "held", "sharing": "locked"}}}
	first, err := lockRunCacheMounts(context.Background(), root, mounts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := first.close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	second, err := lockRunCacheMounts(ctx, root, mounts)
	if second != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended cancellation = %v, %v", second, err)
	}
}

func TestRunCacheLockPartialFailureReleasesEarlierLocks(t *testing.T) {
	root := t.TempDir()
	lockDir := filepath.Join(root, "coopr-cache-locks")
	if err := os.MkdirAll(filepath.Join(lockDir, fmt.Sprintf("%x.lock", sha256.Sum256([]byte("b")))), 0o700); err != nil {
		t.Fatal(err)
	}
	mount := func(id string) RunMount {
		return RunMount{Type: "cache", Properties: map[string]string{"id": id, "sharing": "locked"}}
	}
	if locks, err := lockRunCacheMounts(context.Background(), root, []RunMount{mount("a"), mount("b")}); err == nil || locks != nil {
		t.Fatalf("invalid lock path accepted: %v, %v", locks, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	locks, err := lockRunCacheMounts(ctx, root, []RunMount{mount("a")})
	if err != nil {
		t.Fatalf("earlier lock leaked: %v", err)
	}
	if err := locks.close(); err != nil {
		t.Fatal(err)
	}
	if err := locks.close(); err != nil {
		t.Fatal(err)
	}
}
