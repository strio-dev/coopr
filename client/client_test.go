package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strio-dev/coopr/api"
)

func testClient(t *testing.T, info string, handler http.Handler) *Client {
	t.Helper()
	directory, err := os.MkdirTemp("", "coopr-client-")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(directory, "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/info" && info != "" {
			_, _ = io.WriteString(w, info)
			return
		}
		handler.ServeHTTP(w, r)
	})}
	go func() { _ = server.Serve(listener) }()
	c, err := New(socket, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.CloseIdleConnections()
		_ = server.Close()
		_ = os.RemoveAll(directory)
	})
	return c
}

const allInfo = `{"apiVersion":1,"version":"development-server","capabilities":["execute","build","component","publish","registry-cache","named-contexts","operation-progress"]}`

func execution() api.Request {
	return api.Request{Kind: "execute", Program: &api.Program{Output: "out", States: []api.State{{ID: "out", From: api.Source{Image: "scratch"}}}}}
}

func encode(t *testing.T, w io.Writer, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func acknowledge(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	encode(t, w, api.Status{ID: id, State: "canceled"})
}

func TestCompatibilityMatrixAndCapabilityPreflight(t *testing.T) {
	for _, version := range []string{"older-development", "0.6.0", "9.4.7"} {
		t.Run(version, func(t *testing.T) {
			var posts atomic.Int32
			info := fmt.Sprintf(`{"apiVersion":1,"version":%q,"capabilities":["execute","future-capability"],"futureInfo":{"enabled":true}}`, version)
			c := testClient(t, info, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				_, _ = io.WriteString(w, `{"type":"result","result":{"imageID":"image","futureResult":17},"futureEnvelope":"ignored"}`)
			}))
			// Progress observation is optional for older API v1 implementations.
			result, err := c.Execute(t.Context(), execution(), StreamOptions{Progress: func(api.OperationProgress) { t.Error("older server emitted progress") }})
			if err != nil || result.ImageID != "image" || posts.Load() != 1 {
				t.Fatalf("compatible server refused: result=%+v posts=%d err=%v", result, posts.Load(), err)
			}
			if _, err := c.Negotiate(t.Context(), api.CapabilityOperationProgress); !errors.Is(err, ErrMissingCapability) {
				t.Fatalf("missing explicitly required progress accepted: %v", err)
			}
		})
	}
	for _, major := range []int{0, 2, 99} {
		t.Run(fmt.Sprintf("major%d", major), func(t *testing.T) {
			var posts atomic.Int32
			c := testClient(t, fmt.Sprintf(`{"apiVersion":%d,"capabilities":["execute"]}`, major), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { posts.Add(1) }))
			if _, err := c.Execute(t.Context(), execution(), StreamOptions{}); !errors.Is(err, ErrUnsupportedVersion) || posts.Load() != 0 {
				t.Fatalf("unsupported major reached execution: posts=%d err=%v", posts.Load(), err)
			}
		})
	}
	tests := map[string]api.Request{
		"build":          {Kind: "build", Build: &api.Build{}},
		"component":      {Kind: "component", Build: &api.Build{}},
		"publish":        {Kind: "publish", Publish: &api.Publish{}},
		"registry read":  execution(),
		"registry write": execution(),
		"contexts":       execution(),
		"copy context":   execution(),
	}
	read := tests["registry read"]
	read.Options.Cache.RegistryFrom = []string{"registry.example/cache"}
	tests["registry read"] = read
	write := tests["registry write"]
	write.Options.Cache.RegistryTo = []string{"registry.example/cache"}
	tests["registry write"] = write
	contexts := tests["contexts"]
	contexts.Options.Contexts = map[string]string{"assets": "/assets"}
	tests["contexts"] = contexts
	copyContext := tests["copy context"]
	copyContext.Program.States[0].Operations = []api.Operation{{Kind: "copy", From: api.Source{Context: "assets"}}}
	tests["copy context"] = copyContext
	for name, request := range tests {
		t.Run(name, func(t *testing.T) {
			var posts atomic.Int32
			c := testClient(t, `{"apiVersion":1,"capabilities":["execute"]}`, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { posts.Add(1) }))
			if _, err := c.Execute(t.Context(), request, StreamOptions{}); !errors.Is(err, ErrMissingCapability) || posts.Load() != 0 {
				t.Fatalf("missing required capability reached POST: posts=%d err=%v", posts.Load(), err)
			}
		})
	}
}

