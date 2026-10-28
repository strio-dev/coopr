package oci

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	"oras.land/oras-go/v2/content"
)

func TestArtifactRegistryNetworkManifestReadRetry(t *testing.T) {
	for _, fault := range []string{"eof", "reset", "body-reset"} {
		for _, retries := range []uint{0, 1} {
			t.Run(fmt.Sprintf("%s/retries=%d", fault, retries), func(t *testing.T) {
				backend := registryserver.New()
				var failNext atomic.Bool
				var failures atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					// Keep each request on a fresh connection so net/http's automatic
					// replay of a reused idempotent connection does not hide the
					// configured retry budget being tested.
					w.Header().Set("Connection", "close")
					if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/manifests/") && failNext.CompareAndSwap(true, false) {
						failures.Add(1)
						if fault == "body-reset" {
							manifest := httptest.NewRecorder()
							backend.ServeHTTP(manifest, request)
							for name, values := range manifest.Header() {
								w.Header()[name] = values
							}
							w.Header().Set("Content-Length", fmt.Sprint(manifest.Body.Len()))
							w.WriteHeader(manifest.Code)
							_, _ = w.Write(manifest.Body.Bytes()[:manifest.Body.Len()/2])
							w.(http.Flusher).Flush()
						}
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Errorf("hijack failing connection: %v", err)
							return
						}
						if fault == "reset" || fault == "body-reset" {
							if err := connection.(*net.TCPConn).SetLinger(0); err != nil {
								t.Errorf("set failing connection reset: %v", err)
							}
						}
						_ = connection.Close()
						return
					}
					backend.ServeHTTP(w, request)
				}))
				t.Cleanup(server.Close)
				resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true, Retry: retries, RetryDelay: time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				metadata, paths := baleComponentMetadata(t, "amd64", "network-response")
				reference := strings.TrimPrefix(server.URL, "http://") + "/coopr/network:stable"
				root, err := resolver.PublishComponent(t.Context(), reference, metadata, paths)
				if err != nil {
					t.Fatal(err)
				}
				failNext.Store(true)
				resolved, err := resolver.Resolve(t.Context(), reference, metadata.Platform, Component)
				if failures.Load() != 1 {
					t.Fatal("failed connection was not exercised exactly once")
				}
				if retries == 0 {
					if err == nil {
						t.Fatal("network failure recovered with zero retry budget")
					}
					return
				}
				if err != nil || !content.Equal(resolved.Root, root) {
					t.Fatalf("configured retry did not recover failed manifest connection: %+v, %v", resolved, err)
				}
			})
		}
	}
}

