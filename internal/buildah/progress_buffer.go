package buildah

import (
	"fmt"
	"sync"
	"unicode/utf8"
)

const (
	operationProgressPhaseLimit = 1024
	operationProgressIDLimit    = 128
)

// operationProgressBuffer admits bounded phase pairs for live worker telemetry
// or direct metadata capture. Once full, new phases are omitted but completions belonging
// to admitted phases still fit. It never retains rejected identities.
type operationProgressBuffer struct {
	mu       sync.Mutex
	limit    int
	admitted map[string]bool
	events   []OperationProgress
	dropped  uint64
	// emit runs under mu, preserving transition order for the private live stream.
	emit OperationProgressFunc
}

func newOperationProgressBuffer() *operationProgressBuffer {
	return &operationProgressBuffer{limit: operationProgressPhaseLimit}
}

// CaptureOperationProgress provides bounded observation capture for callers
// writing an existing metadata report. Snapshot returns independent values.
func CaptureOperationProgress() (OperationProgressFunc, func() ([]OperationProgress, uint64)) {
	buffer := newOperationProgressBuffer()
	return buffer.record, buffer.snapshot
}

func (buffer *operationProgressBuffer) record(event OperationProgress) {
	if buffer == nil {
		return
	}
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	if event.ID == "" || len(event.ID) > operationProgressIDLimit {
		if event.State == "started" {
			buffer.addDroppedLocked(1)
		}
		return
	}
	completed, known := buffer.admitted[event.ID]
	if known {
		if completed || event.Completed == nil || event.State == "started" {
			return
		}
		buffer.admitted[event.ID] = true
	} else {
		if event.State != "started" || event.Completed != nil {
			return
		}
		if len(buffer.admitted) >= buffer.limit {
			buffer.addDroppedLocked(1)
			return
		}
		if buffer.admitted == nil {
			buffer.admitted = make(map[string]bool)
		}
		buffer.admitted[event.ID] = false
	}
	event.Stage = operationProgressText(event.Stage)
	event.Instruction = operationProgressText(event.Instruction)
	event.Phase = operationProgressText(event.Phase)
	event.Cache = operationProgressText(event.Cache)
	event.Error = operationProgressText(event.Error)
	if buffer.emit != nil {
		buffer.emit(cloneOperationProgress(event))
	} else {
		buffer.events = append(buffer.events, cloneOperationProgress(event))
	}
}

func (buffer *operationProgressBuffer) addDropped(count uint64) {
	if buffer == nil {
		return
	}
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	buffer.addDroppedLocked(count)
}

func (buffer *operationProgressBuffer) addDroppedLocked(count uint64) {
	if ^uint64(0)-buffer.dropped < count {
		buffer.dropped = ^uint64(0)
		return
	}
	buffer.dropped += count
}

func (buffer *operationProgressBuffer) snapshot() ([]OperationProgress, uint64) {
	if buffer == nil {
		return nil, 0
	}
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	if len(buffer.events) == 0 {
		return nil, buffer.dropped
	}
	events := make([]OperationProgress, len(buffer.events))
	for i, event := range buffer.events {
		events[i] = cloneOperationProgress(event)
	}
	return events, buffer.dropped
}

func cloneOperationProgress(event OperationProgress) OperationProgress {
	if event.Completed != nil {
		completed := *event.Completed
		event.Completed = &completed
	}
	return event
}

type operationProgressPhase struct {
	start     OperationProgress
	completed bool
}

type operationProgressValidator struct {
	phases map[string]operationProgressPhase
	count  int
}

func (validator *operationProgressValidator) accept(event OperationProgress) error {
	index := validator.count
	if index >= 2*operationProgressPhaseLimit {
		return fmt.Errorf("phase event count exceeds %d", 2*operationProgressPhaseLimit)
	}
	if validator.phases == nil {
		validator.phases = make(map[string]operationProgressPhase)
	}
	if event.ID == "" || len(event.ID) > operationProgressIDLimit || !utf8.ValidString(event.ID) || operationProgressText(event.ID) != event.ID {
		return fmt.Errorf("event %d has an invalid phase ID", index)
	}
	if event.Phase == "" || event.Operation < 0 || event.Started.IsZero() {
		return fmt.Errorf("event %d lacks valid phase attribution", index)
	}
	for _, value := range []string{event.Stage, event.Instruction, event.Phase, event.Cache, event.Error} {
		if len(value) > operationProgressTextLimit || !utf8.ValidString(value) || operationProgressText(value) != value {
			return fmt.Errorf("event %d has invalid phase text", index)
		}
	}
	previous, known := validator.phases[event.ID]
	if event.State == "started" {
		if known || event.Completed != nil || event.DurationNanos != 0 || event.Cache != "" || event.Error != "" {
			return fmt.Errorf("event %d has an invalid phase start", index)
		}
		if len(validator.phases) >= operationProgressPhaseLimit {
			return fmt.Errorf("phase count exceeds %d", operationProgressPhaseLimit)
		}
		validator.phases[event.ID] = operationProgressPhase{start: event}
		validator.count++
		return nil
	}
	if !known || previous.completed || event.Completed == nil || event.Completed.IsZero() || event.DurationNanos < 0 {
		return fmt.Errorf("event %d has an invalid phase completion", index)
	}
	if event.State != "completed" && event.State != "failed" && event.State != "canceled" {
		return fmt.Errorf("event %d has an unknown terminal phase state", index)
	}
	start := previous.start
	if event.Stage != start.Stage || event.Operation != start.Operation || event.Instruction != start.Instruction || event.Phase != start.Phase || !event.Started.Equal(start.Started) {
		return fmt.Errorf("event %d changes phase attribution", index)
	}
	if event.State == "completed" && event.Error != "" {
		return fmt.Errorf("event %d reports an error on a successful phase", index)
	}
	previous.completed = true
	validator.phases[event.ID] = previous
	validator.count++
	return nil
}
