package oci

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestArtifactRegistryNamespaceProxyPreservesInitialQuery(t *testing.T) {
	var observed atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v2/" {
			observed.Add(1)
			if got := request.URL.Query()["existing"]; !slices.Equal(got, []string{"one", "two"}) {
				t.Errorf("existing query values changed: %v", got)
			}
			if request.URL.Path == "/v2/coopr/query/probe" {
				if got := request.URL.Query()["ns"]; !slices.Equal(got, []string{"source.example"}) {
					t.Errorf("namespace query was not set exactly once: %v", got)
				}
				http.Redirect(w, request, "/v2/coopr/query/redirected?existing=one&existing=two", http.StatusTemporaryRedirect)
				return
			}
			if request.URL.Path != "/v2/coopr/query/redirected" || request.URL.Query().Has("ns") {
				t.Errorf("namespace was injected into a same-origin registry redirect: %s", request.URL)
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(proxy.Close)
	host := strings.TrimPrefix(proxy.URL, "http://")
	configPath := filepath.Join(t.TempDir(), "registries.conf")
	configuration := fmt.Sprintf("[[registry]]\nlocation = \"source.example\"\n[[registry.mirror]]\nlocation = %q\ninsecure = true\nnamespace-proxy = \"source.example\"\n", host)
	if err := os.WriteFile(configPath, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := testResolver(t, Options{RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	resolver.system.SystemRegistriesConfPath = configPath
	resolver.system.SystemRegistriesConfDirPath = t.TempDir()
	ref, err := ParseReference("source.example/coopr/query:stable")
	if err != nil {
		t.Fatal(err)
	}
	target, err := resolver.repository(ref)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := target.readRepositories("stable")
	if err != nil || len(sources) == 0 {
		t.Fatalf("namespace proxy sources: %v", err)
	}
	repository, err := sources[0](t.Context())
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, proxy.URL+"/v2/coopr/query/probe?existing=one&existing=two&ns=wrong", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := repository.Client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || observed.Load() != 2 {
		t.Fatalf("namespace query request: status=%d, observed=%d", response.StatusCode, observed.Load())
	}
}
