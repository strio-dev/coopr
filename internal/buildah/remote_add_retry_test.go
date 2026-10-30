package buildah

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	upstream "go.podman.io/buildah"
)

func TestRemoteAddUsesNativeRetryPolicy(t *testing.T) {
	for _, retries := range []int{0, 1} {
		t.Run(strconv.Itoa(retries), func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if attempts.Add(1) == 1 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				_, _ = w.Write([]byte("retried payload"))
			}))
			defer server.Close()
			var applied int
			builder := &inspectingAddBuilder{inspect: func(_ string, _ bool, options upstream.AddAndCopyOptions, sources ...string) error {
				applied++
				contents, err := os.ReadFile(filepath.Join(options.ContextDir, sources[0]))
				if err != nil || string(contents) != "retried payload" {
					t.Fatalf("downloaded source = %q, %v", contents, err)
				}
				return nil
			}}
			options := upstream.AddAndCopyOptions{MaxRetries: retries, RetryDelay: time.Millisecond}
			err := applyRemoteAddSource(context.Background(), builder, Add{Destination: "/payload"}, options, server.URL+"/payload")
			if retries == 0 {
				if err == nil || attempts.Load() != 1 || applied != 0 {
					t.Fatalf("disabled retries: attempts=%d, error=%v", attempts.Load(), err)
				}
			} else if err != nil || attempts.Load() != 2 || applied != 1 {
				t.Fatalf("native retry: attempts=%d, error=%v", attempts.Load(), err)
			}
		})
	}
}

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
