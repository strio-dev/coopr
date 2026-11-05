package buildah

import (
	"context"
	"errors"
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

func TestWorkerResponsesPreserveValidatedProgressAndTruncation(t *testing.T) {
	for _, failure := range []string{"", "fixture failure"} {
		t.Run(failure, func(t *testing.T) {
			events := progressProtocolPair(t)
			path := filepath.Join(t.TempDir(), "response.json")
			if err := writeWorkerJSON(path, planWorkerResponse{Progress: events, ProgressDropped: 7, Error: failure}); err != nil {
				t.Fatal(err)
			}
			response, err := readWorkerResponse(path)
			if err != nil || len(response.Progress) != 2 || response.ProgressDropped != 7 || response.Result.ProgressDropped != 7 || response.Error != failure {
				t.Fatalf("plan response lost observations: %+v, %v", response, err)
			}
			if err := writeWorkerJSON(path, stageWorkerResponse{Progress: events, ProgressDropped: 9, Error: failure}); err != nil {
				t.Fatal(err)
			}
			stage, err := readStageWorkerResponse(path)
			if err != nil || len(stage.Progress) != 2 || stage.ProgressDropped != 9 || stage.Result.ProgressDropped != 9 || stage.Error != failure {
				t.Fatalf("stage response lost observations: %+v, %v", stage, err)
			}
		})
	}
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
		{"invalid utf8", func(events []OperationProgress) []OperationProgress { events[0].Stage = "\xff"; return events }},
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
			path := filepath.Join(t.TempDir(), "response.json")
			if err := writeWorkerJSON(path, planWorkerResponse{Progress: events}); err != nil {
				t.Fatal(err)
			}
			if _, err := readWorkerResponse(path); err == nil {
				t.Fatal("plan reader accepted invalid progress")
			}
			if err := writeWorkerJSON(path, stageWorkerResponse{Progress: events}); err != nil {
				t.Fatal(err)
			}
			if _, err := readStageWorkerResponse(path); err == nil {
				t.Fatal("stage reader accepted invalid progress")
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

func TestOperationProgressReplayUsesIndependentValues(t *testing.T) {
	events := progressProtocolPair(t)
	want := *events[1].Completed
	sink, snapshot := CaptureOperationProgress()
	replayOperationProgress(sink, events)
	got, dropped := snapshot()
	if len(got) != 2 || dropped != 0 || got[1].Cache != "hit" {
		t.Fatalf("replay differs: %+v dropped=%d", got, dropped)
	}
	replayOperationProgress(func(event OperationProgress) {
		if event.Completed != nil {
			*event.Completed = time.Time{}
		}
	}, events)
	if *events[1].Completed != want {
		t.Fatal("observer mutated the worker response")
	}
	replayOperationProgress(nil, events)
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
