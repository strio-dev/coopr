package coopr

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/strio-dev/coopr/internal/buildcontext"
	"github.com/strio-dev/coopr/internal/planner"
)

func TestTypedNamedContextAndLiveHostRunMounts(t *testing.T) {
	root := t.TempDir()
	program := Program{Output: "out", States: []State{{ID: "out", From: Source{Image: "scratch"}, Operations: []Operation{Run{Args: []string{"true"}, Mounts: []Mount{
		{Type: "bind", Target: "/snapshot", From: Source{Context: "assets"}, Source: "/", ReadOnly: true},
		{Type: "bind", Target: "/live", HostPath: root, ReadOnly: false},
	}}}}}}
	spec := buildcontext.Spec{Name: "assets", Kind: buildcontext.Local, Path: root}
	plan, err := compile(program, spec)
	if err != nil {
		t.Fatal(err)
	}
	op := plan.Stages[0].Operations[0]
	if op.MountContexts[0] != "assets" || op.HostMounts[1] != root || op.MountContexts[1] != "" {
		t.Fatalf("source kinds conflated: %#v", op)
	}
	want := []planner.ContextBinding{{Role: "run-mount", StageID: "0", Operation: 0, MountIndex: 0}}
	if len(plan.Contexts) != 1 || !reflect.DeepEqual(plan.Contexts[0].Bindings, want) {
		t.Fatalf("context bindings = %#v", plan.Contexts)
	}
	run := program.States[0].Operations[0].(Run)
	for _, mount := range []Mount{
		{Type: "cache", Target: "/cache", Sharing: "private"},
		{Type: "bind", Target: "/live", HostPath: root, Relabel: "unknown"},
		{Type: "bind", Target: "/snapshot", Relabel: "shared"},
		{Type: "bind", Target: "/live", HostPath: "relative"},
		{Type: "bind", Target: "/live", HostPath: filepath.Join(root, "missing")},
		{Type: "bind", Target: "/live", HostPath: root, Source: "/subpath"},
		{Type: "bind", Target: "/live", HostPath: root, From: Source{Context: "assets"}},
		{Type: "bind", Target: "/input", From: Source{Context: "unknown"}},
		{Type: "bind", Target: "/input", From: Source{Context: "assets", Image: "image"}},
	} {
		run.Mounts = []Mount{mount}
		program.States[0].Operations[0] = run
		if _, err := compile(program, spec); err == nil {
			t.Fatalf("invalid mount accepted: %#v", mount)
		}
	}
}
