package coopr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/strio-dev/coopr/internal/buildah"
)

func TestLiteralProgramAndDependencies(t *testing.T) {
	literal := `$VALUE "quoted" \\ path`
	p := Program{Platform: "linux/amd64", Output: "result", States: []State{
		{ID: "left", From: Source{Image: "scratch"}, Operations: []Operation{Copy{Sources: []string{literal}, Destination: "/" + literal}}},
		{ID: "right", From: Source{Image: "scratch"}},
		{ID: "result", From: Source{State: "left"}, Operations: []Operation{Env{Name: "VALUE", Value: literal}, Workdir{Path: "/" + literal}, Run{Args: []string{"/bin/tool", literal}, Mounts: []Mount{{Type: "bind", From: Source{State: "right"}, Target: "/input"}}}, Copy{Sources: []string{"/file"}, Destination: "/out", From: Source{State: "right"}}}},
	}}
	plan, err := compile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Stages[2].Dependencies; !reflect.DeepEqual(got, []string{"0", "1"}) {
		t.Fatalf("dependencies %v", got)
	}
	ops := plan.Stages[2].Operations
	if ops[0].Arguments[1] != literal || ops[1].Arguments[0] != "/"+literal || ops[2].Arguments[1] != literal || ops[2].Form != "exec" {
		t.Fatalf("literal values changed: %#v", ops)
	}
	if plan.Stages[0].Operations[0].Arguments[0] != literal {
		t.Fatal("copy path changed")
	}
	if _, _, ok := plan.ExecutionRecipe(); ok {
		t.Fatal("static plan gained frontend recipe")
	}
	again, err := compile(p)
	if err != nil || !reflect.DeepEqual(plan, again) {
		t.Fatal("nondeterministic compilation", err)
	}
	p.States[0].Operations[0].(Copy).Sources[0] = "mutated"
	if plan.Stages[0].Operations[0].Arguments[0] != literal {
		t.Fatal("plan retained caller slices")
	}
}