func TestArtifactRegistryIdentityTokenRefresh(t *testing.T) {
	backend := registryserver.New()
	var realm string
	var tokenRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/token" {
			tokenRequests.Add(1)
			if err := request.ParseForm(); err != nil || request.Method != http.MethodPost || request.Form.Get("grant_type") != "refresh_token" || request.Form.Get("refresh_token") != "refresh-secret" {
				t.Errorf("identity token was not used as an OAuth refresh token: method=%s form=%v error=%v", request.Method, request.Form, err)
				http.Error(w, "invalid refresh token exchange", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "refreshed-token", "token": "refreshed-token", "expires_in": 3600})
			return
		}
		if request.Header.Get("Authorization") != "Bearer refreshed-token" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="test-refresh",scope="repository:coopr/refresh:pull,push"`, realm))
			http.Error(w, "bearer token required", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, request)
	}))
	t.Cleanup(server.Close)
	realm = server.URL + "/token"
	host := strings.TrimPrefix(server.URL, "http://")
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, []byte(fmt.Sprintf(`{"auths":{%q:{"auth":"Og==","identitytoken":"refresh-secret"}}}`, host)), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := testResolver(t, Options{AuthFile: authFile, TLSVerify: boolOption(false), RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	metadata, paths := baleComponentMetadata(t, "amd64", "refresh-token", baleRegistryDependency)
	reference := host + "/coopr/refresh:stable"
	root, err := resolver.PublishComponent(t.Context(), reference, metadata, paths)
	if err != nil {
		t.Fatalf("publish using identity-token refresh: %v", err)
	}
	resolved, err := resolver.Resolve(t.Context(), reference, metadata.Platform, Component)
	if err != nil || !content.Equal(resolved.Root, root) {
		t.Fatalf("resolve using identity-token refresh: %+v, %v", resolved, err)
	}
	if tokenRequests.Load() == 0 {
		t.Fatal("identity-token refresh endpoint was never used")
	}
}

func TestArtifactRegistryConfiguredBearerTokenUsesPingChallenge(t *testing.T) {
	for _, pingStatus := range []int{http.StatusOK, http.StatusUnauthorized} {
		t.Run(fmt.Sprintf("ping-status=%d", pingStatus), func(t *testing.T) {
			backend := registryserver.New()
			var realm string
			var tokenRequests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/unused-token" {
					tokenRequests.Add(1)
					http.Error(w, "configured token should bypass token service", http.StatusInternalServerError)
					return
				}
				if request.URL.Path == "/v2/" {
					if request.Header.Get("Authorization") != "" {
						t.Error("configured bearer token leaked into unauthenticated registry ping")
					}
					w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="test-token"`, realm))
					http.Error(w, "bearer token required", pingStatus)
					return
				}
				if request.Header.Get("Authorization") != "Bearer configured-token" {
					// The resource does not repeat the ping's authentication challenge.
					http.Error(w, "configured bearer token required", http.StatusUnauthorized)
					return
				}
				backend.ServeHTTP(w, request)
			}))
			t.Cleanup(server.Close)
			realm = server.URL + "/unused-token"
			resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true})
			if err != nil {
				t.Fatal(err)
			}
			resolver.system.DockerBearerRegistryToken = "configured-token"
			metadata, paths := baleComponentMetadata(t, "amd64", "configured-token", baleRegistryDependency)
			reference := strings.TrimPrefix(server.URL, "http://") + "/coopr/token:stable"
			root, err := resolver.PublishComponent(t.Context(), reference, metadata, paths)
			if err != nil {
				t.Fatalf("publish using configured bearer token: %v", err)
			}
			resolved, err := resolver.Resolve(t.Context(), reference, metadata.Platform, Component)
			if err != nil || !content.Equal(resolved.Root, root) {
				t.Fatalf("resolve using configured bearer token: %+v, %v", resolved, err)
			}
			if tokenRequests.Load() != 0 {
				t.Fatal("configured bearer token unexpectedly invoked the token service")
			}
		})
	}
}

func TestArtifactRegistryConfiguredUserAgent(t *testing.T) {
	const agent = "Coopr registry compatibility test"
	backend := registryserver.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.UserAgent() != agent {
			http.Error(w, "configured user agent required", http.StatusForbidden)
			return
		}
		backend.ServeHTTP(w, request)
	}))
	t.Cleanup(server.Close)
	resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	resolver.system.DockerRegistryUserAgent = agent
	metadata, paths := baleComponentMetadata(t, "amd64", "user-agent", baleRegistryDependency)
	reference := strings.TrimPrefix(server.URL, "http://") + "/coopr/user-agent:stable"
	root, err := resolver.PublishComponent(t.Context(), reference, metadata, paths)
	if err != nil {
		t.Fatalf("publish with configured user agent: %v", err)
	}
	resolved, err := resolver.Resolve(t.Context(), reference, metadata.Platform, Component)
	if err != nil || !content.Equal(resolved.Root, root) {
		t.Fatalf("resolve with configured user agent: %+v, %v", resolved, err)
	}
}

func TestArtifactRegistryProxyCallbackOverridesProxyURL(t *testing.T) {
	for _, callback := range []bool{false, true} {
		t.Run(fmt.Sprintf("callback=%t", callback), func(t *testing.T) {
			server := httptest.NewServer(registryserver.New())
			t.Cleanup(server.Close)
			var proxyRequests, callbackCalls atomic.Int64
			badProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				proxyRequests.Add(1)
				http.Error(w, "proxy unavailable", http.StatusServiceUnavailable)
			}))
			t.Cleanup(badProxy.Close)
			proxyURL, err := url.Parse(badProxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true})
			if err != nil {
				t.Fatal(err)
			}
			resolver.system.DockerProxyURL = proxyURL
			if callback {
				resolver.system.DockerProxy = func(*url.URL) (*url.URL, error) {
					callbackCalls.Add(1)
					return nil, nil
				}
			}
			metadata, paths := baleComponentMetadata(t, "amd64", "proxy-precedence", baleRegistryDependency)
			reference := strings.TrimPrefix(server.URL, "http://") + "/coopr/proxy:stable"
			_, err = resolver.PublishComponent(t.Context(), reference, metadata, paths)
			if callback {
				if err != nil || callbackCalls.Load() == 0 || proxyRequests.Load() != 0 {
					t.Fatalf("proxy callback did not override URL: callbacks=%d proxy=%d error=%v", callbackCalls.Load(), proxyRequests.Load(), err)
				}
			} else if err == nil || proxyRequests.Load() == 0 {
				t.Fatalf("configured proxy URL was not used: requests=%d error=%v", proxyRequests.Load(), err)
			}
		})
	}
}

