package apiservice

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/strio-dev/coopr/api"
)

func TestOperationProgressPrecedesTerminalResult(t *testing.T) {
	started := time.Now().UTC()
	completed := started.Add(time.Millisecond)
	server := New(t.Context(), "test", func(ctx context.Context, _ api.Request, _, stderr io.Writer) (api.Result, error) {
		observer := api.ProgressObserver(ctx)
		if observer == nil {
			t.Fatal("backend did not receive the invocation observer")
		}
		observer(api.OperationProgress{ID: "copy", Instruction: "copy", Phase: "lookup", State: "started", Started: started})
		if _, err := io.WriteString(stderr, "ordinary log\n"); err != nil {
			return api.Result{}, err
		}
		observer(api.OperationProgress{ID: "copy", Instruction: "copy", Phase: "lookup", State: "completed", Cache: "hit", Started: started, Completed: &completed, DurationNanos: int64(time.Millisecond)})
		return api.Result{ImageID: "result", ProgressDropped: 3}, nil
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/v1/executions/"+strings.Repeat("a", 32), strings.NewReader(`{"kind":"execute","options":{}}`))
	server.ServeHTTP(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("HTTP %d: %s", recorder.Code, recorder.Body)
	}
	decoder := json.NewDecoder(recorder.Body)
	want := []string{"progress", "stderr", "progress", "result"}
	for _, kind := range want {
		var event api.Event
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event.Type != kind {
			t.Fatalf("event type %q, want %q", event.Type, kind)
		}
		if kind == "progress" && (event.Progress == nil || event.Progress.ID != "copy" || event.Result != nil || event.Error != nil) {
			t.Fatalf("invalid structured observation: %+v", event)
		}
		if kind == "result" && (event.Result == nil || event.Result.ProgressDropped != 3) {
			t.Fatalf("terminal result lost truncation count: %+v", event)
		}
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatalf("unexpected trailing event: %v", err)
	}
}
