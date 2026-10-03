package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"coopr/internal/imagecatalog"
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
	targets := []string{"linux/amd64", "linux/arm64", "linux/ppc64le"}
	started := make(chan int, len(targets))
	exited := make(chan int, len(targets)-1)
	allStarted := make(chan struct{})
	var startMu sync.Mutex
	startCount := 0
	want := errors.New("platform failed")

	results, err := runPlatformBuildsWithLimit(context.Background(), targets, len(targets), func(ctx context.Context, i int, _ string) (platformBuild, error) {
		started <- i
		startMu.Lock()
		startCount++
		if startCount == len(targets) {
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
	if results != nil {
		t.Fatalf("results = %+v, want nil after failure", results)
	}
	for range targets {
		receiveWithin(t, started)
	}
	for i := 0; i < len(targets)-1; i++ {
		receiveWithin(t, exited)
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

func imageSelection(i int) imagecatalog.Selection {
	return imagecatalog.Selection{ImageID: fmt.Sprintf("image-%d", i)}
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