func TestTypedRequestsAndPartialPublicationErrors(t *testing.T) {
	var mu sync.Mutex
	var requests []api.Request
	c := testClient(t, allInfo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request api.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		result := api.Result{ImageID: "image", Cache: &api.CacheStats{Hits: 3}}
		var failure *api.Error
		if request.Kind == "publish" {
			result = api.Result{Destinations: []api.Destination{{Destination: "first", Status: "passed", Reference: "first@sha256:abc"}, {Destination: "second", Status: "failed", Error: "denied"}}}
			failure = &api.Error{Code: "execution_failed", Message: "second destination denied"}
		}
		encode(t, w, api.Event{Type: "result", Result: &result, Error: failure})
	}))
	request := execution()
	literal := "${literal} quoted \" value\nnext"
	request.Program.States[0].Operations = []api.Operation{{Kind: "env", Name: "VALUE", Value: literal}, {Kind: "run", Args: []string{"echo", literal}}, {Kind: "copy", Sources: []string{"file"}, Destination: "/file", From: api.Source{Context: "assets"}}}
	request.Options.Contexts = map[string]string{"assets": "/component"}
	request.Options.Cache = api.Cache{RegistryFrom: []string{"registry.example/source"}, RegistryTo: []string{"registry.example/mirror"}}
	result, err := c.Execute(t.Context(), request, StreamOptions{})
	if err != nil || result.Cache == nil || result.Cache.Hits != 3 {
		t.Fatalf("typed result changed: %+v %v", result, err)
	}
	for _, kind := range []string{"build", "component"} {
		if _, err := c.Execute(t.Context(), api.Request{Kind: kind, Build: &api.Build{Files: []string{"/component/build.coopr"}, Target: "release", Args: map[string]string{"VALUE": literal}}, Options: request.Options}, StreamOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	partial, err := c.Execute(t.Context(), api.Request{Kind: "publish", Publish: &api.Publish{Artifact: "image", Source: "image", Destinations: []string{"first", "second"}}}, StreamOptions{})
	var remote *api.Error
	var executionErr *ExecutionError
	if !errors.As(err, &remote) || remote.Code != "execution_failed" || !errors.As(err, &executionErr) || !validID(executionErr.ID) || len(partial.Destinations) != 2 {
		t.Fatalf("partial publication lost: result=%+v err=%v", partial, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 4 || requests[0].Program.States[0].Operations[0].Value != literal || requests[0].Program.States[0].Operations[1].Args[1] != literal || requests[0].Options.Contexts["assets"] != "/component" || requests[0].Program.States[0].Operations[2].From.Context != "assets" {
		t.Fatalf("request semantics changed: %+v", requests)
	}
}

func TestAdmissionRefusalsPreserveStructuredErrorWithoutCancel(t *testing.T) {
	for code, status := range map[string]int{"duplicate": 409, "capacity": 429, "shutdown": 503} {
		t.Run(code, func(t *testing.T) {
			var deletes atomic.Int32
			c := testClient(t, allInfo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deletes.Add(1)
					acknowledge(t, w, r)
					return
				}
				w.WriteHeader(status)
				encode(t, w, api.Error{Code: code, Message: "registration refused"})
			}))
			_, err := c.Execute(t.Context(), execution(), StreamOptions{})
			var admission *HTTPError
			var remote *api.Error
			var invocation *ExecutionError
			if !errors.As(err, &admission) || admission.StatusCode != status || !errors.As(err, &remote) || remote.Code != code || !errors.As(err, &invocation) || !validID(invocation.ID) || deletes.Load() != 0 {
				t.Fatalf("admission refusal lost or existing ID canceled: %v deletes=%d", err, deletes.Load())
			}
		})
	}
}

func TestUncertainEventsCancelAndWait(t *testing.T) {
	for name, body := range map[string]string{
		"empty": "", "unknown": `{"type":"future"}`, "empty terminal": `{"type":"result"}`,
		"truncated": `{"type":"result","result":`, "extra json": `{"type":"result","result":{}} {}`,
		"duplicate event": `{"type":"result","result":{}}` + "\n" + `{"type":"result","result":{}}`,
		"duplicate key":   `{"type":"stdout","type":"result","result":{}}`,
		"mixed null":      `{"type":"result","result":{},"data":null}`,
		"mixed log":       `{"type":"stdout","data":"YQ==","error":{"code":"error"}}`,
		"null":            `null`, "invalid error": `{"type":"result","error":{"message":"missing code"}}`,
		"oversized": strings.Repeat("x", api.MaxEventBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			var deletes atomic.Int32
			c := testClient(t, allInfo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deletes.Add(1)
					acknowledge(t, w, r)
					return
				}
				_, _ = io.WriteString(w, body)
			}))
			_, err := c.Execute(t.Context(), execution(), StreamOptions{})
			var invocation *ExecutionError
			if err == nil || !errors.As(err, &invocation) || !validID(invocation.ID) || deletes.Load() != 1 || errors.Is(err, ErrCleanupFailed) {
				t.Fatalf("uncertain response not reconciled: deletes=%d err=%v", deletes.Load(), err)
			}
		})
	}
}