func TestArtifactRegistryBaseTLSConfigAlgorithms(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(tls.VersionName(version), func(t *testing.T) {
			server := httptest.NewUnstartedServer(registryserver.New())
			server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12}
			server.StartTLS()
			t.Cleanup(server.Close)
			certDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(certDir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
				t.Fatal(err)
			}
			resolver, err := testResolver(t, Options{CertDir: certDir, TLSVerify: boolOption(true), RetrySet: true})
			if err != nil {
				t.Fatal(err)
			}
			base := &tls.Config{MinVersion: version, MaxVersion: version}
			resolver.system.BaseTLSConfig = base
			metadata, paths := baleComponentMetadata(t, "amd64", "tls-version")
			reference := strings.TrimPrefix(server.URL, "https://") + "/coopr/tls-version:stable"
			_, err = resolver.PublishComponent(t.Context(), reference, metadata, paths)
			if (err == nil) != (version == tls.VersionTLS12) {
				t.Fatalf("configured TLS version was ignored: %v", err)
			}
			if base.MinVersion != version || base.MaxVersion != version || base.RootCAs != nil || base.InsecureSkipVerify {
				t.Fatal("registry client mutated caller's base TLS configuration")
			}
		})
	}
}

func TestArtifactRegistryManifestReadCancellation(t *testing.T) {
	backend := registryserver.New()
	var armed atomic.Bool
	var enteredOnce sync.Once
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if armed.Load() && request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/manifests/") {
			enteredOnce.Do(func() { close(entered) })
			<-request.Context().Done()
			return
		}
		backend.ServeHTTP(w, request)
	}))
	t.Cleanup(server.Close)
	resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true, Retry: 1, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	metadata, paths := baleComponentMetadata(t, "amd64", "cancellation")
	reference := strings.TrimPrefix(server.URL, "http://") + "/coopr/cancellation:stable"
	if _, err := resolver.PublishComponent(t.Context(), reference, metadata, paths); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	armed.Store(true)
	go func() {
		_, err := resolver.Resolve(ctx, reference, metadata.Platform, Component)
		result <- err
	}()
	select {
	case <-entered:
		cancel()
	case err := <-result:
		t.Fatalf("manifest read did not enter the blocking request: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("manifest read never reached the server")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation identity was lost: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled manifest read did not return")
	}
}

