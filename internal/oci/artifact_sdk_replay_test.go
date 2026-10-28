package oci

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type artifactRequestBody struct {
	reader              io.Reader
	readBytes           atomic.Int64
	entered, closed     chan struct{}
	readOnce, closeOnce sync.Once
}

func (b *artifactRequestBody) Read(p []byte) (int, error) {
	if b.entered != nil {
		b.readOnce.Do(func() { close(b.entered) })
	}
	n, e := b.reader.Read(p)
	b.readBytes.Add(int64(n))
	return n, e
}
func (b *artifactRequestBody) Close() error {
	b.closeOnce.Do(func() {
		if c, ok := b.reader.(io.Closer); ok {
			_ = c.Close()
		}
		close(b.closed)
	})
	return nil
}

type artifactFailureReader struct{ err error }

func (r artifactFailureReader) Read([]byte) (int, error) { return 0, r.err }

func TestArtifactRegistrySDKRequestReplay(t *testing.T) {
	for _, preexisting := range []bool{false, true} {
		name := "capture"
		if preexisting {
			name = "existing-getbody"
		}
		t.Run(name, func(t *testing.T) {
			const data = "bounded attachment payload survives HTTP replay"
			var attempts, replays atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v2/" {
					w.WriteHeader(http.StatusOK)
					return
				}
				got, e := io.ReadAll(r.Body)
				if e != nil || string(got) != data {
					t.Errorf("replayed upload=%q error=%v", got, e)
				}
				if attempts.Add(1) == 1 {
					http.Error(w, "retry this upload", http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusCreated)
			}))
			t.Cleanup(server.Close)
			client := artifactSDKConnection(t, server)
			body := &artifactRequestBody{reader: strings.NewReader(data), closed: make(chan struct{})}
			request, e := http.NewRequestWithContext(t.Context(), http.MethodPut, server.URL+"/v2/coopr/sdk/blobs/uploads/session", body)
			if e != nil {
				t.Fatal(e)
			}
			request.ContentLength = int64(len(data))
			if preexisting {
				request.GetBody = func() (io.ReadCloser, error) { replays.Add(1); return io.NopCloser(strings.NewReader(data)), nil }
			}
			response, e := client.Do(request)
			if e != nil {
				t.Fatal(e)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusCreated || attempts.Load() != 2 {
				t.Fatalf("upload replay status=%d attempts=%d", response.StatusCode, attempts.Load())
			}
			if preexisting && replays.Load() != 1 {
				t.Fatalf("existing GetBody calls=%d want1", replays.Load())
			}
			select {
			case <-body.closed:
			case <-time.After(5 * time.Second):
				t.Fatal("original request body was not closed")
			}
			if body.readBytes.Load() != int64(len(data)) {
				t.Fatalf("original payload read=%d want%d", body.readBytes.Load(), len(data))
			}
		})
	}
}

func TestArtifactRegistrySDKRequestCaptureFailure(t *testing.T) {
	sentinel := errors.New("attachment reader failed")
	for _, failure := range []string{"advertised-size", "unknown-size", "read-error", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v2/" {
					w.WriteHeader(http.StatusOK)
					return
				}
				requests.Add(1)
				w.WriteHeader(http.StatusCreated)
			}))
			t.Cleanup(server.Close)
			client := artifactSDKConnection(t, server)
			body := &artifactRequestBody{reader: io.LimitReader(zeroReader{}, maxMetadataBytes+2), closed: make(chan struct{}), entered: make(chan struct{})}
			if failure == "read-error" {
				body.reader = artifactFailureReader{err: sentinel}
			}
			if failure == "cancel" {
				reader, writer := io.Pipe()
				defer func() { _ = writer.Close() }()
				body.reader = reader
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request, e := http.NewRequestWithContext(ctx, http.MethodPut, server.URL+"/v2/coopr/sdk/blobs/uploads/session", body)
			if e != nil {
				t.Fatal(e)
			}
			request.ContentLength = -1
			if failure == "advertised-size" {
				request.ContentLength = maxMetadataBytes + 1
			}
			done := make(chan error, 1)
			go func() {
				response, e := client.Do(request)
				if response != nil {
					_ = response.Body.Close()
				}
				done <- e
			}()
			if failure == "cancel" {
				select {
				case <-body.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("capture read did not begin")
				}
				cancel()
			}
			select {
			case e := <-done:
				if e == nil {
					t.Fatal("invalid attachment body accepted")
				}
				if failure == "read-error" && !errors.Is(e, sentinel) {
					t.Fatalf("reader error identity lost: %v", e)
				}
				if failure == "cancel" && !errors.Is(e, context.Canceled) {
					t.Fatalf("capture cancellation: %v", e)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("capture did not finish")
			}
			select {
			case <-body.closed:
			case <-time.After(5 * time.Second):
				t.Fatal("failed capture body was not closed")
			}
			if requests.Load() != 0 {
				t.Fatalf("invalid captured body reached registry: %d requests", requests.Load())
			}
			if failure == "advertised-size" && body.readBytes.Load() != 0 {
				t.Fatal("oversized advertised body was read")
			}
			if body.readBytes.Load() > maxMetadataBytes+1 {
				t.Fatalf("unbounded capture read=%d", body.readBytes.Load())
			}
		})
	}
}
