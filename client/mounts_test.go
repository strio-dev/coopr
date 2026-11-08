package client

import (
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/strio-dev/coopr/api"
)

func TestRunMountFeatureNegotiation(t *testing.T) {
	for _, mount := range []api.Mount{
		{Type: "bind", HostPath: "/host", Target: "/live"},
		{Type: "bind", From: api.Source{Image: "registry.example/files@sha256:0000000000000000000000000000000000000000000000000000000000000000"}, Target: "/artifact"},
		{Type: "bind", From: api.Source{Context: "assets"}, Target: "/assets"},
	} {
		required, err := requestCapabilities(api.Request{Kind: "execute", Program: &api.Program{States: []api.State{{Operations: []api.Operation{{Kind: "run", Mounts: []api.Mount{mount}}}}}}})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(required, api.CapabilityRunMounts) {
			t.Fatalf("new mount capability omitted: %v", required)
		}
	}
}

func TestArtifactRunMountRequiresCapabilityBeforePOST(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, allInfo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	request := execution()
	request.Program.States[0].Operations = []api.Operation{{Kind: "run", Args: []string{"true"}, Mounts: []api.Mount{{Type: "bind", From: api.Source{Image: "registry.example/assets@sha256:0000000000000000000000000000000000000000000000000000000000000000"}, Target: "/files", ReadOnly: true}}}}
	if _, err := c.Execute(t.Context(), request, StreamOptions{}); err == nil || !strings.Contains(err.Error(), api.CapabilityRunMounts) {
		t.Fatalf("missing artifact mount capability not reported: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("unsupported mount reached service: %d requests", calls.Load())
	}
}
