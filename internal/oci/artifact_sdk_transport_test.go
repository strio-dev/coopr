package oci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func artifactSDKConnection(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()
	resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true, Retry: 1, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	client, _, _, err := resolver.orasConnection(t.Context(), strings.TrimPrefix(server.URL, "http://")+"/coopr/sdk", nil, resolver.SystemContext(), 1)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestArtifactRegistrySDKMetadataBodyLifecycle(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		for _, failure := range []string{"advertised-size", "chunked-size", "cancel"} {
			t.Run(fmt.Sprintf("status=%d/%s", status, failure), func(t *testing.T) {
				entered, closed := make(chan struct{}), make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					if request.URL.Path == "/v2/" {
						w.WriteHeader(http.StatusOK)
						return
					}
					w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
					switch failure {
					case "advertised-size":
						w.Header().Set("Content-Length", fmt.Sprint(maxMetadataBytes+1))
					case "cancel":
						w.Header().Set("Content-Length", "1024")
					}
					w.WriteHeader(status)
					w.(http.Flusher).Flush()
					if failure == "chunked-size" {
						_, _ = io.CopyN(w, zeroReader{}, maxMetadataBytes+2)
						w.(http.Flusher).Flush()
					} else {
						_, _ = io.WriteString(w, "{}")
						w.(http.Flusher).Flush()
					}
					close(entered)
					<-request.Context().Done()
					close(closed)
				}))
				t.Cleanup(server.Close)
				client := artifactSDKConnection(t, server)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v2/coopr/sdk/manifests/stable", nil)
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() {
					response, err := client.Do(request)
					if response != nil {
						_ = response.Body.Close()
					}
					done <- err
				}()
				if failure == "cancel" {
					select {
					case <-entered:
					case <-time.After(5 * time.Second):
						t.Fatal("metadata body did not open")
					}
					cancel()
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("hostile metadata was accepted")
					}
					if failure == "cancel" && !errors.Is(err, context.Canceled) {
						t.Fatalf("metadata cancellation: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("metadata request did not finish")
				}
				select {
				case <-closed:
				case <-time.After(5 * time.Second):
					t.Fatal("metadata body connection was not closed")
				}
			})
		}
	}
}

func TestArtifactRegistrySDKMetadataResponsesRemainReadable(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			data := `{"schemaVersion":2}`
			if status == http.StatusForbidden {
				data = `{"errors":[{"code":"DENIED","message":"original denial"}]}`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/v2/" {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Original-Header", "preserved")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, data)
			}))
			t.Cleanup(server.Close)
			response, err := artifactSDKConnection(t, server).Get(server.URL + "/v2/coopr/sdk/manifests/stable")
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if err != nil || closeErr != nil || string(got) != data || response.StatusCode != status || response.Header.Get("X-Original-Header") != "preserved" {
				t.Fatalf("metadata response changed: status=%d body=%q read=%v close=%v", response.StatusCode, got, err, closeErr)
			}
		})
	}
}

func TestArtifactRegistrySDKBlobBodyRemainsStreaming(t *testing.T) {
	release, entered := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(16<<20))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "body")
		w.(http.Flusher).Flush()
		close(entered)
		<-release
		_, _ = io.CopyN(w, zeroReader{}, (16<<20)-4)
	}))
	t.Cleanup(server.Close)
	client := artifactSDKConnection(t, server)
	type result struct {
		response *http.Response
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, err := client.Get(server.URL + "/v2/coopr/sdk/blobs/sha256:" + strings.Repeat("a", 64))
		done <- result{response: response, err: err}
	}()
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("blob request did not open")
	}
	var response *http.Response
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		response = got.response
	case <-time.After(5 * time.Second):
		t.Fatal("SDK eagerly buffered blob payload")
	}
	if response == nil {
		t.Fatal("no blob response")
	}
	close(release)
	released = true
	size, err := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil || size != 16<<20 {
		t.Fatalf("blob stream: size=%d read=%v close=%v", size, err, closeErr)
	}
}