func TestInvalidPrograms(t *testing.T) {
	cases := []Program{
		{},
		{Output: "a", States: []State{{ID: "a"}}},
		{Output: "a", States: []State{{ID: "a", From: Source{Image: "scratch", State: "a"}}}},
		{Output: "a", States: []State{{ID: "a", From: Source{State: "later"}}, {ID: "later", From: Source{Image: "scratch"}}}},
		{Output: "a", States: []State{{ID: "a", From: Source{Image: "scratch"}}, {ID: "a", From: Source{Image: "scratch"}}}},
		{Output: "a", States: []State{{ID: "a", From: Source{Image: "scratch"}}, {ID: "b", From: Source{Image: "scratch"}}}},
		{Output: "a", States: []State{{ID: "a", From: Source{Image: "scratch"}, Operations: []Operation{Run{}}}}},
		{Output: "a", States: []State{{ID: "a", From: Source{Image: "scratch"}, Operations: []Operation{Run{Args: []string{"true"}, Mounts: []Mount{{Type: "cache", Target: "/cache", Sharing: "invalid"}}}}}}},
	}
	for i, p := range cases {
		if _, err := compile(p); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestExecuteCanceledBeforeValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Execute(ctx, Program{}, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestOutputClosureAndOnBuild(t *testing.T) {
	p, err := compile(Program{Output: "out", States: []State{{ID: "unused", From: Source{Image: "unused:latest"}}, {ID: "base", From: Source{Image: "scratch"}}, {ID: "out", From: Source{State: "base"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Stages) != 2 || len(p.Inputs) != 0 {
		t.Fatalf("unused graph retained: %#v", p)
	}
	for _, stage := range p.Stages {
		if !stage.InheritedOnBuildPlanned {
			t.Fatal("typed API may consume frontend ONBUILD")
		}
	}
}

func TestExecuteRequiresInit(t *testing.T) {
	_, err := Execute(context.Background(), Program{}, Options{})
	if err == nil || !strings.Contains(err.Error(), "InitReexec") {
		t.Fatalf("initialization error: %v", err)
	}
}

func TestNativeCacheMountScopeAndHostInput(t *testing.T) {
	p, err := compile(Program{Output: "out", States: []State{{ID: "out", From: Source{Image: "scratch"}, Operations: []Operation{Run{Args: []string{"true"}, Mounts: []Mount{{Type: "cache", Target: "/cache"}, {Type: "bind", Target: "/context"}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	op := p.Stages[0].Operations[0]
	if op.Children[0].Properties["id"] != "" {
		t.Fatal("adapter invented a cache ID")
	}
	if op.MountContexts[1] != "build" {
		t.Fatal("host bind context is not classified")
	}
}

func TestFreshOutputAndOverlap(t *testing.T) {
	root := t.TempDir()
	if err := freshOutput(root); err == nil {
		t.Fatal("existing output accepted")
	}
	if err := freshOutput("relative"); err == nil {
		t.Fatal("relative output accepted")
	}
	if err := freshOutput(filepath.Join(root, "new")); err != nil {
		t.Fatal(err)
	}
	if err := freshOutput(filepath.Join(root, "missing", "new")); err == nil {
		t.Fatal("missing parent accepted")
	}
	for _, pair := range [][2]string{{"/a", "/a"}, {"/a", "/a/.hidden"}, {"/a/b", "/a"}} {
		if !pathsOverlap(pair[0], pair[1]) {
			t.Fatal("overlap missed", pair)
		}
	}
	if pathsOverlap("/a", "/ab") || pathsOverlap("/a", "/b") {
		t.Fatal("independent outputs overlap")
	}
}

func TestCompiledGraphPassesNativeValidation(t *testing.T) {
	p, err := compile(Program{Output: "out", States: []State{
		{ID: "base", From: Source{Image: "scratch"}},
		{ID: "files", From: Source{Image: "scratch"}},
		{ID: "out", From: Source{State: "base"}, Operations: []Operation{
			Copy{Sources: []string{"/file"}, Destination: "/out", From: Source{State: "files"}},
			Run{Args: []string{"true"}, Mounts: []Mount{{Type: "bind", Target: "/host"}, {Type: "bind", Target: "/image", From: Source{Image: "busybox:latest"}}, {Type: "cache", Target: "/cache"}}},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	// A missing mandatory store root is checked immediately after native graph
	// validation, before workers or native storage can be started.
	_, err = buildah.BuildPlanSupervised(context.Background(), p, buildah.SupervisedPlanOptions{})
	if err == nil || !strings.Contains(err.Error(), "run root is required") {
		t.Fatalf("native graph rejected adapter: %v", err)
	}
}

func TestNumericImageAndBindAccess(t *testing.T) {
	p, err := compile(Program{Output: "out", States: []State{
		{ID: "base", From: Source{Image: "scratch"}},
		{ID: "out", From: Source{Image: "0"}, Operations: []Operation{Run{Args: []string{"true"}, Mounts: []Mount{{Type: "bind", Target: "/write"}, {Type: "bind", Target: "/read", ReadOnly: true}, {Type: "bind", Target: "/state", From: Source{State: "base"}, ReadOnly: true}}}, Copy{Sources: []string{"/x"}, Destination: "/x", From: Source{Image: "0"}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	out := p.Stages[1]
	if out.Source != "0:latest" || len(p.Inputs) != 1 || p.Inputs[0].Reference != "0:latest" {
		t.Fatalf("numeric image conflated with stage: %#v", p)
	}
	if len(out.Dependencies) != 1 || out.Dependencies[0] != "0" {
		t.Fatalf("state reference lost: %v", out.Dependencies)
	}
	if out.Operations[1].Properties["from"] != "0:latest" {
		t.Fatal("copy image reference changed role")
	}
	mounts := out.Operations[0].Children
	if mounts[0].Properties["readonly"] != "false" || mounts[1].Properties["readonly"] != "true" {
		t.Fatalf("bind access lost: %#v", mounts)
	}
}

func TestPromoteFilesystemDoesNotReplaceConcurrentDirectory(t *testing.T) {
	root := t.TempDir()
	source, dest := filepath.Join(root, "source"), filepath.Join(root, "destination")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "owned"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := freshOutput(dest); err != nil {
		t.Fatal(err)
	}
	// Another caller creates an empty directory after successful preflight.
	if err := os.Mkdir(dest, 0700); err != nil {
		t.Fatal(err)
	}
	if err := promoteFilesystem(source, dest); !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "owned")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("concurrent output replaced", err)
	}
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}
	if err := promoteFilesystem(source, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "owned")); err != nil {
		t.Fatal(err)
	}
}

func TestExecuteRemovesFilesystemStagingAfterValidationFailure(t *testing.T) {
	wasInitialized := initialized.Swap(true)
	defer initialized.Store(wasInitialized)
	root := t.TempDir()
	_, err := Execute(context.Background(), Program{Output: "out", States: []State{{ID: "out", From: Source{Image: "scratch"}}}}, Options{Output: Output{OCI: filepath.Join(root, "image"), Filesystem: filepath.Join(root, "rootfs")}})
	if err == nil || !strings.Contains(err.Error(), "run root is required") {
		t.Fatalf("unexpected backend validation: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed execution left staging or outputs: %v", entries)
	}
}
