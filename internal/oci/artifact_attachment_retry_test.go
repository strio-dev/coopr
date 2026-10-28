package oci

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
)

func TestArtifactRegistryAttachmentReadRetry(t *testing.T) {
	for _, mode := range []string{"direct", "layout"} {
		for _, fault := range []string{"unknown400", "body-reset", "two-independent", "denied400", "unknown-denied400", "denied-unknown400", "nonjson429", "invalid-token401", "status500", "status503", "persistent503", "legacy403", "json400wrongmime", "persistent429", "delay503", "sequence-zero-missing", "sequence-past-date"} {
			for _, retries := range []uint{0, 1} {
				sequence := strings.HasPrefix(fault, "sequence-")
				if sequence && (mode != "direct" || retries != 0) {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/retries=%d", mode, fault, retries), func(t *testing.T) {
					backend := registryserver.New()
					var armed atomic.Bool
					var faults, subjectAttempts, failedAt, recoveredAt atomic.Int64
					var subjectDigest string
					var inventoryDigest atomic.Value
					inventoryDigest.Store("")
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
						w.Header().Set("Connection", "close")
						if armed.Load() && fault == "two-independent" && request.Method == http.MethodHead && strings.HasSuffix(request.URL.Path, "/manifests/"+subjectDigest) {
							if subjectAttempts.Add(1) == 1 {
								http.Error(w, "temporary subject HEAD failure", http.StatusServiceUnavailable)
								return
							}
						}
						if armed.Load() && request.Method == http.MethodHead && strings.Contains(request.URL.Path, "/manifests/sha256-") {
							response := httptest.NewRecorder()
							backend.ServeHTTP(response, request)
							if digest := response.Header().Get("Docker-Content-Digest"); digest != "" {
								inventoryDigest.Store("/manifests/" + digest)
							}
							for name, values := range response.Header() {
								w.Header()[name] = values
							}
							w.WriteHeader(response.Code)
							_, _ = w.Write(response.Body.Bytes())
							return
						}
						if request.Method == http.MethodGet && (strings.Contains(request.URL.Path, "/manifests/sha256-") || (inventoryDigest.Load().(string) != "" && strings.HasSuffix(request.URL.Path, inventoryDigest.Load().(string)))) && (((fault == "persistent503" || fault == "persistent429" || sequence && faults.Load() < 2) && armed.Load()) || armed.CompareAndSwap(true, false)) {
							attempt := faults.Add(1)
							if sequence && attempt == 2 {
								armed.Store(false)
							}
							failedAt.CompareAndSwap(0, time.Now().UnixNano())
							if fault != "body-reset" && fault != "two-independent" {
								status := http.StatusBadRequest
								body := `{"errors":[{"code":"UNKNOWN","message":"temporary inventory read error"}]}`
								w.Header().Set("Content-Type", "application/json")
								switch fault {
								case "sequence-zero-missing", "sequence-past-date":
									status = http.StatusTooManyRequests
									body = "rate limited"
									w.Header().Set("Content-Type", "text/plain")
									if attempt == 1 {
										if fault == "sequence-zero-missing" {
											w.Header().Set("Retry-After", "0")
										} else {
											w.Header().Set("Retry-After", "1")
										}
									}
									if attempt == 2 && fault == "sequence-past-date" {
										w.Header().Set("Retry-After", "Thu, 01 Jan 1970 00:00:00 GMT")
									}
								case "legacy403":
									status = http.StatusForbidden
									body = `{"details":"legacy registry failure"}`
								case "json400wrongmime":
									w.Header().Set("Content-Type", "text/plain")
								case "denied400":
									body = `{"errors":[{"code":"DENIED"}]}`
								case "unknown-denied400":
									body = `{"errors":[{"code":"UNKNOWN"},{"code":"DENIED"}]}`
								case "denied-unknown400":
									body = `{"errors":[{"code":"DENIED"},{"code":"UNKNOWN"}]}`
								case "nonjson429", "persistent429":
									status = http.StatusTooManyRequests
									body = "rate limited"
									w.Header().Set("Retry-After", "0")
									w.Header().Set("Content-Type", "text/plain")
								case "invalid-token401":
									status = http.StatusUnauthorized
									w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
								case "status500":
									status = http.StatusInternalServerError
								case "status503", "persistent503", "delay503":
									status = http.StatusServiceUnavailable
								}
								w.WriteHeader(status)
								_, _ = w.Write([]byte(body))
								return
							}
							response := httptest.NewRecorder()
							backend.ServeHTTP(response, request)
							for name, values := range response.Header() {
								w.Header()[name] = values
							}
							w.Header().Set("Content-Length", fmt.Sprint(response.Body.Len()))
							w.WriteHeader(response.Code)
							_, _ = w.Write(response.Body.Bytes()[:response.Body.Len()/2])
							w.(http.Flusher).Flush()
							connection, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							if err := connection.(*net.TCPConn).SetLinger(0); err != nil {
								t.Error(err)
							}
							_ = connection.Close()
							return
						}
						if fault == "delay503" && faults.Load() > 0 && request.Method == http.MethodGet && (strings.Contains(request.URL.Path, "/manifests/sha256-") || inventoryDigest.Load().(string) != "" && strings.HasSuffix(request.URL.Path, inventoryDigest.Load().(string))) {
							recoveredAt.CompareAndSwap(0, time.Now().UnixNano())
						}
						backend.ServeHTTP(w, request)
					}))
					t.Cleanup(server.Close)
					delay := time.Millisecond
					if fault == "delay503" {
						delay = 20 * time.Millisecond
					}
					resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true, Retry: retries, RetryDelay: delay})
					if err != nil {
						t.Fatal(err)
					}
					metadata, paths := baleComponentMetadata(t, "amd64", "attachment-retry", baleRegistryDependency)
					reference := strings.TrimPrefix(server.URL, "http://") + "/coopr/attachment-retry:stable"
					root, err := publishBaleRegistryComponent(t, mode, resolver, reference, metadata, paths)
					if err != nil {
						t.Fatal(err)
					}
					subjectDigest = root.Digest.String()
					armed.Store(true)
					if sequence {
						deadline := 3 * time.Second
						if fault == "sequence-past-date" {
							deadline = 1500 * time.Millisecond
						}
						ctx, cancel := context.WithTimeout(t.Context(), deadline)
						defer cancel()
						_, err = resolver.PublishComponent(ctx, reference, metadata, paths)
					} else {
						_, err = publishBaleRegistryComponent(t, mode, resolver, reference, metadata, paths)
					}
					wantFaults := int64(1)
					if sequence {
						wantFaults = 2
					}
					if fault == "persistent503" {
						wantFaults = int64(retries + 1)
					}
					if fault == "persistent429" {
						wantFaults = 5
					}
					if fault == "two-independent" && retries == 0 {
						wantFaults = 0
					}
					if faults.Load() != wantFaults {
						t.Fatal("dependency attachment inventory read fault was not exercised exactly once")
					}
					if fault == "two-independent" && subjectAttempts.Load() != int64(retries+1) {
						t.Fatalf("subject HEAD attempts=%d want%d", subjectAttempts.Load(), retries+1)
					}
					if sequence {
						if fault == "sequence-past-date" {
							if !errors.Is(err, context.DeadlineExceeded) {
								t.Fatalf("past Retry-After date bypassed previous delay: %v", err)
							}
						} else if err != nil {
							t.Fatalf("zero Retry-After was not retained for missing header: %v", err)
						}
						return
					}
					expectFailure := (retries == 0 && fault != "nonjson429") || fault == "unknown-denied400" || fault == "denied400" || fault == "denied-unknown400" || fault == "invalid-token401" || fault == "status500" || fault == "persistent503" || fault == "persistent429"
					if expectFailure {
						if err == nil {
							t.Fatal("attachment read recovered despite terminal status or exhausted retry budget")
						}
						return
					}
					if err != nil {
						t.Fatalf("configured retry failed to recover dependency attachment read: %v", err)
					}
					if fault == "delay503" && time.Duration(recoveredAt.Load()-failedAt.Load()) < delay {
						t.Fatalf("configured retry delay not honored: gap=%v want at least%v", time.Duration(recoveredAt.Load()-failedAt.Load()), delay)
					}
				})
			}
		}
	}
}
