package oci

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestArtifactRegistryPingChallengeDoesNotEscapeScope(t *testing.T) {
	for _, route := range []string{"token-realm", "same-origin-redirect", "origin-return"} {
		t.Run(route, func(t *testing.T) {
			var sourceURL string
			var denied atomic.Int64
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				http.Redirect(w, request, sourceURL+"/v2/coopr/scope/denied", http.StatusTemporaryRedirect)
			}))
			t.Cleanup(sink.Close)
			sinkURL := strings.Replace(sink.URL, "127.0.0.1", "localhost", 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/v2/" {
					w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if request.URL.Path == "/token" || request.URL.Path == "/v2/coopr/scope/denied" {
					denied.Add(1)
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, "original denial body")
					return
				}
				destination := sourceURL + "/v2/coopr/scope/denied"
				if route == "origin-return" {
					destination = sinkURL + "/forward"
				}
				http.Redirect(w, request, destination, http.StatusTemporaryRedirect)
			}))
			t.Cleanup(server.Close)
			sourceURL = server.URL
			resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), Credentials: "tester:secret", RetrySet: true})
			if err != nil {
				t.Fatal(err)
			}
			repositoryName := strings.TrimPrefix(server.URL, "http://") + "/coopr/scope"
			repository, err := resolver.orasRepository(t.Context(), repositoryName, nil, resolver.SystemContext(), 0)
			if err != nil {
				t.Fatal(err)
			}
			path := "/v2/coopr/scope/start"
			if route == "token-realm" {
				path = "/token"
			}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := repository.Client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != http.StatusUnauthorized || string(data) != "original denial body" || len(response.Header.Values("WWW-Authenticate")) != 0 {
				t.Fatalf("out-of-scope denial was changed: status=%d headers=%v body=%q error=%v", response.StatusCode, response.Header, data, err)
			}
			if denied.Load() != 1 {
				t.Fatalf("out-of-scope denial triggered authentication retry: %d requests", denied.Load())
			}
			if route == "token-realm" {
				return
			}
			denied.Store(0)
			client := baleRegistryTestClient(t, resolver, repositoryName)
			if _, err := client.InspectDependencies(t.Context(), repositoryName+"@sha256:"+strings.Repeat("a", 64)); err == nil {
				t.Fatal("SDK accepted denied redirected manifest")
			}
			if denied.Load() != 1 {
				t.Fatalf("SDK redirected denial triggered authentication retry: %d requests", denied.Load())
			}
		})
	}
}
