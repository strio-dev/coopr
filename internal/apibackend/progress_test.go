package apibackend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/strio-dev/coopr/api"
	"github.com/strio-dev/coopr/internal/apiservice"
)

func TestMaximumNativeTraceDoesNotOverflowTerminalResult(t *testing.T) {
	started := time.Now().UTC()
	completed := started.Add(time.Millisecond)
	events := make([]api.OperationProgress, 0, 2048)
	for i := range 1024 {
		start := api.OperationProgress{ID: fmt.Sprint(i), Stage: strings.Repeat("s", 1024), Instruction: "copy", Phase: "prepare", State: "started", Started: started}
		finish := start
		finish.State, finish.Completed, finish.DurationNanos = "completed", &completed, int64(time.Millisecond)
		events = append(events, start, finish)
	}
	data, err := json.Marshal(map[string]any{"coopr.operation.progress": events, "coopr.operation.progress.dropped": 7, "containerimage.digest": "sha256:native"})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < api.MaxEventBytes {
		t.Fatal("boundary fixture is smaller than the terminal limit")
	}
	server := apiservice.New(t.Context(), "test", func(ctx context.Context, _ api.Request, _, _ io.Writer) (api.Result, error) {
		for _, event := range events {
			api.ProgressObserver(ctx)(event)
		}
		result := api.Result{ImageID: "native", ProgressDropped: 7}
		if err := applyNativeResultMetadata(&result, data); err != nil {
			return api.Result{}, err
		}
		return result, nil
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/v1/executions/"+strings.Repeat("b", 32), strings.NewReader(`{"kind":"build","options":{}}`))
	server.ServeHTTP(recorder, request)
	decoder := json.NewDecoder(recorder.Body)
	progress, terminals := 0, 0
	for {
		var event api.Event
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if event.Type == "progress" {
			progress++
			continue
		}
		terminals++
		if event.Type != "result" || event.Error != nil || event.Result == nil || event.Result.ImageID != "native" || event.Result.ProgressDropped != 7 {
			t.Fatalf("valid trace changed the successful terminal outcome: %+v", event)
		}
		if _, exists := event.Result.Metadata["coopr.operation.progress"]; exists {
			t.Fatal("terminal result duplicated the streamed trace")
		}
		if string(event.Result.Metadata["containerimage.digest"]) != `"sha256:native"` {
			t.Fatal("terminal result lost native artifact metadata")
		}
	}
	if progress != len(events) || terminals != 1 {
		t.Fatalf("got %d observations and %d terminal events", progress, terminals)
	}
}
