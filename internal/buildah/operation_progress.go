package buildah

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"unicode/utf8"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/buildkit/util/progress/controller"
	digest "github.com/opencontainers/go-digest"
	"github.com/strio-dev/coopr/api"
)

// OperationProgress uses Coopr's wire type; BuildKit's progress values stay
// private to this adapter and never become part of Coopr's public protocol.
type OperationProgress = api.OperationProgress

// OperationProgressFunc receives each transition synchronously. The caller
// owns serialization when sharing a sink across concurrent execution workers.
type OperationProgressFunc func(OperationProgress)

const operationProgressTextLimit = 1024

// startOperationProgress reuses BuildKit's measured phase lifecycle with a
// direct writer. Unlike a buffered progress reader or replaying MultiWriter,
// this writer retains no event history and has no collector goroutine.
func startOperationProgress(ctx context.Context, sink OperationProgressFunc, attributes OperationProgress) (context.Context, func(string, error)) {
	if sink == nil {
		return ctx, finishNoOperationProgress
	}
	writer := &operationProgressWriter{
		sink: sink,
		attributes: OperationProgress{
			Stage:       operationProgressText(attributes.Stage),
			Operation:   attributes.Operation,
			Instruction: operationProgressText(attributes.Instruction),
			Phase:       operationProgressText(attributes.Phase),
		},
		state: "completed",
	}
	phase := &controller.Controller{
		// This display identity does not participate in cache identity. The
		// controller supplies a distinct transition ID for each phase instance.
		Digest: digest.FromString(writer.attributes.Stage + "\x00" + strconv.Itoa(attributes.Operation) + "\x00" + writer.attributes.Instruction + "\x00" + writer.attributes.Phase),
		Name:   writer.attributes.Phase,
		WriterFactory: func(parent context.Context) (progress.Writer, bool, context.Context) {
			return writer, true, parent
		},
	}
	_, complete := phase.Start(ctx)
	var once sync.Once
	return ctx, func(cache string, err error) {
		once.Do(func() {
			writer.cache = operationProgressText(cache)
			if err != nil {
				writer.state = "failed"
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
					writer.state = "canceled"
				}
			}
			complete(err)
		})
	}
}

func finishNoOperationProgress(string, error) {}

type operationProgressWriter struct {
	sink       OperationProgressFunc
	attributes OperationProgress
	cache      string
	state      string
}

func (writer *operationProgressWriter) Write(id string, value any) error {
	vertex, ok := value.(client.Vertex)
	if !ok || vertex.Started == nil {
		return nil
	}
	event := writer.attributes
	event.ID = id
	event.State = "started"
	event.Started = *vertex.Started
	if vertex.Completed != nil {
		completed := *vertex.Completed
		event.Completed = &completed
		event.State = writer.state
		event.Cache = writer.cache
		event.DurationNanos = completed.Sub(event.Started).Nanoseconds()
		event.Error = operationProgressText(vertex.Error)
	}
	writer.sink(event)
	return nil
}

func (*operationProgressWriter) Close() error { return nil }

func operationProgressText(value string) string {
	value = progressText(value)
	if len(value) <= operationProgressTextLimit {
		return value
	}
	end := operationProgressTextLimit - len("...")
	for !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "..."
}
