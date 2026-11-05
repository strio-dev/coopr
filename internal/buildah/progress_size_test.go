package buildah

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkerResultTraceCannotDisplaceRequiredPayload(t *testing.T) {
	started := time.Now().UTC()
	completed := started.Add(time.Millisecond)
	var events []OperationProgress
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		start := OperationProgress{ID: id, Stage: strings.Repeat("s", 1024), Phase: "execute", State: "started", Started: started}
		end := start
		end.State, end.Completed, end.DurationNanos = "completed", &completed, int64(time.Millisecond)
		events = append(events, start, end)
	}
	required := Result{Reference: strings.Repeat("x", workerMaxJSON-4096)}
	for _, kind := range []string{"plan", "stage"} {
		t.Run(kind, func(t *testing.T) {
			var message any = planWorkerResponse{Result: required, Progress: events, ProgressDropped: 3}
			if kind == "stage" {
				message = stageWorkerResponse{Result: required, Progress: events, ProgressDropped: 3}
			}
			encoded, err := json.Marshal(message)
			if err != nil || len(encoded) <= workerMaxJSON {
				t.Fatalf("boundary fixture does not exceed the result limit: size=%d err=%v", len(encoded), err)
			}
			path := filepath.Join(t.TempDir(), "result.json")
			if err := writeWorkerJSON(path, message); err != nil {
				t.Fatalf("optional trace caused required result failure: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil || len(data) > workerMaxJSON {
				t.Fatalf("bounded response size=%d err=%v", len(data), err)
			}
			var read planWorkerResponse
			if err := json.Unmarshal(data, &read); err != nil {
				t.Fatal(err)
			}
			if read.Result.Reference != required.Reference || len(read.Progress) != 0 || read.ProgressDropped != 8 || read.Result.ProgressDropped != 8 {
				t.Fatalf("trace reduction lost result data or counts: trace=%d dropped=%d nested=%d", len(read.Progress), read.ProgressDropped, read.Result.ProgressDropped)
			}
		})
	}
	if err := writeWorkerJSON(filepath.Join(t.TempDir(), "too-large.json"), planWorkerResponse{Result: Result{Reference: required.Reference + strings.Repeat("x", 8192)}}); err == nil {
		t.Fatal("oversized required payload was accepted")
	}
}