func TestArtifactRegistryConfiguredBearerRedirectScope(t *testing.T) {
	for _, test := range []struct {
		name           string
		crossOrigin    bool
		returnToOrigin bool
	}{
		{name: "same-origin"},
		{name: "cross-origin", crossOrigin: true},
		{name: "return-to-origin", crossOrigin: true, returnToOrigin: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := registryserver.New()
			var redirects atomic.Bool
			var observed atomic.Int64
			var serverURL, sinkURL string
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				observed.Add(1)
				if request.Header.Get("Authorization") != "" {
					t.Error("configured bearer token leaked across registry origin")
				}
				if test.returnToOrigin {
					http.Redirect(w, request, serverURL+request.URL.Path+"?redirected=yes&returned=yes", http.StatusTemporaryRedirect)
					return
				}
				backend.ServeHTTP(w, request)
			}))
			t.Cleanup(sink.Close)
			sinkURL = strings.Replace(sink.URL, "127.0.0.1", "localhost", 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/v2/" {
					w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="redirect-scope"`, serverURL+"/unused-token"))
					http.Error(w, "bearer token required", http.StatusUnauthorized)
					return
				}
				if request.URL.Query().Has("redirected") {
					observed.Add(1)
					expected := "Bearer configured-token"
					if request.URL.Query().Has("returned") {
						expected = ""
					}
					if request.Header.Get("Authorization") != expected {
						t.Errorf("redirected authorization=%q, want %q", request.Header.Get("Authorization"), expected)
					}
					backend.ServeHTTP(w, request)
					return
				}
				if request.Header.Get("Authorization") != "Bearer configured-token" {
					http.Error(w, "configured token required", http.StatusUnauthorized)
					return
				}
				if redirects.Load() && request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/manifests/") {
					destination := serverURL
					if test.crossOrigin {
						destination = sinkURL
					}
					http.Redirect(w, request, destination+request.URL.Path+"?redirected=yes", http.StatusTemporaryRedirect)
					return
				}
				backend.ServeHTTP(w, request)
			}))
			t.Cleanup(server.Close)
			serverURL = server.URL
			resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true})
			if err != nil {
				t.Fatal(err)
			}
			resolver.system.DockerBearerRegistryToken = "configured-token"
			metadata, paths := baleComponentMetadata(t, "amd64", "redirect-token", baleRegistryDependency)
			repository := strings.TrimPrefix(server.URL, "http://") + "/coopr/token-scope"
			root, err := resolver.PublishComponent(t.Context(), repository+":stable", metadata, paths)
			if err != nil {
				t.Fatal(err)
			}
			redirects.Store(true)
			if _, err := resolver.Resolve(t.Context(), repository+":stable", metadata.Platform, Component); err != nil {
				t.Fatalf("resolve redirected manifest: %v", err)
			}
			before := observed.Load()
			if before == 0 {
				t.Fatal("raw artifact read did not follow registry redirect")
			}
			client := baleRegistryTestClient(t, resolver, repository)
			if _, err := client.InspectDependencies(t.Context(), repository+"@"+root.Digest.String()); err != nil {
				t.Fatalf("inspect redirected dependency declaration: %v", err)
			}
			if observed.Load() <= before {
				t.Fatal("Bale SDK read did not follow registry redirect")
			}
		})
	}
}

func TestArtifactRegistryManifestErrorListRetryClassification(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		codes    []string
		attempts int64
	}{
		{name: "unknown-denied", status: http.StatusServiceUnavailable, codes: []string{"UNKNOWN", "DENIED"}, attempts: 2},
		{name: "unknown-unauthorized", status: http.StatusServiceUnavailable, codes: []string{"UNKNOWN", "UNAUTHORIZED"}, attempts: 2},
		{name: "all-unknown", status: http.StatusServiceUnavailable, codes: []string{"UNKNOWN", "UNKNOWN"}, attempts: 2},
		{name: "denied-unknown", status: http.StatusServiceUnavailable, codes: []string{"DENIED", "UNKNOWN"}, attempts: 2},
		{name: "unauthorized-unknown", status: http.StatusServiceUnavailable, codes: []string{"UNAUTHORIZED", "UNKNOWN"}, attempts: 2},
		{name: "400-denied-unknown", status: http.StatusBadRequest, codes: []string{"DENIED", "UNKNOWN"}, attempts: 1},
		{name: "400-unauthorized-unknown", status: http.StatusBadRequest, codes: []string{"UNAUTHORIZED", "UNKNOWN"}, attempts: 1},
		{name: "400-unknown-denied", status: http.StatusBadRequest, codes: []string{"UNKNOWN", "DENIED"}, attempts: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := registryserver.New()
			var armed atomic.Bool
			var attempts atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if armed.Load() && request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/manifests/") {
					attempts.Add(1)
					entries := make([]map[string]string, len(test.codes))
					for i, code := range test.codes {
						entries[i] = map[string]string{"code": code, "message": "test registry failure"}
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(test.status)
					_ = json.NewEncoder(w).Encode(map[string]any{"errors": entries})
					return
				}
				backend.ServeHTTP(w, request)
			}))
			t.Cleanup(server.Close)
			resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true, Retry: 1, RetryDelay: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			metadata, paths := baleComponentMetadata(t, "amd64", "error-list")
			reference := strings.TrimPrefix(server.URL, "http://") + "/coopr/error-list:stable"
			if _, err := resolver.PublishComponent(t.Context(), reference, metadata, paths); err != nil {
				t.Fatal(err)
			}
			armed.Store(true)
			if _, err := resolver.Resolve(t.Context(), reference, metadata.Platform, Component); err == nil {
				t.Fatal("failed manifest unexpectedly resolved")
			}
			if attempts.Load() != test.attempts {
				t.Fatalf("manifest attempts=%d, want %d for native primary registry error", attempts.Load(), test.attempts)
			}
		})
	}
}
