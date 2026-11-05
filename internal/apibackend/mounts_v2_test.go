package apibackend

import (
	"os"
	"path/filepath"
	"testing"

	coopr "github.com/strio-dev/coopr"
	"github.com/strio-dev/coopr/api"
)

func TestNamedRunContextAndLiveHostMountReachPublicProgram(t *testing.T) {
	r := request(t)
	input := filepath.Join(filepath.Dir(r.Options.ContextDir), "mount-input")
	if err := os.Mkdir(input, 0700); err != nil {
		t.Fatal(err)
	}
	r.Options.Contexts = map[string]string{"assets": input}
	r.Program.States[0].Operations = []api.Operation{{Kind: "run", Args: []string{"true"}, Mounts: []api.Mount{
		{Type: "bind", Target: "/assets", From: api.Source{Context: "assets"}, ReadOnly: true},
		{Type: "bind", Target: "/live", HostPath: input},
	}}}
	if err := validate(r); err != nil {
		t.Fatal(err)
	}
	p, err := program(*r.Program)
	if err != nil {
		t.Fatal(err)
	}
	run := p.States[0].Operations[0].(coopr.Run)
	if run.Mounts[0].From.Context != "assets" || run.Mounts[1].HostPath != input {
		t.Fatalf("mount inputs lost: %#v", run.Mounts)
	}
	r.Program.States[0].Operations[0].Mounts[0].From.Context = "unknown"
	if err := validate(r); err == nil {
		t.Fatal("unknown named RUN context accepted")
	}
	r.Program.States[0].Operations[0].Mounts[0].From.Context = "assets"
	for _, invalid := range []string{"relative", filepath.Join(input, "missing"), r.Options.Store.GraphRoot} {
		r.Program.States[0].Operations[0].Mounts[1].HostPath = invalid
		if err := validate(r); err == nil {
			t.Fatalf("invalid host mount %q accepted", invalid)
		}
	}
}
