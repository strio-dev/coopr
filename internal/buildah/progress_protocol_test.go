package buildah

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func progressProtocolPair(t *testing.T) []OperationProgress {
	t.Helper()
	var events []OperationProgress
	_, complete := startOperationProgress(context.Background(), func(event OperationProgress) { events = append(events, event) }, OperationProgress{
		Stage: "producer", Instruction: "copy", Operation: 1, Phase: "cache_lookup",
	})
	complete("hit", nil)
	return events
}

func TestOperationProgressProtocolRejectsInvalidBatches(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func([]OperationProgress) []OperationProgress
	}{
		{"orphan completion", func(events []OperationProgress) []OperationProgress { return events[1:] }},
		{"duplicate start", func(events []OperationProgress) []OperationProgress { return []OperationProgress{events[0], events[0]} }},
		{"duplicate completion", func(events []OperationProgress) []OperationProgress { return append(events, events[1]) }},
		{"empty id", func(events []OperationProgress) []OperationProgress { events[0].ID = ""; return events }},
		{"long id", func(events []OperationProgress) []OperationProgress {
			events[0].ID = strings.Repeat("x", operationProgressIDLimit+1)
			return events
		}},
		{"empty phase", func(events []OperationProgress) []OperationProgress { events[0].Phase = ""; return events }},
		{"negative operation", func(events []OperationProgress) []OperationProgress { events[0].Operation = -1; return events }},
		{"missing start time", func(events []OperationProgress) []OperationProgress { events[0].Started = time.Time{}; return events }},
		{"invalid utf8", func(events []OperationProgress) []OperationProgress {
			events[0].Stage, events[1].Stage = "\xff", "\xff"
			return events
		}},
		{"control text", func(events []OperationProgress) []OperationProgress {
			events[0].Instruction = "copy\nforged"
			return events
		}},
		{"long text", func(events []OperationProgress) []OperationProgress {
			events[0].Stage = strings.Repeat("x", operationProgressTextLimit+1)
			return events
		}},
		{"cache on start", func(events []OperationProgress) []OperationProgress { events[0].Cache = "hit"; return events }},
		{"duration on start", func(events []OperationProgress) []OperationProgress { events[0].DurationNanos = 1; return events }},
		{"changed stage", func(events []OperationProgress) []OperationProgress { events[1].Stage = "other"; return events }},
		{"changed operation", func(events []OperationProgress) []OperationProgress { events[1].Operation++; return events }},
		{"changed phase", func(events []OperationProgress) []OperationProgress { events[1].Phase = "execute"; return events }},
		{"changed start", func(events []OperationProgress) []OperationProgress {
			events[1].Started = events[0].Started.Add(time.Second)
			return events
		}},
		{"negative duration", func(events []OperationProgress) []OperationProgress { events[1].DurationNanos = -1; return events }},
		{"missing completion time", func(events []OperationProgress) []OperationProgress { events[1].Completed = nil; return events }},
		{"unknown state", func(events []OperationProgress) []OperationProgress { events[1].State = "unknown"; return events }},
		{"error on success", func(events []OperationProgress) []OperationProgress { events[1].Error = "failure"; return events }},
		{"event cap", func(events []OperationProgress) []OperationProgress {
			for len(events) <= 2*operationProgressPhaseLimit {
				events = append(events, events[0])
			}
			return events
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := test.edit(progressProtocolPair(t))
			if err := validateOperationProgressBatch(events); err == nil {
				t.Fatal("accepted invalid progress batch")
			}
		})
	}
}

func TestOperationProgressProtocolRetainsMonotonicDurationAcrossClockAdjustment(t *testing.T) {
	events := progressProtocolPair(t)
	backward := events[0].Started.Add(-time.Second)
	events[1].Completed = &backward
	events[1].DurationNanos = time.Second.Nanoseconds()
	if err := validateOperationProgressBatch(events); err != nil {
		t.Fatalf("positive worker-measured duration rejected after wall-clock adjustment: %v", err)
	}
	if err := validateOperationProgressBatch(events[:1]); err != nil {
		t.Fatalf("partial phase capture rejected: %v", err)
	}
}

func TestOperationProgressProtocolAcceptsFailureAndCancellation(t *testing.T) {
	for _, failure := range []error{errors.New("fixture failure"), context.Canceled, context.DeadlineExceeded} {
		var events []OperationProgress
		_, finish := startOperationProgress(t.Context(), func(event OperationProgress) { events = append(events, event) }, OperationProgress{Phase: "execute"})
		finish("miss", failure)
		if err := validateOperationProgressBatch(events); err != nil {
			t.Fatalf("valid terminal capture rejected: %v", err)
		}
	}
}

func TestMissingProgressSummaryDrainsAndClosesLiveReceiver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	receiver, err := newWorkerProgressReceiver(path, func(OperationProgress) {})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = receiver.close() }()
	if err := receiver.finish(nil); err == nil {
		t.Fatal("missing live summary accepted")
	}
	select {
	case <-receiver.done:
	default:
		t.Fatal("invalid summary left receiver polling")
	}
	if _, err := receiver.file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("invalid summary left live file open: %v", err)
	}
}

func validateOperationProgressBatch(events []OperationProgress) error {
	if len(events) > 2*operationProgressPhaseLimit {
		return fmt.Errorf("phase event count exceeds %d", 2*operationProgressPhaseLimit)
	}
	validator := operationProgressValidator{}
	for _, event := range events {
		if err := validator.accept(event); err != nil {
			return err
		}
	}
	return nil
}
