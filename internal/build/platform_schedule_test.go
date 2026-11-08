package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/oci"
)

func TestBuildJobLimitsBoundPlatformsAndStages(t *testing.T) {
	for _, test := range []struct {
		platforms, jobs           int
		wantPlatforms, wantStages int
	}{
		{1, 4, 1, 4},
		{2, 4, 2, 2},
		{4, 3, 3, 1},
		{4, 1, 1, 1},
	} {
		platformJobs, stageJobs := buildJobLimits(test.jobs, test.platforms)
		if platformJobs != test.wantPlatforms || stageJobs != test.wantStages {
			t.Fatalf("allocation(%d, %d) = (%d, %d), want (%d, %d)", test.platforms, test.jobs, platformJobs, stageJobs, test.wantPlatforms, test.wantStages)
		}
		if platformJobs*stageJobs > test.jobs {
			t.Fatalf("allocation(%d, %d) exceeds jobs bound: %d*%d", test.platforms, test.jobs, platformJobs, stageJobs)
		}
	}
}

func TestBuildJobLimitsZeroRunsAllPlatformsAndStages(t *testing.T) {
	platformJobs, stageJobs := buildJobLimits(0, 4)
	if platformJobs != 4 || stageJobs != 0 {
		t.Fatalf("allocation = (%d, %d), want (4, 0)", platformJobs, stageJobs)
	}
}

func TestRunPlatformBuildsRunsConcurrentlyAndPreservesRequestedOrder(t *testing.T) {
	targets := []string{"linux/arm64", "linux/amd64", "linux/ppc64le"}
	started := make(chan int, len(targets))
	release := make(chan struct{})
	type outcome struct {
		results []platformBuild
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		results, err := runPlatformBuildsWithLimit(context.Background(), targets, 2, func(_ context.Context, i int, _ string) (platformBuild, error) {
			started <- i
			<-release
			return platformBuild{selection: imageSelection(i)}, nil
		})
		done <- outcome{results: results, err: err}
	}()

	first := receiveWithin(t, started)
	second := receiveWithin(t, started)
	if first == second {
		t.Fatalf("started duplicate job %d", first)
	}
	select {
	case third := <-started:
		t.Fatalf("started job %d before a worker was released", third)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	result := receiveWithin(t, done)
	if result.err != nil {
		t.Fatal(result.err)
	}
	if len(result.results) != len(targets) {
		t.Fatalf("got %d results, want %d", len(result.results), len(targets))
	}
	for i := range result.results {
		if got := result.results[i].selection.ImageID; got != fmt.Sprintf("image-%d", i) {
			t.Fatalf("result %d image ID = %q", i, got)
		}
	}
}

func TestRunPlatformBuildsCancelsAndJoinsSiblingsOnFirstError(t *testing.T) {
	const active = 3
	targets := []string{"linux/amd64", "linux/arm64", "linux/ppc64le", "linux/s390x"}
	started := make(chan int, len(targets))
	exited := make(chan int, len(targets)-1)
	allStarted := make(chan struct{})
	var startMu sync.Mutex
	startCount := 0
	want := errors.New("platform failed")

	results, err := runPlatformBuildsWithLimit(context.Background(), targets, active, func(ctx context.Context, i int, _ string) (platformBuild, error) {
		started <- i
		startMu.Lock()
		startCount++
		if startCount == active {
			close(allStarted)
		}
		startMu.Unlock()
		<-allStarted
		if i == 1 {
			return platformBuild{}, want
		}
		<-ctx.Done()
		exited <- i
		return platformBuild{}, ctx.Err()
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("ordinary platform failure acquired canceled-sibling classification: %v", err)
	}
	if results != nil {
		t.Fatalf("results = %+v, want nil after failure", results)
	}
	for range active {
		if i := receiveWithin(t, started); i >= active {
			t.Fatalf("queued platform %d started after a sibling failed", i)
		}
	}
	for i := 0; i < active-1; i++ {
		receiveWithin(t, exited)
	}
	select {
	case i := <-started:
		t.Fatalf("queued platform %d executed after cancellation", i)
	default:
	}
}

func TestLockedWriterSerializesConcurrentWrites(t *testing.T) {
	var output bytes.Buffer
	var outputMu sync.Mutex
	writer := lockedWriter{mu: &outputMu, w: &output}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = fmt.Fprintf(writer, "%02d\n", i)
		}(i)
	}
	wg.Wait()
	if lines := bytes.Count(output.Bytes(), []byte{'\n'}); lines != 16 {
		t.Fatalf("got %d complete output lines, want 16: %q", lines, output.String())
	}
}

func imageSelection(i int) oci.StoredSelection {
	return oci.StoredSelection{ImageID: fmt.Sprintf("image-%d", i)}
}

func receiveWithin[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for concurrent build")
		var zero T
		return zero
	}
}

func TestPlatformSiblingIncompleteWaitRetainsFrontendStaging(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	layout, cleanup, err := stageLayout(filepath.Join(t.TempDir(), "definition.coopr"))
	if err != nil {
		t.Fatal(err)
	}
	ancestor := filepath.Dir(layout)
	t.Cleanup(func() { _ = os.RemoveAll(ancestor) })
	primary := errors.New("first platform failed")
	sibling := fmt.Errorf("canceled platform could not drain: %w", buildah.ErrWorkerWaitIncomplete)
	started := make(chan struct{})
	run := func() (retErr error) {
		defer func() { retErr = cleanupBuildFiles(filepath.Dir(layout), cleanup, retErr) }()
		_, retErr = runPlatformBuildsWithLimit(context.Background(), []string{"linux/amd64", "linux/arm64"}, 2, func(ctx context.Context, i int, _ string) (platformBuild, error) {
			if i == 0 {
				<-started
				return platformBuild{}, primary
			}
			close(started)
			<-ctx.Done()
			return platformBuild{}, sibling
		})
		return retErr
	}
	resultErr := run()
	if !errors.Is(resultErr, primary) || !errors.Is(resultErr, sibling) || !errors.Is(resultErr, buildah.ErrWorkerWaitIncomplete) {
		t.Errorf("lost sibling cleanup failure: %v", resultErr)
	}
	if _, err := os.Stat(ancestor); err != nil {
		t.Fatalf("sibling's retained evidence ancestor deleted: %v", err)
	}
}

type nonComparablePlatformFailure []string

func (failure nonComparablePlatformFailure) Error() string { return strings.Join(failure, " ") }

func TestPlatformFailuresDoNotRequireComparableErrors(t *testing.T) {
	started := make(chan struct{})
	_, err := runPlatformBuildsWithLimit(context.Background(), []string{"linux/amd64", "linux/arm64"}, 2, func(ctx context.Context, i int, _ string) (platformBuild, error) {
		if i == 0 {
			<-started
			return platformBuild{}, nonComparablePlatformFailure{"noncomparable", "primary"}
		}
		close(started)
		<-ctx.Done()
		return platformBuild{}, buildah.ErrWorkerWaitIncomplete
	})
	if err == nil || !strings.Contains(err.Error(), "noncomparable primary") || !errors.Is(err, buildah.ErrWorkerWaitIncomplete) {
		t.Fatalf("lost noncomparable primary/sibling errors: %v", err)
	}
}
