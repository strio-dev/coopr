package buildah

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestOperationProgressBufferKeepsCompletionOfAdmittedPhases(t *testing.T) {
	buffer := newOperationProgressBuffer()
	buffer.limit = 2
	start := time.Now()
	for _, id := range []string{"a", "b", "c"} {
		buffer.record(OperationProgress{ID: id, Phase: "execute", State: "started", Started: start})
	}
	completed := start.Add(time.Second)
	for _, id := range []string{"b", "c", "a"} {
		buffer.record(OperationProgress{ID: id, Phase: "execute", State: "completed", Started: start, Completed: &completed})
	}
	events, dropped := buffer.snapshot()
	if dropped != 1 || len(events) != 4 {
		t.Fatalf("snapshot has %d events/%d dropped phases, want 4/1", len(events), dropped)
	}
	for i, want := range []string{"a/started", "b/started", "b/completed", "a/completed"} {
		if got := events[i].ID + "/" + events[i].State; got != want {
			t.Fatalf("event %d = %q, want %q", i, got, want)
		}
	}
	if len(buffer.admitted) != 2 {
		t.Fatalf("buffer retained %d phase identities after reaching its cap", len(buffer.admitted))
	}
}

func TestOperationProgressBufferIgnoresDuplicateAndOrphanTransitions(t *testing.T) {
	buffer := newOperationProgressBuffer()
	started, completed := time.Now(), time.Now()
	begin := OperationProgress{ID: "a", Phase: "restore", State: "started", Started: started}
	end := OperationProgress{ID: "a", Phase: "restore", State: "completed", Started: started, Completed: &completed}
	buffer.record(end)
	buffer.record(begin)
	buffer.record(begin)
	buffer.record(end)
	buffer.record(end)
	events, dropped := buffer.snapshot()
	if len(events) != 2 || dropped != 0 {
		t.Fatalf("duplicate/orphan transitions changed the capture: %+v dropped=%d", events, dropped)
	}
}

func TestOperationProgressBufferCopiesMutableCompletionTimes(t *testing.T) {
	buffer := newOperationProgressBuffer()
	started := time.Now()
	completed := started.Add(time.Second)
	buffer.record(OperationProgress{ID: "a", State: "started", Started: started})
	buffer.record(OperationProgress{ID: "a", State: "completed", Started: started, Completed: &completed})
	want := completed
	completed = completed.Add(time.Hour)
	events, _ := buffer.snapshot()
	if *events[1].Completed != want {
		t.Fatal("caller mutated the captured completion time")
	}
	*events[1].Completed = completed
	events[0].ID = "changed"
	again, _ := buffer.snapshot()
	if again[0].ID != "a" || *again[1].Completed != want {
		t.Fatal("snapshot consumer mutated the retained events")
	}
}

func TestOperationProgressBufferDefaultCapAndChildTruncation(t *testing.T) {
	buffer := newOperationProgressBuffer()
	for index := range operationProgressPhaseLimit + 3 {
		id := fmt.Sprint(index)
		buffer.record(OperationProgress{ID: id, State: "started"})
		completed := time.Now()
		buffer.record(OperationProgress{ID: id, State: "completed", Completed: &completed})
	}
	buffer.addDropped(7)
	events, dropped := buffer.snapshot()
	if len(events) != 2*operationProgressPhaseLimit || dropped != 10 {
		t.Fatalf("default cap has %d events/%d dropped, want %d/10", len(events), dropped, 2*operationProgressPhaseLimit)
	}
	buffer.addDropped(^uint64(0))
	_, dropped = buffer.snapshot()
	if dropped != ^uint64(0) {
		t.Fatalf("truncation count wrapped to %d", dropped)
	}
}

func TestOperationProgressBufferConcurrentProducersAndSnapshots(t *testing.T) {
	buffer := newOperationProgressBuffer()
	var group sync.WaitGroup
	for range 40 {
		group.Go(func() {
			_, finish := startOperationProgress(t.Context(), buffer.record, OperationProgress{Phase: "execute"})
			_, _ = buffer.snapshot()
			finish("miss", nil)
		})
	}
	group.Wait()
	events, dropped := buffer.snapshot()
	if len(events) != 80 || dropped != 0 {
		t.Fatalf("concurrent capture has %d events/%d dropped, want 80/0", len(events), dropped)
	}
	states := make(map[string][]string)
	for _, event := range events {
		states[event.ID] = append(states[event.ID], event.State)
	}
	if len(states) != 40 {
		t.Fatalf("concurrent phases had %d distinct IDs, want 40", len(states))
	}
	for id, sequence := range states {
		if len(sequence) != 2 || sequence[0] != "started" || sequence[1] != "completed" {
			t.Fatalf("phase %s lifecycle differs: %v", id, sequence)
		}
	}
}

func TestOperationProgressBufferBoundsLabelsAndRejectsOversizeIDs(t *testing.T) {
	buffer := newOperationProgressBuffer()
	buffer.record(OperationProgress{ID: string(make([]byte, operationProgressIDLimit+1)), State: "started"})
	buffer.record(OperationProgress{State: "started"})
	buffer.record(OperationProgress{ID: "valid", State: "started", Stage: string(make([]byte, 2*operationProgressTextLimit))})
	events, dropped := buffer.snapshot()
	if len(events) != 1 || dropped != 2 || len(events[0].Stage) > operationProgressTextLimit {
		t.Fatalf("unbounded input accepted: events=%+v dropped=%d", events, dropped)
	}
}
