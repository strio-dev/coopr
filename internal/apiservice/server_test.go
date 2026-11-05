package apiservice

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/strio-dev/coopr/api"
)

const testID = "0123456789abcdef0123456789abcdef"

func TestInfoAdvertisesRegistryCacheAndPreservesDirectionalRequest(t *testing.T) {
	var received api.Cache
	s := New(context.Background(), "test", func(_ context.Context, request api.Request, _, _ io.Writer) (api.Result, error) {
		received = request.Options.Cache
		return api.Result{}, nil
	})
	infoResponse := httptest.NewRecorder()
	s.ServeHTTP(infoResponse, httptest.NewRequest("GET", "/v1/info", nil))
	var info api.Info
	if err := json.Unmarshal(infoResponse.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.APIVersion != 1 || !slices.Contains(info.Capabilities, "registry-cache") {
		t.Fatalf("missing registry cache negotiation: %+v", info)
	}
	if !slices.Contains(info.Capabilities, api.CapabilityComponentLoadName) {
		t.Fatalf("missing named component-load capability: %+v", info)
	}
	if !slices.Contains(info.Capabilities, api.CapabilityRunNetwork) {
		t.Fatalf("missing RUN network capability: %+v", info)
	}
	if !slices.Contains(info.Capabilities, api.CapabilityBuildPlatform) {
		t.Fatalf("missing native build-platform capability: %+v", info)
	}
	if !slices.Contains(info.Capabilities, api.CapabilityNamedContexts) {
		t.Fatalf("missing named-context negotiation: %+v", info)
	}
	out := httptest.NewRecorder()
	s.ServeHTTP(out, httptest.NewRequest("POST", "/v1/executions/"+testID, strings.NewReader(`{"kind":"execute","options":{"cache":{"localDir":"/local/cache","noCache":true,"registryFrom":["registry.example/read"],"registryTo":["registry.example/write"]}}}`)))
	if out.Code != 200 || received.LocalDir != "/local/cache" || !received.NoCache || !slices.Equal(received.RegistryFrom, []string{"registry.example/read"}) || !slices.Equal(received.RegistryTo, []string{"registry.example/write"}) {
		t.Fatalf("directional cache request changed: %+v response=%s", received, out.Body.String())
	}
}

func TestCancelBeforeRegistration(t *testing.T) {
	calls := 0
	server := New(context.Background(), "test", func(context.Context, api.Request, io.Writer, io.Writer) (api.Result, error) {
		calls++
		return api.Result{}, nil
	})
	canceled := httptest.NewRecorder()
	server.ServeHTTP(canceled, httptest.NewRequest("DELETE", "/v1/executions/"+testID, nil))
	posted := httptest.NewRecorder()
	server.ServeHTTP(posted, httptest.NewRequest("POST", "/v1/executions/"+testID, strings.NewReader(`{"kind":"execute"}`)))
	if canceled.Code != 200 || posted.Code != 409 || calls != 0 {
		t.Fatalf("cancel=%d post=%d calls=%d", canceled.Code, posted.Code, calls)
	}
}
func TestStrictRequests(t *testing.T) {
	for _, body := range []string{`{"unknown":1}`, `{} {}`, strings.Repeat("x", api.MaxRequestBytes+1)} {
		called := false
		s := New(context.Background(), "test", func(context.Context, api.Request, io.Writer, io.Writer) (api.Result, error) {
			called = true
			return api.Result{}, nil
		})
		out := httptest.NewRecorder()
		s.ServeHTTP(out, httptest.NewRequest("POST", "/v1/executions/"+testID, strings.NewReader(body)))
		var event api.Event
		if err := json.Unmarshal(out.Body.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if called || event.Error == nil || event.Error.Code != "invalid_request" {
			t.Fatalf("called=%v event=%+v", called, event)
		}
	}
}
func TestCancellationWaitsForCleanup(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	s := New(context.Background(), "test", func(ctx context.Context, _ api.Request, _, _ io.Writer) (api.Result, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return api.Result{}, &api.Error{Code: "canceled", Message: "cleaned"}
	})
	postDone := make(chan struct{})
	go func() {
		defer close(postDone)
		s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/executions/"+testID, strings.NewReader(`{"kind":"execute"}`)))
	}()
	<-started
	deleteDone := make(chan struct{})
	out := httptest.NewRecorder()
	go func() {
		defer close(deleteDone)
		s.ServeHTTP(out, httptest.NewRequest("DELETE", "/v1/executions/"+testID, nil))
	}()
	<-canceled
	select {
	case <-deleteDone:
		t.Fatal("acknowledged before cleanup")
	default:
	}
	close(release)
	<-deleteDone
	<-postDone
	var status api.Status
	if err := json.Unmarshal(out.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.State != "canceled" {
		t.Fatalf("status=%+v", status)
	}
}
func TestConcurrentStreamsAndTerminal(t *testing.T) {
	s := New(context.Background(), "test", func(_ context.Context, _ api.Request, out, errOut io.Writer) (api.Result, error) {
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); _, _ = out.Write([]byte("one")); _, _ = errOut.Write([]byte("two")) }()
		}
		wg.Wait()
		return api.Result{ImageID: "image"}, nil
	})
	out := httptest.NewRecorder()
	s.ServeHTTP(out, httptest.NewRequest("POST", "/v1/executions/"+testID, strings.NewReader(`{"kind":"execute"}`)))
	decoder := json.NewDecoder(out.Body)
	logs, terminal := 0, 0
	for {
		var event api.Event
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == "result" {
			terminal++
			if event.Result.ImageID != "image" {
				t.Fatal(event)
			}
		} else {
			logs++
		}
	}
	if logs != 16 || terminal != 1 {
		t.Fatalf("logs=%d terminal=%d", logs, terminal)
	}
}
func TestPrivateUnixSocketAndShutdown(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "api.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, socket, "test", func(context.Context, api.Request, io.Writer, io.Writer) (api.Result, error) { return api.Result{}, nil })
	}()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := client.Get("http://coopr/v1/info")
		if err == nil {
			var info api.Info
			err = json.NewDecoder(response.Body).Decode(&info)
			_ = response.Body.Close()
			if err != nil || info.APIVersion != 1 {
				t.Fatalf("info=%+v err=%v", info, err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	info, err := os.Stat(socket)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("socket=%v err=%v", info, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket retained: %v", err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := Serve(context.Background(), socket, "test", nil); err == nil {
		t.Fatal("accepted public directory")
	}
}

func TestExistingSocketPathIsNeverReplaced(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "api.sock")
	if err := os.WriteFile(socket, []byte("owned"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Serve(context.Background(), socket, "test", nil); err == nil {
		t.Fatal("accepted existing destination")
	}
	data, err := os.ReadFile(socket)
	if err != nil || string(data) != "owned" {
		t.Fatalf("destination changed: %q %v", data, err)
	}
}

func TestOversizedTerminalResultIsBounded(t *testing.T) {
	s := New(context.Background(), "test", func(context.Context, api.Request, io.Writer, io.Writer) (api.Result, error) {
		raw, _ := json.Marshal(strings.Repeat("x", api.MaxEventBytes))
		return api.Result{Metadata: map[string]json.RawMessage{"large": raw}}, nil
	})
	post := httptest.NewRecorder()
	s.ServeHTTP(post, httptest.NewRequest("POST", "/v1/executions/"+testID, strings.NewReader(`{"kind":"execute"}`)))
	var event api.Event
	if err := json.Unmarshal(post.Body.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if post.Body.Len() > api.MaxEventBytes || event.Error == nil || event.Error.Code != "result_too_large" {
		t.Fatalf("unbounded result: %d %+v", post.Body.Len(), event.Error)
	}
	canceled := httptest.NewRecorder()
	s.ServeHTTP(canceled, httptest.NewRequest("DELETE", "/v1/executions/"+testID, nil))
	var status api.Status
	if err := json.Unmarshal(canceled.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if canceled.Body.Len() > api.MaxEventBytes || status.State != "failed" || status.Result != nil {
		t.Fatalf("unbounded cleanup acknowledgement: %+v", status)
	}
}

func TestOversizedResultPreservesCleanupFailure(t *testing.T) {
	s := New(context.Background(), "test", func(context.Context, api.Request, io.Writer, io.Writer) (api.Result, error) {
		raw, _ := json.Marshal(strings.Repeat("x", api.MaxEventBytes))
		return api.Result{Metadata: map[string]json.RawMessage{"large": raw}}, &api.Error{Code: "cleanup_failed", Message: strings.Repeat("cleanup incomplete ", api.MaxEventBytes/8)}
	})
	post := httptest.NewRecorder()
	s.ServeHTTP(post, httptest.NewRequest("POST", "/v1/executions/"+testID, strings.NewReader(`{"kind":"execute"}`)))
	var event api.Event
	if err := json.Unmarshal(post.Body.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if post.Body.Len() > api.MaxEventBytes || event.Error == nil || event.Error.Code != "cleanup_failed" {
		t.Fatalf("lost cleanup failure: %d %+v", post.Body.Len(), event.Error)
	}
	ack := httptest.NewRecorder()
	s.ServeHTTP(ack, httptest.NewRequest("DELETE", "/v1/executions/"+testID, nil))
	var status api.Status
	if err := json.Unmarshal(ack.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Error == nil || status.Error.Code != "cleanup_failed" || ack.Body.Len() > api.MaxEventBytes {
		t.Fatalf("lost cleanup failure acknowledgement: %+v", status)
	}
}