func TestCancellationWaitsForAcknowledgement(t *testing.T) {
	entered, deleting, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	c := testClient(t, allInfo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			close(deleting)
			select {
			case <-release:
				acknowledge(t, w, r)
			case <-r.Context().Done():
			}
			return
		}
		close(entered)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Execute(ctx, execution(), StreamOptions{}); done <- err }()
	<-entered
	cancel()
	<-deleting
	select {
	case err := <-done:
		t.Fatalf("returned before acknowledgement: %v", err)
	default:
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) || errors.Is(err, ErrCleanupFailed) {
		t.Fatalf("cancellation classification changed: %v", err)
	}
}

func TestDisconnectAndWriterFailuresAreReconciled(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprint(disconnect), func(t *testing.T) {
			var deletes atomic.Int32
			c := testClient(t, allInfo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deletes.Add(1)
					acknowledge(t, w, r)
					return
				}
				if disconnect {
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = connection.Close()
					return
				}
				encode(t, w, api.Event{Type: "stdout", Data: []byte("output")})
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			_, err := c.Execute(t.Context(), execution(), StreamOptions{Stdout: failingWriter{}})
			if err == nil || deletes.Load() != 1 || errors.Is(err, ErrCleanupFailed) {
				t.Fatalf("disconnect/writer failure not reconciled: %v deletes=%d", err, deletes.Load())
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCleanupAcknowledgementsAndTimeoutIdentity(t *testing.T) {
	tests := map[string]func(string) string{
		"running":              func(id string) string { return fmt.Sprintf(`{"id":%q,"state":"running"}`, id) },
		"wrong id":             func(string) string { return `{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","state":"canceled"}` },
		"failed without error": func(id string) string { return fmt.Sprintf(`{"id":%q,"state":"failed"}`, id) },
		"successful with error": func(id string) string {
			return fmt.Sprintf(`{"id":%q,"state":"succeeded","error":{"code":"execution_failed"}}`, id)
		},
		"unknown state": func(id string) string { return fmt.Sprintf(`{"id":%q,"state":"future"}`, id) },
		"trailing":      func(id string) string { return fmt.Sprintf(`{"id":%q,"state":"canceled"} null`, id) },
		"oversized":     func(string) string { return strings.Repeat("x", maxStatusBytes+1) },
		"case alias":    func(id string) string { return fmt.Sprintf(`{"id":%q,"state":"running","STATE":"canceled"}`, id) },
		"nested cleanup alias": func(id string) string {
			return fmt.Sprintf(`{"id":%q,"state":"failed","error":{"code":"cleanup_failed","Code":"execution_failed"}}`, id)
		},
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			c := testClient(t, allInfo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
					_, _ = io.WriteString(w, body(id))
					return
				}
				_, _ = io.WriteString(w, "malformed")
			}))
			_, err := c.Execute(t.Context(), execution(), StreamOptions{})
			if !errors.Is(err, ErrCleanupFailed) {
				t.Fatalf("malformed acknowledgement accepted: %v", err)
			}
		})
	}
	for _, code := range []string{"execution_failed", "cleanup_failed"} {
		t.Run(code, func(t *testing.T) {
			c := testClient(t, allInfo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
					encode(t, w, api.Status{ID: id, State: "failed", Error: &api.Error{Code: code, Message: "failure retained"}})
					return
				}
				_, _ = io.WriteString(w, "malformed")
			}))
			_, err := c.Execute(t.Context(), execution(), StreamOptions{})
			var remote *api.Error
			if !errors.As(err, &remote) || remote.Code != code || errors.Is(err, ErrCleanupFailed) != (code == "cleanup_failed") {
				t.Fatalf("failed execution confused with failed cleanup: %v", err)
			}
		})
	}
	t.Run("deadline retains ID and allows reconciliation", func(t *testing.T) {
		var release atomic.Bool
		c := testClient(t, allInfo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				if !release.Load() {
					<-r.Context().Done()
					return
				}
				acknowledge(t, w, r)
				return
			}
			_, _ = io.WriteString(w, "malformed")
		}))
		c.cancelTimeout = 20 * time.Millisecond
		_, err := c.Execute(t.Context(), execution(), StreamOptions{})
		var invocation *ExecutionError
		if !errors.Is(err, ErrCleanupFailed) || !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &invocation) || !validID(invocation.ID) {
			t.Fatalf("uncertain identity lost: %v", err)
		}
		release.Store(true)
		if _, err := c.CancelWait(t.Context(), invocation.ID); err != nil {
			t.Fatalf("could not reconcile retained ID: %v", err)
		}
	})
}

