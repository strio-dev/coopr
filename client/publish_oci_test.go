package client

import (
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/strio-dev/coopr/api"
)

func TestPublishOCILayoutCapability(t *testing.T) {
	for _, source := range []string{"native-id", "oci:/absolute/layout"} {
		r := api.Request{Kind: "publish", Publish: &api.Publish{Artifact: "image", Source: source}}
		required, err := requestCapabilities(r)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(required, api.CapabilityPublishOCILayout) != strings.HasPrefix(source, "oci:") {
			t.Fatalf("%s: %v", source, required)
		}
	}
	var posts atomic.Int32
	c := testClient(t, allInfo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts.Add(1); w.WriteHeader(500) }))
	_, err := c.Execute(t.Context(), api.Request{Kind: "publish", Publish: &api.Publish{Artifact: "image", Source: "oci:/absolute/layout", SourceDigest: "sha256:" + strings.Repeat("a", 64)}}, StreamOptions{})
	if err == nil || !strings.Contains(err.Error(), api.CapabilityPublishOCILayout) || posts.Load() != 0 {
		t.Fatalf("unsupported layout reached POST: posts=%d err=%v", posts.Load(), err)
	}
}
