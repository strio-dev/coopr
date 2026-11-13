// Package apiservice serves Coopr's same-account execution API on Unix sockets.
package apiservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/strio-dev/coopr/api"
)

type Backend func(context.Context, api.Request, io.Writer, io.Writer) (api.Result, error)
type execution struct {
	cancel context.CancelFunc
	done   chan struct{}
	status api.Status
}
type Server struct {
	ctx     context.Context
	backend Backend
	version string
	mu      sync.Mutex
	records map[string]*execution
	wg      sync.WaitGroup
	closed  bool
}

func New(ctx context.Context, version string, backend Backend) *Server {
	return &Server{ctx: ctx, backend: backend, version: version, records: map[string]*execution{}}
}

var validID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func reply(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/info" && r.Method == http.MethodGet {
		reply(w, 200, api.Info{APIVersion: api.Version, Version: s.version, Capabilities: []string{api.CapabilityComponentLoad, api.CapabilityComponentLoadName, api.CapabilityRunNetwork, api.CapabilityExecute, api.CapabilityBuild, api.CapabilityComponent, api.CapabilityPublish, api.CapabilityPublishOCILayout, api.CapabilityRegistryCache, api.CapabilityOperationProgress, api.CapabilityNamedContexts, api.CapabilityRunMounts, api.CapabilityBuildPlatform, api.CapabilitySelectedFileExports, api.CapabilityOptionalOCIOutput}})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/executions/")
	if !strings.HasPrefix(r.URL.Path, "/v1/executions/") || !validID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.execute(w, r, id)
	case http.MethodGet:
		s.mu.Lock()
		job := s.records[id]
		var status api.Status
		if job != nil {
			status = job.status
		}
		s.mu.Unlock()
		if job == nil {
			http.NotFound(w, r)
			return
		}
		reply(w, 200, status)
	case http.MethodDelete:
		s.mu.Lock()
		job := s.records[id]
		if job == nil {
			if len(s.records) >= 4096 {
				s.mu.Unlock()
				reply(w, 429, api.Error{Code: "capacity", Message: "service execution limit reached"})
				return
			}
			done := make(chan struct{})
			close(done)
			job = &execution{cancel: func() {}, done: done, status: api.Status{ID: id, State: "canceled", Error: &api.Error{Code: "canceled", Message: "canceled before execution"}}}
			s.records[id] = job
		}
		job.cancel()
		s.mu.Unlock()
		select {
		case <-job.done:
			s.mu.Lock()
			status := job.status
			status.Result = nil // cancellation acknowledges cleanup, not artifact delivery
			s.mu.Unlock()
			reply(w, 200, status)
		case <-r.Context().Done():
			return
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
func (s *Server) execute(w http.ResponseWriter, r *http.Request, id string) {
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	job := &execution{cancel: cancel, done: make(chan struct{}), status: api.Status{ID: id, State: "running"}}
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil {
		s.mu.Unlock()
		reply(w, 503, api.Error{Code: "shutdown", Message: "service is shutting down"})
		return
	}
	if _, ok := s.records[id]; ok {
		s.mu.Unlock()
		reply(w, 409, api.Error{Code: "duplicate", Message: "execution ID already used"})
		return
	}
	if len(s.records) >= 4096 {
		s.mu.Unlock()
		reply(w, 429, api.Error{Code: "capacity", Message: "service execution limit reached"})
		return
	}
	s.records[id] = job
	s.wg.Add(1)
	s.mu.Unlock()
	var result api.Result
	var failure *api.Error
	w.Header().Set("Content-Type", "application/x-ndjson")
	stream := &eventWriter{w: w, cancel: cancel}
	defer func() {
		if value := recover(); value != nil {
			failure = &api.Error{Code: "cleanup_failed", Message: "backend panic: cleanup could not be confirmed"}
		}
		encoded, encodeErr := json.Marshal(api.Event{Type: "result", Result: &result, Error: failure})
		if encodeErr != nil || len(encoded)+1 > api.MaxEventBytes {
			result = api.Result{}
			if failure == nil {
				failure = &api.Error{Code: "result_too_large", Message: "execution completed but its structured result exceeds the API event limit"}
			} else {
				bounded := *failure
				if len(bounded.Message) > 8192 {
					bounded.Message = bounded.Message[:8192] + " (truncated)"
				}
				if len(bounded.Code) > 128 {
					bounded.Code = "execution_failed"
				}
				failure = &bounded
			}
		}
		state := "succeeded"
		if failure != nil {
			state = "failed"
			if failure.Code == "canceled" {
				state = "canceled"
			}
		}
		s.mu.Lock()
		job.status = api.Status{ID: id, State: state, Result: &result, Error: failure}
		close(job.done)
		s.mu.Unlock()
		_ = stream.event(api.Event{Type: "result", Result: &result, Error: failure})
		s.wg.Done()
	}()
	r.Body = http.MaxBytesReader(w, r.Body, api.MaxRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request api.Request
	err := decoder.Decode(&request)
	if err == nil {
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			err = errors.New("request must contain exactly one JSON object")
		}
	}
	if err != nil {
		failure = &api.Error{Code: "invalid_request", Message: err.Error()}
		return
	}
	if err = ctx.Err(); err != nil {
		failure = &api.Error{Code: "canceled", Message: err.Error()}
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	ctx = api.WithProgressObserver(ctx, func(event api.OperationProgress) {
		_ = stream.event(api.Event{Type: "progress", Progress: &event})
	})
	result, err = s.backend(ctx, request, logWriter{stream, "stdout"}, logWriter{stream, "stderr"})
	if err != nil {
		var typed *api.Error
		if errors.As(err, &typed) {
			failure = typed
		} else {
			code := "execution_failed"
			if errors.Is(err, context.Canceled) {
				code = "canceled"
			}
			failure = &api.Error{Code: code, Message: err.Error()}
		}
	}
}

type eventWriter struct {
	mu     sync.Mutex
	w      http.ResponseWriter
	cancel context.CancelFunc
}

func (s *eventWriter) event(event api.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = http.NewResponseController(s.w).SetWriteDeadline(time.Now().Add(10 * time.Second))
	data, err := json.Marshal(event)
	if err == nil && len(data)+1 > api.MaxEventBytes {
		err = errors.New("event exceeds API size limit")
	}
	if err == nil {
		_, err = s.w.Write(append(data, '\n'))
	}
	if err != nil {
		s.cancel()
		return err
	}
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

type logWriter struct {
	stream *eventWriter
	kind   string
}

func (w logWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		size := min(len(p), 32<<10)
		if err := w.stream.event(api.Event{Type: w.kind, Data: p[:size]}); err != nil {
			return n, err
		}
		n += size
		p = p[size:]
	}
	return n, nil
}

// Drain prevents new execution registration, cancels all active requests and
// waits for their backend cleanup. Existing cancellation endpoints remain usable.
func (s *Server) Drain() {
	s.mu.Lock()
	s.closed = true
	for _, job := range s.records {
		job.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// Serve creates a private Unix listener and removes only its own socket inode.
func Serve(ctx context.Context, socket, version string, backend Backend) error {
	if !filepath.IsAbs(socket) {
		return errors.New("socket path must be absolute")
	}
	parent, err := os.Lstat(filepath.Dir(socket))
	if err != nil {
		return err
	}
	if !parent.IsDir() || parent.Mode().Perm() != 0700 {
		return errors.New("socket parent must be a private mode-0700 directory")
	}
	if stat, ok := parent.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("socket parent must belong to the service account")
	}
	if _, err = os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return errors.New("socket path already exists")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return err
	}
	listener.SetUnlinkOnClose(false)
	defer func() { _ = listener.Close() }()
	if err = os.Chmod(socket, 0600); err != nil {
		return err
	}
	original, err := os.Lstat(socket)
	if err != nil {
		return err
	}
	defer func() {
		current, e := os.Lstat(socket)
		if e == nil && os.SameFile(original, current) {
			_ = os.Remove(socket)
		}
	}()
	handler := New(ctx, version, backend)
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			handler.Drain()
			_ = server.Close()
		case <-done:
		}
	}()
	err = server.Serve(listener)
	close(done)
	handler.Drain()
	if errors.Is(err, http.ErrServerClosed) || ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("API service: %w", err)
}
