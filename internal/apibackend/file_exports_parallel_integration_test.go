package apibackend

import (
	"path/filepath"
	"testing"

	"github.com/strio-dev/coopr/api"
	"github.com/strio-dev/coopr/client"
)

func TestPublicClientNativeSelectedFileExportsFromParallelStages(t *testing.T) {
	f := newNativeAPIFixture(t)
	c, _ := publicNativeClient(t)
	r := f.request("parallel-exports", "", "shared")
	r.Options.Output.OCI, r.Options.Output.Filesystem = "", ""
	r.Options.Network, r.Options.Isolation, r.Options.Jobs = "none", "rootless", 2
	publicNativeWrite(t, filepath.Join(f.context, "left"), "left\n", 0600)
	publicNativeWrite(t, filepath.Join(f.context, "right"), "right\n", 0600)
	// Both scratch states are independently eligible, so Jobs=2 sends their
	// work through the isolated stage workers rather than the in-process path.
	r.Program = &api.Program{Output: "right", States: []api.State{
		{ID: "left", From: api.Source{Image: "scratch"}, Operations: []api.Operation{{Kind: "copy", Sources: []string{"left"}, Destination: "/report"}}},
		{ID: "right", From: api.Source{Image: "scratch"}, Operations: []api.Operation{{Kind: "copy", Sources: []string{"right"}, Destination: "/report"}}},
	}}
	r.Options.Output.Exports = []api.FileExport{
		{Source: "/report", Destination: filepath.Join(f.root, "right-report")},
		{State: "left", Source: "/report", Destination: filepath.Join(f.root, "left-report")},
	}
	result, err := c.Execute(t.Context(), r, client.StreamOptions{})
	if err != nil || len(result.Exports) != len(r.Options.Output.Exports) {
		t.Fatalf("parallel export receipts: %+v, %v", result.Exports, err)
	}
	for i, want := range r.Options.Output.Exports {
		got := result.Exports[i]
		if got.State != want.State || got.Source != want.Source || got.Destination != want.Destination || got.Status != "passed" || got.Error != "" {
			t.Fatalf("export %d did not preserve its requested path and order: %+v", i, got)
		}
		if data := publicNativeRead(t, got.Destination); data != []string{"right\n", "left\n"}[i] {
			t.Fatalf("export %d contains another stage's bytes: %q", i, data)
		}
	}
}
