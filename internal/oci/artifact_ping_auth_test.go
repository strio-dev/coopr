package oci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
)

func TestArtifactRegistryRepositoryReusesConfiguredConnection(t *testing.T) {
	backend := registryserver.New()
	var realm string
	var pings, tokens, connections atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/token" {
			tokens.Add(1)
			user, password, ok := request.BasicAuth()
			if !ok || user != "tester" || password != "secret" {
				t.Error("token request omitted configured credentials")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "reuse-token", "expires_in": 3600})
			return
		}
		if request.URL.Path == "/v2/" {
			pings.Add(1)
		}
		if request.Header.Get("Authorization") != "Bearer reuse-token" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="reuse-registry",scope="repository:coopr/reuse:pull,push"`, realm))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, request)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	realm = server.URL + "/token"
	resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), Credentials: "tester:secret", RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	target, err := resolver.CacheRepository(strings.TrimPrefix(server.URL, "https://") + "/coopr/reuse")
	if err != nil {
		t.Fatal(err)
	}
	var blobs []v1.Descriptor
	for _, data := range [][]byte{[]byte(`{}`), []byte("verified shared-connection payload")} {
		descriptor := Descriptor(ComponentPackageType, data)
		if err := target.Push(t.Context(), descriptor, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		if exists, err := target.Exists(t.Context(), descriptor); err != nil || !exists {
			t.Fatalf("uploaded blob: exists=%t error=%v", exists, err)
		}
		reader, err := target.Fetch(t.Context(), descriptor)
		if err != nil {
			t.Fatal(err)
		}
		got, err := content.ReadAll(reader, descriptor)
		closeErr := reader.Close()
		if err != nil || closeErr != nil || !bytes.Equal(got, data) {
			t.Fatalf("shared-connection blob integrity: read=%v close=%v", err, closeErr)
		}
		blobs = append(blobs, descriptor)
	}
	data, err := json.Marshal(VersionedManifest(blobs[0], blobs[1:], ComponentArtifactType))
	if err != nil {
		t.Fatal(err)
	}
	manifest := Descriptor(v1.MediaTypeImageManifest, data)
	if err := target.Push(t.Context(), manifest, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := oras.TagBytes(t.Context(), target, manifest.MediaType, data, "stable"); err != nil {
		t.Fatal(err)
	}
	if pings.Load() != 1 || tokens.Load() != 1 || connections.Load() != 1 {
		t.Fatalf("repository initialization was repeated: pings=%d tokenRequests=%d connections=%d; want1each", pings.Load(), tokens.Load(), connections.Load())
	}
	if err := target.Tag(t.Context(), manifest, "alias"); err != nil {
		t.Fatal(err)
	}
	// Native Tag streams its open GET body into a PUT, requiring another HTTP/1
	// connection while preserving the same configured client and auth cache.
	if pings.Load() != 1 || tokens.Load() != 1 || connections.Load() > 2 {
		t.Fatalf("streaming Tag reinitialized the repository: pings=%d tokenRequests=%d connections=%d", pings.Load(), tokens.Load(), connections.Load())
	}
	t.Logf("repository reuse: pings=%d tokenRequests=%d connections=%d (1 before streaming Tag)", pings.Load(), tokens.Load(), connections.Load())
}

func TestArtifactRegistryPingOnlyAuthentication(t *testing.T) {
	for _, scheme := range []string{"Basic", "Bearer"} {
		for _, pingStatus := range []int{http.StatusOK, http.StatusUnauthorized} {
			for _, advertisement := range []string{"single", "unsupported-first"} {
				for _, mode := range []string{"direct", "layout"} {
					t.Run(fmt.Sprintf("%s/ping-status=%d/%s/%s", scheme, pingStatus, advertisement, mode), func(t *testing.T) {
						backend := registryserver.New()
						var realm string
						var authorized, tokenRequests atomic.Int64
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
							if request.URL.Path == "/token" {
								tokenRequests.Add(1)
								user, password, ok := request.BasicAuth()
								if !ok || user != "tester" || password != "secret" {
									http.Error(w, "invalid token credentials", http.StatusUnauthorized)
									return
								}
								w.Header().Set("Content-Type", "application/json")
								_ = json.NewEncoder(w).Encode(map[string]any{"token": "ping-token", "expires_in": 3600})
								return
							}
							if request.URL.Path == "/v2/" {
								challenge := `Basic realm="registry"`
								if scheme == "Bearer" {
									challenge = fmt.Sprintf(`Bearer realm=%q,service="ping-registry"`, realm)
								}
								if advertisement == "unsupported-first" {
									w.Header().Add("WWW-Authenticate", "Negotiate")
								}
								w.Header().Add("WWW-Authenticate", challenge)
								http.Error(w, "registry authentication required", pingStatus)
								return
							}
							valid := request.Header.Get("Authorization") == "Bearer ping-token"
							if scheme == "Basic" {
								user, password, ok := request.BasicAuth()
								valid = ok && user == "tester" && password == "secret"
							}
							if !valid {
								http.Error(w, "resource authentication required", http.StatusUnauthorized)
								return
							}
							authorized.Add(1)
							backend.ServeHTTP(w, request)
						}))
						t.Cleanup(server.Close)
						realm = server.URL + "/token"
						resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), Credentials: "tester:secret", RetrySet: true})
						if err != nil {
							t.Fatal(err)
						}
						metadata, paths := baleComponentMetadata(t, "amd64", "ping-only", baleRegistryDependency)
						reference := strings.TrimPrefix(server.URL, "http://") + "/coopr/ping-only:stable"
						root, err := publishBaleRegistryComponent(t, mode, resolver, reference, metadata, paths)
						if err != nil {
							t.Fatalf("publish using ping-only %s challenge: %v", scheme, err)
						}
						resolved, err := resolver.Resolve(t.Context(), reference, metadata.Platform, Component)
						if err != nil || !content.Equal(resolved.Root, root) {
							t.Fatalf("resolve using ping-only %s challenge: %+v, %v", scheme, resolved, err)
						}
						if authorized.Load() == 0 || (scheme == "Bearer" && tokenRequests.Load() == 0) {
							t.Fatal("configured ping authentication was not exercised")
						}
					})
				}
			}
		}
	}
}
