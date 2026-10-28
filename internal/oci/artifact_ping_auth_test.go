package oci

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	"oras.land/oras-go/v2/content"
)

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
