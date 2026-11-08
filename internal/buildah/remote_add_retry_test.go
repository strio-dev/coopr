package buildah

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	upstream "go.podman.io/buildah"
)

func TestRemoteAddDoesNotRetryDestinationMutation(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		_, _ = w.Write([]byte("payload"))
	}))
	defer server.Close()
	var applied int
	builder := &inspectingAddBuilder{inspect: func(_ string, _ bool, _ upstream.AddAndCopyOptions, _ ...string) error {
		applied++
		return syscall.EBUSY
	}}
	err := applyRemoteAddSource(context.Background(), builder, Add{Destination: "/payload"}, upstream.AddAndCopyOptions{MaxRetries: 2, RetryDelay: time.Millisecond}, server.URL+"/payload")
	if !errors.Is(err, syscall.EBUSY) || attempts.Load() != 1 || applied != 1 {
		t.Fatalf("destination mutation repeated: downloads=%d applies=%d error=%v", attempts.Load(), applied, err)
	}
}

func TestRemoteAddRetryPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
		cancel()
	}))
	defer server.Close()
	err := applyRemoteAddSource(ctx, &recordingBuilder{}, Add{Destination: "/payload"}, upstream.AddAndCopyOptions{MaxRetries: 2, RetryDelay: time.Second}, server.URL+"/payload")
	if !errors.Is(err, context.Canceled) || attempts.Load() != 1 {
		t.Fatalf("canceled retry: attempts=%d error=%v", attempts.Load(), err)
	}
}
