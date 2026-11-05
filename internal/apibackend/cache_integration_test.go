package apibackend

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/strio-dev/coopr"
	"github.com/strio-dev/coopr/api"
	"github.com/strio-dev/coopr/internal/apiservice"
	"github.com/strio-dev/coopr/internal/buildah"
)

func TestMain(m *testing.M) {
	if coopr.InitReexec() {
		return
	}
	os.Exit(m.Run())
}

type nativeAPIResponse struct {
	result   *api.Result
	failure  *api.Error
	progress []api.OperationProgress
	logs     string
	err      error
}

type nativeAPIFixture struct {
	root, context, policy, cache string
	gate                         *httptest.Server
	arrivals                     chan string
	release                      chan struct{}
	once                         sync.Once
}

func newNativeAPIFixture(t *testing.T) *nativeAPIFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("live supervised Buildah workers")
	}
	if !apiTestCGOEnabled {
		t.Skip("rootless Buildah workers require CGO")
	}
	root := t.TempDir()
	f := &nativeAPIFixture{root: root, context: filepath.Join(root, "context"), policy: filepath.Join(root, "policy.json"), cache: filepath.Join(root, "cache"), arrivals: make(chan string, 16), release: make(chan struct{})}
	if err := os.Mkdir(f.context, 0700); err != nil {
		t.Fatal(err)
	}
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Skip("static BusyBox fixture unavailable")
	}
	data, err := os.ReadFile(busybox)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.context, "busybox"), data, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.policy, []byte(`{"default":[{"type":"reject"}],"transports":{"containers-storage":{"":[{"type":"insecureAcceptAnything"}]},"oci":{"":[{"type":"insecureAcceptAnything"}]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	f.gate = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case f.arrivals <- r.URL.Query().Get("lane"):
		case <-r.Context().Done():
			return
		}
		select {
		case <-f.release:
			_, _ = io.WriteString(w, "released\n")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		f.once.Do(func() { close(f.release) })
		f.gate.Close()
		// API callers remain outside the worker's user namespace. Let native
		// maintenance remove fixture image trees whose mapped owners cannot be
		// removed by an ordinary host os.RemoveAll.
		for _, name := range []string{"shared", "fresh"} {
			store := buildah.StoreOptions{RunRoot: filepath.Join(root, name, "run"), GraphRoot: filepath.Join(root, name, "graph"), GraphDriverName: "vfs"}
			if _, err := os.Stat(store.GraphRoot); os.IsNotExist(err) {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, err := buildah.MaintainStoreSupervised(ctx, buildah.StoreMaintenanceRequest{Store: store, Mode: buildah.StoreMaintenancePrune, All: true})
			cancel()
			if err != nil {
				t.Errorf("prune fixture %s: %v", name, err)
			}
		}
	})
	return f
}

func (f *nativeAPIFixture) request(label, lane, store string) api.Request {
	command := fmt.Sprintf("/bin/busybox wget -T 90 -q -O /gate %s; /bin/busybox od -An -N16 -tx1 /dev/urandom > /proof; printf '%%s\\n' %s > /lane", strconv.Quote(f.gate.URL+"/?lane="+lane), strconv.Quote(lane))
	operations := []api.Operation{
		{Kind: "env", Name: "http_proxy", Value: ""}, {Kind: "env", Name: "HTTP_PROXY", Value: ""}, {Kind: "env", Name: "ALL_PROXY", Value: ""}, {Kind: "env", Name: "NO_PROXY", Value: "*"},
		{Kind: "copy", Sources: []string{"busybox"}, Destination: "/bin/busybox"},
		{Kind: "run", Args: []string{"/bin/busybox", "sh", "-eu", "-c", command}},
	}
	return api.Request{Kind: "execute", Program: &api.Program{States: []api.State{{ID: "result", From: api.Source{Image: "scratch"}, Operations: operations}}, Output: "result"}, Options: api.Options{ContextDir: f.context, Isolation: "chroot", Runtime: "crun", Store: api.Store{RunRoot: filepath.Join(f.root, store, "run"), GraphRoot: filepath.Join(f.root, store, "graph"), Driver: "vfs"}, Cache: api.Cache{LocalDir: f.cache}, Registry: api.Registry{SignaturePolicyPath: f.policy}, Output: api.Output{OCI: filepath.Join(f.root, label+".oci"), Filesystem: filepath.Join(f.root, label+".rootfs")}}}
}

func nativeAPIServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(apiservice.New(t.Context(), "cache-integration", Execute))
	t.Cleanup(server.Close)
	return server
}

func startNativeAPI(ctx context.Context, server *httptest.Server, request api.Request) (string, <-chan nativeAPIResponse) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		panic(err)
	}
	id := hex.EncodeToString(nonce[:])
	done := make(chan nativeAPIResponse, 1)
	go func() {
		out := nativeAPIResponse{}
		defer func() { done <- out }()
		body, err := json.Marshal(request)
		if err != nil {
			out.err = err
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/executions/"+id, bytes.NewReader(body))
		if err != nil {
			out.err = err
			return
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(req)
		if err != nil {
			out.err = err
			return
		}
		defer func() { out.err = errors.Join(out.err, response.Body.Close()) }()
		if response.StatusCode != http.StatusOK {
			out.err = fmt.Errorf("execution HTTP status %s", response.Status)
			return
		}
		decoder := json.NewDecoder(response.Body)
		var logs strings.Builder
		for {
			var event api.Event
			if err := decoder.Decode(&event); err != nil {
				if err != io.EOF {
					out.err = err
				}
				break
			}
			if event.Type == "result" {
				if out.result != nil {
					out.err = fmt.Errorf("multiple terminal events")
					break
				}
				out.result, out.failure = event.Result, event.Error
			} else if event.Type == "progress" && event.Progress != nil {
				out.progress = append(out.progress, *event.Progress)
			} else {
				logs.Write(event.Data)
			}
		}
		out.logs = logs.String()
		if out.result == nil && out.err == nil {
			out.err = fmt.Errorf("missing terminal event")
		}
	}()
	return id, done
}

func awaitNativeAPI(t *testing.T, ctx context.Context, done <-chan nativeAPIResponse, success bool) nativeAPIResponse {
	t.Helper()
	select {
	case response := <-done:
		if response.err != nil || (response.failure == nil) != success {
			t.Fatalf("API result: error=%v failure=%+v\n%s", response.err, response.failure, response.logs)
		}
		return response
	case <-ctx.Done():
		t.Fatalf("API response did not drain: %v", ctx.Err())
		return nativeAPIResponse{}
	}
}

func awaitNativeAPIArrivals(t *testing.T, ctx context.Context, f *nativeAPIFixture, lanes ...string) {
	t.Helper()
	var got []string
	for len(got) < len(lanes) {
		select {
		case lane := <-f.arrivals:
			got = append(got, lane)
		case <-ctx.Done():
			t.Fatalf("native RUN rendezvous timed out, arrivals=%v: %v", got, ctx.Err())
		}
	}
	slices.Sort(got)
	slices.Sort(lanes)
	if !slices.Equal(got, lanes) {
		t.Fatalf("RUN arrivals=%v, want %v", got, lanes)
	}
}

func directNativeAPIWorkers(t *testing.T) []int {
	t.Helper()
	children := map[int]bool{}
	tasks, err := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		data, err := os.ReadFile(task)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(text)
			if err != nil {
				t.Fatal(err)
			}
			command, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
			if err == nil && bytes.HasPrefix(command, []byte("coopr-buildah-plan-worker-v1")) {
				children[pid] = true
			}
		}
	}
	var result []int
	for pid := range children {
		result = append(result, pid)
	}
	slices.Sort(result)
	return result
}

func TestAPIWorkersOverlapAndRecoverGraphCacheAcrossRestarts(t *testing.T) {
	f := newNativeAPIFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	server := nativeAPIServer(t)
	requests := []api.Request{f.request("cold-a", "a", "shared"), f.request("cold-b", "b", "shared")}
	_, first := startNativeAPI(ctx, server, requests[0])
	_, second := startNativeAPI(ctx, server, requests[1])
	awaitNativeAPIArrivals(t, ctx, f, "a", "b")
	workers := directNativeAPIWorkers(t)
	if len(workers) != 2 {
		t.Fatalf("rendezvous did not retain two distinct API worker processes: %v", workers)
	}
	f.once.Do(func() { close(f.release) })
	proofs := map[string]string{}
	for i, done := range []<-chan nativeAPIResponse{first, second} {
		response := awaitNativeAPI(t, ctx, done, true)
		if response.result.Cache == nil || response.result.Cache.Errors != 0 {
			t.Fatalf("cold cache statistics: %+v\n%s", response.result.Cache, response.logs)
		}
		data, err := os.ReadFile(filepath.Join(requests[i].Options.Output.Filesystem, "proof"))
		if err != nil {
			t.Fatal(err)
		}
		proofs[[]string{"a", "b"}[i]] = string(data)
	}
	server.Close()
	if remaining := directNativeAPIWorkers(t); len(remaining) != 0 {
		t.Fatalf("completed API requests retained worker processes: %v", remaining)
	}
	// Every request launches a fresh native worker. A new service removes all
	// in-memory solver state; fresh stores additionally require portable graphs.
	for _, store := range []string{"shared", "fresh"} {
		server = nativeAPIServer(t)
		for _, lane := range []string{"a", "b"} {
			request := f.request("warm-"+store+"-"+lane, lane, store)
			_, done := startNativeAPI(ctx, server, request)
			response := awaitNativeAPI(t, ctx, done, true)
			if response.result.Cache == nil || response.result.Cache.Hits != 2 || response.result.Cache.Misses != 0 || response.result.Cache.Errors != 0 {
				t.Fatalf("%s/%s did not restore complete graph: %+v\n%s", store, lane, response.result.Cache, response.logs)
			}
			data, err := os.ReadFile(filepath.Join(request.Options.Output.Filesystem, "proof"))
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != proofs[lane] {
				t.Fatalf("%s/%s reran nondeterministic workload", store, lane)
			}
		}
		server.Close()
	}
	select {
	case lane := <-f.arrivals:
		t.Fatalf("warm API cache reuse reran lane %q", lane)
	default:
	}
}

func TestAPICancellationDrainsWorkerBeforeAcknowledgingAndDoesNotCacheRun(t *testing.T) {
	f := newNativeAPIFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	server := nativeAPIServer(t)
	id, done := startNativeAPI(ctx, server, f.request("canceled", "cancel", "shared"))
	awaitNativeAPIArrivals(t, ctx, f, "cancel")
	if workers := directNativeAPIWorkers(t); len(workers) != 1 {
		t.Fatalf("missing native worker before cancellation: %v", workers)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, server.URL+"/v1/executions/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	var status api.Status
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || status.State != "canceled" {
		t.Fatalf("cancel acknowledgement: HTTP=%s status=%+v", response.Status, status)
	}
	if workers := directNativeAPIWorkers(t); len(workers) != 0 {
		t.Fatalf("cancellation acknowledged before worker cleanup: %v", workers)
	}
	terminal := awaitNativeAPI(t, ctx, done, false)
	if terminal.failure.Code != "canceled" {
		t.Fatalf("cancellation terminal: %+v", terminal.failure)
	}
	var canceledPhase bool
	for _, event := range checkProgressPairs(t, terminal.progress) {
		if event.Instruction == "run" && event.Phase == "execute" && event.State == "canceled" {
			canceledPhase = true
		}
	}
	if !canceledPhase {
		t.Fatalf("graceful cancellation did not retain its RUN observation: %+v", terminal.progress)
	}
	if _, err := os.Lstat(filepath.Join(f.root, "canceled.oci")); !os.IsNotExist(err) {
		t.Fatalf("cancellation published output: %v", err)
	}
	f.once.Do(func() { close(f.release) })
	_, retry := startNativeAPI(ctx, server, f.request("retry", "cancel", "shared"))
	awaitNativeAPIArrivals(t, ctx, f, "cancel")
	completed := awaitNativeAPI(t, ctx, retry, true)
	if completed.result.Cache == nil || completed.result.Cache.Misses == 0 {
		t.Fatalf("canceled RUN unexpectedly reused a result: %+v", completed.result.Cache)
	}
}
