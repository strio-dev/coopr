package client

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/strio-dev/coopr/api"
)

func TestBuildPlatformCapabilityIsConditional(t *testing.T) {
	for _, kind := range []string{"build", "component"} {
		for _, platform := range []string{"", "linux/arm64"} {
			required, err := requestCapabilities(api.Request{Kind: kind, Build: &api.Build{Platform: platform}})
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(required, api.CapabilityBuildPlatform) != (platform != "") {
				t.Fatalf("%s %q capabilities=%v", kind, platform, required)
			}
		}
	}
	request := execution()
	request.Program.Platform = "linux/arm64"
	required, err := requestCapabilities(request)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(required, api.CapabilityBuildPlatform) {
		t.Fatal("typed literal program acquired native build capability")
	}
}

func TestBuildPlatformRequiresCapabilityBeforePOST(t *testing.T) {
	var posts atomic.Int32
	c := testClient(t, allInfo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts.Add(1); w.WriteHeader(500) }))
	_, err := c.Execute(t.Context(), api.Request{Kind: "build", Build: &api.Build{Files: []string{"image.coopr"}, Platform: "linux/arm64"}}, StreamOptions{})
	if err == nil || !strings.Contains(err.Error(), api.CapabilityBuildPlatform) {
		t.Fatalf("missing capability error: %v", err)
	}
	if posts.Load() != 0 {
		t.Fatalf("unsupported platform reached POST: %d", posts.Load())
	}
}

func TestBuildPlatformCapabilityPermitsWireField(t *testing.T) {
	info := strings.Replace(allInfo, `"registry-cache"`, `"registry-cache","build-platform"`, 1)
	var received api.Request
	c := testClient(t, info, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		encode(t, w, api.Event{Type: "result", Result: &api.Result{Platform: "linux/arm64"}})
	}))
	result, err := c.Execute(t.Context(), api.Request{Kind: "build", Build: &api.Build{Files: []string{"image.coopr"}, Platform: "linux/arm64"}}, StreamOptions{})
	if err != nil || received.Build == nil || received.Build.Platform != "linux/arm64" || result.Platform != "linux/arm64" {
		t.Fatalf("platform wire/result changed: %+v %+v %v", received, result, err)
	}
}
