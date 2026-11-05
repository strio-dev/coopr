package buildah

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestOperationProgressPairsMeasuredPhaseAndCacheDecision(t *testing.T) {
	var events []OperationProgress
	parent := context.WithValue(context.Background(), operationProgressTestKey{}, "retained")
	ctx, finish := startOperationProgress(parent, func(event OperationProgress) { events = append(events, event) }, OperationProgress{
		Stage: "app", Operation: 2, Instruction: "copy", Phase: "cache_lookup",
	})
	if ctx.Value(operationProgressTestKey{}) != "retained" {
		t.Fatal("progress context discarded caller values")
	}
	finish("hit", nil)
	if len(events) != 2 {
		t.Fatalf("events = %+v, want one start and one completion", events)
	}
	start, complete := events[0], events[1]
	if start.ID == "" || complete.ID != start.ID || start.State != "started" || complete.State != "completed" {
		t.Fatalf("phase identity/lifecycle differs: start=%+v complete=%+v", start, complete)
	}
	for _, event := range events {
		if event.Stage != "app" || event.Operation != 2 || event.Instruction != "copy" || event.Phase != "cache_lookup" || event.Started.IsZero() {
			t.Fatalf("phase attribution differs: %+v", event)
		}
	}
	if start.Completed != nil || start.DurationNanos != 0 || start.Cache != "" || complete.Completed == nil || complete.Cache != "hit" {
		t.Fatalf("phase timing/cache fields differ: start=%+v complete=%+v", start, complete)
	}
	if complete.Started != start.Started || complete.Completed.Before(start.Started) || complete.DurationNanos != complete.Completed.Sub(start.Started).Nanoseconds() {
		t.Fatalf("duration was not derived from paired timestamps: start=%+v complete=%+v", start, complete)
	}
}

type operationProgressTestKey struct{}

func TestOperationProgressCompletionIsExactlyOnce(t *testing.T) {
	var events []OperationProgress
	_, finish := startOperationProgress(context.Background(), func(event OperationProgress) { events = append(events, event) }, OperationProgress{Phase: "execute"})
	var group sync.WaitGroup
	for range 20 {
		group.Go(func() { finish("miss", nil) })
	}
	group.Wait()
	if len(events) != 2 || events[1].State != "completed" || events[1].Cache != "miss" {
		t.Fatalf("concurrent completion duplicated or changed the phase: %+v", events)
	}
}

func TestOperationProgressClassifiesFailureAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name, state string
		err         error
	}{
		{"failure", "failed", errors.New("fixture failure")},
		{"cancel", "canceled", context.Canceled},
		{"deadline", "canceled", context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			var events []OperationProgress
			_, finish := startOperationProgress(context.Background(), func(event OperationProgress) { events = append(events, event) }, OperationProgress{Phase: "execute"})
			finish("", test.err)
			if len(events) != 2 || events[1].State != test.state || events[1].Error != test.err.Error() || events[1].Completed == nil {
				t.Fatalf("terminal phase differs: %+v", events)
			}
		})
	}
}

func TestOperationProgressRetainsCancellationWithNativeExitError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var events []OperationProgress
	_, finish := startOperationProgress(ctx, func(event OperationProgress) { events = append(events, event) }, OperationProgress{Phase: "execute"})
	cancel()
	finish("", errors.New("native exit status 1"))
	if len(events) != 2 || events[1].State != "canceled" || events[1].Error != "native exit status 1" {
		t.Fatalf("native exit discarded the cancellation scope: %+v", events)
	}
}

func TestOperationProgressBoundsUntrustedLabelsAndErrors(t *testing.T) {
	var events []OperationProgress
	label := strings.Repeat("界", 1000) + "\nforged"
	_, finish := startOperationProgress(context.Background(), func(event OperationProgress) { events = append(events, event) }, OperationProgress{
		Stage: label, Instruction: label, Phase: label,
	})
	finish(label, errors.New(label))
	for _, event := range events {
		for _, value := range []string{event.Stage, event.Instruction, event.Phase, event.Cache, event.Error} {
			if len(value) > operationProgressTextLimit || !utf8.ValidString(value) || strings.ContainsAny(value, "\n\r\x1b") {
				t.Fatalf("unbounded or unsafe progress text: %q", value)
			}
		}
	}
}

func TestOperationProgressNilSinkHasNoObserverWork(t *testing.T) {
	ctx := context.Background()
	allocs := testing.AllocsPerRun(100, func() {
		got, finish := startOperationProgress(ctx, nil, OperationProgress{Phase: "execute"})
		if got != ctx {
			t.Fatal("disabled observer replaced the caller context")
		}
		finish("miss", nil)
	})
	if allocs != 0 {
		t.Fatalf("disabled observer allocated %g objects", allocs)
	}
}

func TestOperationProgressDeliveryIsSynchronous(t *testing.T) {
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_, finish := startOperationProgress(context.Background(), func(event OperationProgress) {
			if event.State == "started" {
				close(entered)
				<-release
			}
		}, OperationProgress{Phase: "restore"})
		finish("hit", nil)
		close(returned)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("observer did not enter its sink")
	}
	select {
	case <-returned:
		t.Fatal("observer queued events instead of respecting sink backpressure")
	default:
	}
	close(release)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("observer did not finish after sink release")
	}
}

func BenchmarkOperationProgressPhase(b *testing.B) {
	ctx := context.Background()
	for _, test := range []struct {
		name string
		sink OperationProgressFunc
	}{{"disabled", nil}, {"observed", func(OperationProgress) {}}} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, finish := startOperationProgress(ctx, test.sink, OperationProgress{Stage: "app", Operation: 2, Instruction: "copy", Phase: "cache_lookup"})
				finish("hit", nil)
			}
		})
	}
}