func TestGetBoundsAdditiveFieldsAndStatusEnvelopeHeadroom(t *testing.T) {
	id := strings.Repeat("a", 32)
	empty, err := json.Marshal(api.Event{Type: "result", Result: &api.Result{Metadata: map[string]json.RawMessage{"padding": json.RawMessage(`""`)}}})
	if err != nil {
		t.Fatal(err)
	}
	padding, err := json.Marshal(strings.Repeat("x", api.MaxEventBytes-len(empty)-1))
	if err != nil {
		t.Fatal(err)
	}
	result := api.Result{Metadata: map[string]json.RawMessage{"padding": padding}}
	event, err := json.Marshal(api.Event{Type: "result", Result: &result})
	if err != nil {
		t.Fatal(err)
	}
	if len(event)+1 > api.MaxEventBytes {
		t.Fatal("fixture terminal exceeds event contract")
	}
	status := api.Status{ID: id, State: "succeeded", Result: &result}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) <= api.MaxEventBytes {
		t.Fatal("fixture does not exercise status envelope headroom")
	}
	c := testClient(t, allInfo, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) }))
	got, err := c.Get(t.Context(), id)
	if err != nil || got.Result == nil || !bytes.Equal(got.Result.Metadata["padding"], padding) {
		t.Fatalf("valid near-limit status rejected: %v", err)
	}
	for _, body := range []string{`{"apiVersion":1} {}`, `{"apiVersion":1}` + strings.Repeat(" ", maxInfoBytes), `null`, `{"apiVersion":2,"APIVERSION":1}`} {
		c := testClient(t, body, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unexpected execution") }))
		if _, err := c.Info(t.Context()); err == nil {
			t.Fatal("invalid or oversized info accepted")
		}
	}
}

func TestConcurrentInvocationsUseAcceptedInfoAndRefreshInvalidatesIt(t *testing.T) {
	var incompatible atomic.Bool
	var infos, posts atomic.Int32
	c := testClient(t, "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/info" {
			infos.Add(1)
			if incompatible.Load() {
				encode(t, w, api.Info{APIVersion: 2})
			} else {
				_, _ = io.WriteString(w, allInfo)
			}
			return
		}
		posts.Add(1)
		encode(t, w, api.Event{Type: "result", Result: &api.Result{ImageID: "image"}})
	}))
	info, err := c.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	info.Capabilities[0] = "caller mutation"
	if _, err := c.Negotiate(t.Context(), api.CapabilityExecute); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 24 {
		wg.Go(func() {
			if _, err := c.Execute(t.Context(), execution(), StreamOptions{}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if infos.Load() != 1 || posts.Load() != 24 {
		t.Fatalf("repeated or corrupted negotiation: infos=%d posts=%d", infos.Load(), posts.Load())
	}
	incompatible.Store(true)
	if _, err := c.Info(t.Context()); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatal(err)
	}
	if _, err := c.Execute(t.Context(), execution(), StreamOptions{}); !errors.Is(err, ErrUnsupportedVersion) || posts.Load() != 24 {
		t.Fatalf("failed refresh retained stale acceptance: posts=%d err=%v", posts.Load(), err)
	}
}

func TestLocalValidationAndRequestBound(t *testing.T) {
	if _, err := New("relative.sock", Options{}); err == nil {
		t.Fatal("relative socket accepted")
	}
	if _, err := New("/absolute.sock", Options{CancellationTimeout: -1}); err == nil {
		t.Fatal("negative acknowledgement deadline accepted")
	}
	var posts atomic.Int32
	c := testClient(t, allInfo, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { posts.Add(1) }))
	request := execution()
	request.Program.States[0].ID = strings.Repeat("x", api.MaxRequestBytes)
	if _, err := c.Execute(t.Context(), request, StreamOptions{}); err == nil || posts.Load() != 0 {
		t.Fatalf("oversized request sent: %v", err)
	}
	if _, err := c.Get(t.Context(), "bad/id"); err == nil || posts.Load() != 0 {
		t.Fatalf("invalid ID sent: %v", err)
	}
	if _, err := c.Execute(t.Context(), api.Request{Kind: "unknown"}, StreamOptions{}); err == nil || posts.Load() != 0 {
		t.Fatalf("unsupported kind sent: %v", err)
	}
}
