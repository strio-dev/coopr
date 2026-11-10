package coopr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/buildcontext"
	"github.com/strio-dev/coopr/internal/planner"
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

func TestPublicCleanupFailureSurvivesWorkerErrorRelay(t *testing.T) {
	workerErr := buildah.WorkerFailure(fmt.Errorf("descendant drain: %w", buildah.ErrWorkerWaitIncomplete))
	relayed := errors.Join(context.Canceled, fmt.Errorf("execution failed: %w", workerErr))
	if !errors.Is(relayed, ErrCleanupFailed) || !errors.Is(relayed, context.Canceled) {
		t.Fatalf("public cleanup classification lost through worker relay: %v", relayed)
	}
	for _, err := range []error{nil, context.Canceled, errors.New(ErrCleanupFailed.Error()), buildah.WorkerFailure(errors.New("command failed"))} {
		if errors.Is(err, ErrCleanupFailed) {
			t.Fatalf("ordinary failure incorrectly requires input retention: %v", err)
		}
	}
}

func TestInvalidRegistryCachePrecedesFilesystemMutation(t *testing.T) {
	wasInitialized := initialized.Swap(true)
	defer initialized.Store(wasInitialized)
	for _, direction := range []string{"from", "to"} {
		t.Run(direction, func(t *testing.T) {
			root := t.TempDir()
			cache := CacheOptions{LocalDir: filepath.Join(root, "cache")}
			if direction == "from" {
				cache.RegistryFrom = []string{"registry.example/cache:tag"}
			} else {
				cache.RegistryTo = []string{"registry.example/cache:tag"}
			}
			options := Options{Store: StoreOptions{RunRoot: filepath.Join(root, "runroot"), GraphRoot: filepath.Join(root, "graphroot")}, Cache: cache, Output: Output{OCI: filepath.Join(root, "oci"), Filesystem: filepath.Join(root, "rootfs")}}
			_, err := Execute(context.Background(), Program{Output: "out", States: []State{{ID: "out", From: Source{Image: "scratch"}}}}, options)
			if err == nil || !strings.Contains(err.Error(), "registry cache") {
				t.Fatalf("invalid %s cache: %v", direction, err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("cache preflight mutated resources: %v %v", entries, err)
			}
		})
	}
}

func TestExecuteResolvesNativeSecurityDefaultsBeforeStartingWorkers(t *testing.T) {
	wasInitialized := initialized.Swap(true)
	defer initialized.Store(wasInitialized)
	root := t.TempDir()
	profile := filepath.Join(root, "missing-seccomp.json")
	config := filepath.Join(root, "containers.conf")
	if err := os.WriteFile(config, []byte("[containers]\nseccomp_profile = \""+profile+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_CONF", config)
	t.Setenv("CONTAINERS_CONF_OVERRIDE", "")
	_, err := Execute(context.Background(), Program{Output: "out", States: []State{{ID: "out", From: Source{Image: "scratch"}}}}, Options{
		ContextDir: root, Output: Output{OCI: filepath.Join(root, "output")},
	})
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), profile) {
		t.Fatalf("native security policy was not resolved before worker startup: %v", err)
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

func TestNamedCopyContextClassifiedAndPruned(t *testing.T) {
	assets := buildcontext.Spec{Name: "stoke-assets", Kind: buildcontext.Local, Path: t.TempDir()}
	unused := buildcontext.Spec{Name: "unused", Kind: buildcontext.Local, Path: t.TempDir()}
	literal := "scripts/$literal white space.sh"
	program := Program{Output: "out", States: []State{
		{ID: "unused-image", From: Source{Image: "stoke-assets"}, Operations: []Operation{Copy{Sources: []string{"unused"}, Destination: "/unused", From: Source{Context: "unused"}}}},
		{ID: "out", From: Source{Image: "scratch"}, Operations: []Operation{
			Copy{Sources: []string{literal}, Destination: "/check", From: Source{Context: "stoke-assets"}},
			Copy{Sources: []string{"project.txt"}, Destination: "/project"},
		}},
	}}
	plan, err := compile(program, unused, assets)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Stages) != 1 || len(plan.Contexts) != 1 || len(plan.Inputs) != 1 || plan.Inputs[0].Kind != "context" || plan.Inputs[0].Reference != "stoke-assets" {
		t.Fatalf("unused image/context retained or context unclassified: %#v", plan)
	}
	expected := planner.ContextInput{Spec: assets, Bindings: []planner.ContextBinding{{Role: "copy", StageID: "1", Operation: 0, MountIndex: -1}}}
	if !reflect.DeepEqual(plan.Contexts[0], expected) {
		t.Fatalf("context binding %#v", plan.Contexts[0])
	}
	ops := plan.Stages[0].Operations
	if ops[0].InputContext != "stoke-assets" || ops[0].Properties["from"] != "stoke-assets" || ops[0].Arguments[0] != literal || ops[1].InputContext != "build" {
		t.Fatalf("source roles/literals changed: %#v", ops)
	}
	_, err = buildah.BuildPlanSupervised(context.Background(), plan, buildah.SupervisedPlanOptions{})
	if err == nil || !strings.Contains(err.Error(), "run root is required") {
		t.Fatalf("native graph rejected named COPY before store validation: %v", err)
	}
}

func TestNamedContextsRejectUnsupportedOrAmbiguousSources(t *testing.T) {
	input := buildcontext.Spec{Name: "assets", Kind: buildcontext.Local, Path: t.TempDir()}
	for _, source := range []Source{{Context: "missing"}, {Context: "assets", Image: "scratch"}, {Context: "assets", State: "base"}} {
		p := Program{Output: "out", States: []State{{ID: "out", From: Source{Image: "scratch"}, Operations: []Operation{Copy{Sources: []string{"x"}, Destination: "/x", From: source}}}}}
		if _, err := compile(p, input); err == nil {
			t.Fatalf("accepted ambiguous/unknown COPY source %#v", source)
		}
	}
	for _, program := range []Program{
		{Output: "out", States: []State{{ID: "out", From: Source{Context: "assets"}}}},
	} {
		if _, err := compile(program, input); err == nil {
			t.Fatal("named context accepted as a state base")
		}
	}
}

func TestLocalNamedContextDirectoriesAndResourceOwnership(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "assets")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for _, values := range []map[string]string{{"build": directory}, {"scratch": directory}, {"ASSETS": directory}, {"bad name": directory}, {"assets": "relative"}, {"assets": filepath.Join(root, "missing")}} {
		if _, err := localContextSpecs(values); err == nil {
			t.Fatalf("accepted named contexts %#v", values)
		}
	}
	spec, err := localContextSpecs(map[string]string{"assets": directory, "overlap": directory})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateContextResources(spec, Options{ContextDir: directory, Output: Output{OCI: filepath.Join(root, "out")}}); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	for _, resource := range []Options{
		{Store: StoreOptions{RunRoot: filepath.Join(alias, "run")}},
		{Store: StoreOptions{GraphRoot: directory}},
		{Cache: CacheOptions{LocalDir: filepath.Join(alias, "cache")}},
		{Output: Output{OCI: filepath.Join(alias, "image")}},
		{Output: Output{Filesystem: filepath.Join(alias, "rootfs")}},
	} {
		if err := validateContextResources(spec, resource); err == nil {
			t.Fatalf("accepted context/resource overlap %#v", resource)
		}
	}
}
