package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strio-dev/coopr/api"
)

func progressEvent() api.OperationProgress {
	return api.OperationProgress{ID: "phase", Stage: "build", Instruction: "copy", Operation: 2, Phase: "lookup", State: "started", Started: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
}

func TestProgressInterleavesWithoutRequiringAnObserver(t *testing.T) {
	for _, observe := range []bool{false, true} {
		t.Run(fmt.Sprint(observe), func(t *testing.T) {
			started := progressEvent()
			finished := started
			completed := started.Started.Add(-time.Second) // Monotonic duration survives wall-clock changes.
			finished.State, finished.Cache, finished.Completed, finished.DurationNanos = "completed", "hit", &completed, 50
			c := testClient(t, allInfo, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for _, event := range []api.Event{{Type: "stdout", Data: []byte("output\n")}, {Type: "progress", Progress: &started}, {Type: "stderr", Data: []byte("building\n")}, {Type: "progress", Progress: &finished}, {Type: "result", Result: &api.Result{ImageID: "image", ProgressDropped: 7}}} {
					data, err := json.Marshal(event)
					if err != nil {
						t.Error(err)
						return
					}
					// Additive fields in known nested observations remain compatible.
					if event.Type == "progress" {
						data = bytes.Replace(data, []byte(`"phase":"lookup"`), []byte(`"phase":"lookup","future":true`), 1)
					}
					_, _ = w.Write(append(data, '\n'))
				}
			}))
			var stdout, stderr bytes.Buffer
			var events []api.OperationProgress
			options := StreamOptions{Stdout: &stdout, Stderr: &stderr}
			if observe {
				options.Progress = func(event api.OperationProgress) { events = append(events, event) }
			}
			result, err := c.Execute(t.Context(), execution(), options)
			if err != nil || stdout.String() != "output\n" || stderr.String() != "building\n" || result.ProgressDropped != 7 || result.ImageID != "image" {
				t.Fatalf("event delivery changed: stdout=%q stderr=%q result=%+v err=%v", stdout.String(), stderr.String(), result, err)
			}
			if observe && (len(events) != 2 || events[1].Cache != "hit" || events[1].DurationNanos != 50) {
				t.Fatalf("observer order/content changed: %+v", events)
			}
		})
	}
}

func TestInvalidProgressAndCaseAliasesRequireCleanup(t *testing.T) {
	encoded := func(change func(*api.OperationProgress)) string {
		event := progressEvent()
		change(&event)
		data, err := json.Marshal(api.Event{Type: "progress", Progress: &event})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	valid := encoded(func(*api.OperationProgress) {})
	tests := map[string]string{
		"missing payload":                 `{"type":"progress"}`,
		"missing id":                      encoded(func(p *api.OperationProgress) { p.ID = "" }),
		"missing phase":                   encoded(func(p *api.OperationProgress) { p.Phase = " " }),
		"zero start":                      encoded(func(p *api.OperationProgress) { p.Started = time.Time{} }),
		"negative operation":              encoded(func(p *api.OperationProgress) { p.Operation = -1 }),
		"negative duration":               encoded(func(p *api.OperationProgress) { p.DurationNanos = -1 }),
		"unknown state":                   encoded(func(p *api.OperationProgress) { p.State = "unknown" }),
		"started completion":              encoded(func(p *api.OperationProgress) { p.Completed = &p.Started }),
		"started duration":                encoded(func(p *api.OperationProgress) { p.DurationNanos = 5 }),
		"started cache":                   encoded(func(p *api.OperationProgress) { p.Cache = "hit" }),
		"started error":                   encoded(func(p *api.OperationProgress) { p.Error = "error" }),
		"missing completion":              encoded(func(p *api.OperationProgress) { p.State = "completed" }),
		"zero completion":                 encoded(func(p *api.OperationProgress) { p.State = "completed"; zero := time.Time{}; p.Completed = &zero }),
		"successful error":                encoded(func(p *api.OperationProgress) { p.State, p.Completed, p.Error = "completed", &p.Started, "failure" }),
		"oversized label":                 encoded(func(p *api.OperationProgress) { p.Stage = strings.Repeat("x", 1025) }),
		"control label":                   encoded(func(p *api.OperationProgress) { p.Stage = "first\nsecond" }),
		"mixed empty data":                strings.TrimSuffix(valid, "}") + `,"data":""}`,
		"mixed differently cased payload": `{"type":"stdout","Progress":{"id":"hidden"}}`,
		"type alias":                      `{"type":"stdout","TYPE":"result","result":{}}`,
		"unicode alias":                   `{"type":"stdout","reſult":{}}`,
		"nested state alias":              strings.Replace(valid, `"state":"started"`, `"state":"started","STATE":"completed"`, 1),
		"nested error alias":              `{"type":"result","error":{"code":"cleanup_failed","Code":"execution_failed"}}`,
		"progress after terminal":         `{"type":"result","result":{}}` + "\n" + valid,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			var acknowledged atomic.Bool
			c := testClient(t, allInfo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					acknowledged.Store(true)
					acknowledge(t, w, r)
					return
				}
				_, _ = io.WriteString(w, body)
			}))
			if _, err := c.Execute(t.Context(), execution(), StreamOptions{}); err == nil || !acknowledged.Load() {
				t.Fatalf("invalid observation accepted or cleanup skipped: %v", err)
			}
		})
	}
}

func TestProgressCallbackCancellationWaitsForCleanup(t *testing.T) {
	var acknowledged atomic.Bool
	progress := progressEvent()
	c := testClient(t, allInfo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			acknowledged.Store(true)
			acknowledge(t, w, r)
			return
		}
		encode(t, w, api.Event{Type: "progress", Progress: &progress})
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := c.Execute(ctx, execution(), StreamOptions{Progress: func(api.OperationProgress) { cancel() }})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrCleanupFailed) || !acknowledged.Load() {
		t.Fatalf("callback cancellation lost acknowledgement: %v", err)
	}
}

func TestTerminalCleanupErrorAndTruncatedTerminalRemainDistinct(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(fmt.Sprint(truncated), func(t *testing.T) {
			var deletes atomic.Int32
			c := testClient(t, allInfo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deletes.Add(1)
					acknowledge(t, w, r)
					return
				}
				data := `{"type":"result","result":{"imageID":"partial"},"error":{"code":"cleanup_failed","message":"worker still alive"}}` + "\n"
				if truncated {
					w.Header().Set("Content-Length", fmt.Sprint(len(data)+10))
				}
				_, _ = io.WriteString(w, data)
			}))
			result, err := c.Execute(t.Context(), execution(), StreamOptions{})
			var remote *api.Error
			if !errors.Is(err, ErrCleanupFailed) || errors.Is(err, context.Canceled) || !errors.As(err, &remote) || remote.Code != "cleanup_failed" || result.ImageID != "partial" || deletes.Load() != map[bool]int32{false: 0, true: 1}[truncated] {
				t.Fatalf("terminal/transport classification changed: result=%+v deletes=%d err=%v", result, deletes.Load(), err)
			}
		})
	}
}
