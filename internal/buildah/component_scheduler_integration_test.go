package buildah

import (
	"context"
	"fmt"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/buildcontext"
	"coopr/internal/imageconfig"
	"coopr/internal/planner"
)

func TestComponentReturnSchedulesLateEarlierDependencies(t *testing.T) {
	for _, jobs := range []int{1, 2} {
		t.Run(fmt.Sprintf("jobs=%d", jobs), func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.options.Jobs = jobs
			f.write(t, "value", "dependency")
			f.write(t, "shared.coopr", `extend { env selected="source"; }`)
			source := `from "scratch" as="source" { copy "value" "/value"; }
from "scratch" as="left" {
 component "./shared.coopr"
 copy "/value" "/left" from="${selected}"
}
from "scratch" as="right" {
 component "./shared.coopr"
 copy "/value" "/right" from="${selected}"
}
from "scratch" {
 copy "/left" "/left" from="left"
 copy "/right" "/right" from="right"
}`
			for _, attempt := range []string{"cold", "warm"} {
				_, layout := buildComponentContinuationFixture(t, f, attempt, source)
				manifest, _ := readPlanImage(t, layout)
				for index, name := range []string{"left", "right"} {
					got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[index].Digest.Encoded()), name)
					if got != "dependency" {
						t.Fatalf("%s %s=%q", attempt, name, got)
					}
				}
			}
		})
	}
}

func TestComponentReturnDependencyFailureReapsPausedStage(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.options.Jobs = 1
	f.write(t, "shared.coopr", `extend { env selected="source"; }`)
	def := parseWorkerDefinition(t, `from "scratch" as="source" { copy "missing-file" "/value"; }
from "scratch" { component "./shared.coopr"; copy "/value" "/value" from="${selected}"; }`)
	plan, err := planner.Create(def, planner.Options{Mode: planner.Build, AwaitComponentReturns: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	options := f.options
	options.Output = Output{Path: filepath.Join(f.root, "failed"), DisableCompression: true}
	_, err = BuildPlan(ctx, plan, options)
	if err == nil || !strings.Contains(err.Error(), "missing-file") {
		t.Fatalf("dependency failure=%v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("paused stage was not reaped promptly: %v", ctx.Err())
	}
}

func TestSupervisedComponentPlanPreservesAuthoredContinuation(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "shared.coopr", `extend { env captured="${caller}"; }`)
	def := parseWorkerDefinition(t, `from "scratch" { env caller="worker"; component "./shared.coopr"; env proof="${captured}"; }`)
	plan, err := planner.Create(def, planner.Options{Mode: planner.Build})
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(f.root, "supervised")
	options := SupervisedPlanOptions{Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun", ComponentStoreDir: filepath.Join(f.root, "components"), Output: Output{Path: layout, DisableCompression: true}, SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath}
	if _, err := BuildPlanSupervised(f.ctx, plan, options); err != nil {
		t.Fatal(err)
	}
	_, image := readPlanImage(t, layout)
	if !strings.Contains("\n"+strings.Join(image.Config.Env, "\n")+"\n", "\nproof=worker\n") {
		t.Fatalf("serialized plan lost authored expressions: %v", image.Config.Env)
	}
}

func buildComponentContinuationFixture(t *testing.T, f localComponentFixture, name, source string) (Result, string) {
	t.Helper()
	def := parseWorkerDefinition(t, source)
	plan, err := planner.Create(def, planner.Options{Mode: planner.Build, AwaitComponentReturns: true})
	if err != nil {
		t.Fatal(err)
	}
	options := f.options
	options.Output = Output{Path: filepath.Join(f.root, name), DisableCompression: true}
	result, err := BuildPlan(f.ctx, plan, options)
	if err != nil {
		t.Fatal(err)
	}
	return result, options.Output.Path
}

func TestSupervisedPublicationUsesReturnedPackageArguments(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "shared.coopr", `extend { env captured="${caller}"; }`)
	f.write(t, "value", "payload")
	def := parseWorkerDefinition(t, `package as="payload" {
 env caller="package"
 component "./shared.coopr"
 arg "fixed" "${captured}"
 env proof="${fixed}"
 copy "value" "/value"
}
extend {
 arg "fixed"
 copy "/value" "/value" from="payload"
 env delivered="${fixed}"
}`)
	plan, err := planner.Create(def, planner.Options{Mode: planner.Publish, AwaitComponentReturns: true})
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(f.root, "publication")
	result, err := PublishPlanSupervised(f.ctx, plan, SupervisedPlanOptions{Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun", ComponentStoreDir: filepath.Join(f.root, "components"), Output: Output{Path: layout, DisableCompression: true}, SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath})
	if err != nil {
		t.Fatal(err)
	}
	metadata := readComponentMetadata(t, f.ctx, layout, result.Root)
	if metadata.Component.PackageArguments["fixed"] != "package" {
		t.Fatalf("stale published package arguments: %v", metadata.Component.PackageArguments)
	}
	if len(metadata.Packages) != 1 {
		t.Fatalf("packages=%v", metadata.Packages)
	}
	config, err := imageconfig.Parse(metadata.Packages[0].Config)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := imageEnvironment(config)
	if err != nil {
		t.Fatal(err)
	}
	if environment["proof"] != "package" {
		t.Fatalf("package config=%v", environment)
	}
}

func TestComponentReturnPreservesSyntheticStageLabels(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.options.Lifecycle.StageLabels = true
	f.options.Lifecycle.SaveStages = true
	f.write(t, "shared.coopr", `extend { env captured="${caller}"; }`)
	_, layout := buildComponentContinuationFixture(t, f, "stage-labels", `from "scratch" as="outer" { env caller="labelled"; component "./shared.coopr"; env proof="${captured}"; }`)
	_, image := readPlanImage(t, layout)
	if !strings.Contains("\n"+strings.Join(image.Config.Env, "\n")+"\n", "\nproof=labelled\n") {
		t.Fatalf("stage-label continuation ENV=%v", image.Config.Env)
	}
	if image.Config.Labels["io.buildah.stage.name"] == "" {
		t.Fatal("synthetic stage label missing from selected output")
	}
}

func TestIsolatedNestedReturnSharesAncestorImageSelection(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(fmt.Sprintf("named-context=%t", named), func(t *testing.T) { testIsolatedNestedImageSelection(t, named) })
	}
}

func testIsolatedNestedImageSelection(t *testing.T, named bool) {
	f := newLocalComponentFixture(t)
	f.options.Jobs = 2
	f.write(t, "value", "first")
	_, firstLayout := f.build(t, "first-base", `from "scratch" { copy "value" "/value"; }`)
	f.write(t, "value", "second")
	_, secondLayout := f.build(t, "second-base", `from "scratch" { copy "value" "/value"; }`)
	selection := "invalid.example/coopr-return-base:mutable"
	if named {
		selection = "toolchain"
		f.options.BuildContexts = []buildcontext.Spec{{Name: selection, Kind: buildcontext.DockerImage, Reference: "invalid.example/coopr-return-base:mutable"}}
	}
	f.write(t, "inner.coopr", fmt.Sprintf(`extend { env selected=%q; }`, selection))
	f.write(t, "outer.coopr", fmt.Sprintf(`extend { arg "selected" %q; component "./inner.coopr"; copy "/value" "/proof" from="${selected}"; }`, selection))
	def := parseWorkerDefinition(t, `from "scratch" as="left" { component "./outer.coopr"; }
from "scratch" as="right" { component "./outer.coopr"; }
from "scratch" { copy "/proof" "/left" from="left"; copy "/proof" "/right" from="right"; }`)
	plan, err := planner.Create(def, planner.Options{Mode: planner.Build, AwaitComponentReturns: true, BuildContexts: f.options.BuildContexts})
	if err != nil {
		t.Fatal(err)
	}
	stages, output, err := validateStandaloneGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := readyGraphStages(stages, nil, nil, 2)
	if err != nil || len(ready) != 2 {
		t.Fatalf("requires two initially-ready isolated branches: %v %v", ready, err)
	}
	options := f.options
	options.Output = Output{Path: filepath.Join(f.root, "isolated-selected"), DisableCompression: true}
	executor, err := newGraphExecutor(f.ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := executor.Close(); err != nil {
			t.Error(err)
		}
	}()
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	first, err := resolveTransportSource(f.ctx, options.Resolver, "oci:"+firstLayout, platform, executor.store, executor.system)
	if err != nil {
		t.Fatal(err)
	}
	second, err := resolveTransportSource(f.ctx, options.Resolver, "oci:"+secondLayout, platform, executor.store, executor.system)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	executor.baseSelectionHandler = func(_ context.Context, request baseSelectionRequest) (ResolvedImageSource, error) {
		if request.Reference != "invalid.example/coopr-return-base:mutable" {
			return ResolvedImageSource{}, fmt.Errorf("unexpected selection %q", request.Reference)
		}
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	}
	if _, err := executor.executePlanGraph(f.ctx, plan, stages, output, map[string]bool{output: true}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("ancestor selected mutable input %d times, want once", calls)
	}
	manifest, _ := readPlanImage(t, options.Output.Path)
	for index, name := range []string{"left", "right"} {
		got := readLayerFile(t, filepath.Join(options.Output.Path, "blobs", "sha256", manifest.Layers[index].Digest.Encoded()), name)
		if got != "first" {
			t.Fatalf("%s used different immutable input: %q", name, got)
		}
	}
}

func TestComponentReturnActivatesBoundPackageInput(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.options.Jobs = 1
	f.write(t, "value", "packaged")
	f.write(t, "selector.coopr", `extend { env selected="payload"; }`)
	f.write(t, "shared.coopr", `package as="payload" { copy "value" "/value"; }
extend { arg "selected" "payload"; component "./selector.coopr"; copy "/value" "/proof" from="${selected}"; }`)
	ctx, cancel := context.WithTimeout(f.ctx, 20*time.Second)
	defer cancel()
	f.ctx = ctx
	for _, attempt := range []string{"cold", "warm"} {
		_, layout := buildComponentContinuationFixture(t, f, attempt, `from "scratch" { component "./shared.coopr"; }`)
		if got := localComponentLastFile(t, layout, "proof"); got != "packaged" {
			t.Fatalf("%s proof=%q", attempt, got)
		}
	}
}

func TestNestedIsolatedComponentCyclePreservesInvocationAncestry(t *testing.T) {
	for _, singleCaller := range []bool{false, true} {
		for _, published := range []bool{false, true} {
			t.Run(fmt.Sprintf("single-caller=%t/published=%t", singleCaller, published), func(t *testing.T) {
				f := newLocalComponentFixture(t)
				f.options.Jobs = 2
				reference := "./cycle.coopr"
				source := `arg "depth" "x"
from "scratch" as="recursive" {
 arg "depth"
 component "./cycle.coopr" { arg "depth" "${depth}x"; }
}
from "scratch" as="independent" { env independent="yes"; }
extend { copy "/unused" "/unused" from="recursive"; copy "/unused" "/independent" from="independent"; }`
				if published {
					reference = "cycle"
					source = `from "scratch" as="recursive" { component "cycle"; }
from "scratch" as="independent" { env independent="yes"; }
extend { copy "/unused" "/unused" from="recursive"; copy "/unused" "/independent" from="independent"; }`
					if singleCaller {
						source = strings.Replace(source, `component "cycle";`, `component "boundary"; component "cycle";`, 1)
					}
					f.options.Resolver = localConfigComponentResolverWithDefinitions(t, f.ctx, f.root, []componentDefinitionFixture{{name: "boundary", source: `extend { env boundary="returned"; }`}, {name: "cycle", source: source}}, false)
				} else {
					if singleCaller {
						source = strings.Replace(source, ` component "./cycle.coopr"`, " component \"./boundary.coopr\"\n component \"./cycle.coopr\"", 1)
						f.write(t, "boundary.coopr", `extend { env boundary="returned"; }`)
					}
					f.write(t, "cycle.coopr", source)
				}
				caller := fmt.Sprintf(`from "scratch" as="recursive" { component %q; }
from "scratch" as="independent" { env independent="yes"; }
from "scratch" { copy "/unused" "/unused" from="recursive"; copy "/unused" "/independent" from="independent"; }`, reference)

				if singleCaller {
					caller = fmt.Sprintf(`from "scratch" { component %q; }`, reference)
				}
				callerPlan := testPlan(t, caller)
				ready, err := readyGraphStages(callerPlan.Stages, nil, nil, 2)
				wantReady := 2
				if singleCaller {
					wantReady = 1
				}
				if err != nil || len(ready) != wantReady {
					t.Fatalf("caller topology ready=%v want=%d error=%v", ready, wantReady, err)
				}
				// With a single ready caller, the root executes in process. Its
				// component has two ready branches and retains Jobs=2, so those
				// branches launch actual workers with nonempty invocation ancestry.
				executor, err := newGraphExecutor(f.ctx, f.options)
				if err != nil {
					t.Fatal(err)
				}
				resolved, resolveErr := executor.resolveComponentInvocation(f.ctx, reference, nil, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "", nil)
				closeErr := executor.Close()
				if resolveErr != nil || closeErr != nil {
					t.Fatalf("inspect component topology: %v %v", resolveErr, closeErr)
				}
				ready, err = readyGraphStages(resolved.Plan.Stages, nil, nil, 2)
				if err != nil || len(ready) != 2 {
					t.Fatalf("component must have two initially-ready branches: %v %v", ready, err)
				}
				if singleCaller {
					recursive := resolved.Plan.Stages[ready[0]]
					boundary := "./boundary.coopr"
					if published {
						boundary = "boundary"
					}
					if recursive.Name != "recursive" || recursive.AwaitingComponent == nil || len(recursive.Operations) == 0 {
						t.Fatalf("recursive branch must await helper before cycle: %#v", recursive)
					}
					operation := recursive.Operations[len(recursive.Operations)-1]
					if operation.Name != "component" || len(operation.Arguments) != 1 || operation.Arguments[0] != boundary {
						t.Fatalf("cycle was exposed before descendant worker launch: %#v", operation)
					}
				}

				options := f.options
				options.Output = Output{Path: filepath.Join(f.root, "cycle-output")}
				ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
				defer cancel()
				_, err = BuildPlan(ctx, callerPlan, options)
				if err == nil || !strings.Contains(err.Error(), "invocation cycle at") || ctx.Err() != nil {
					t.Fatalf("nested isolated cycle=%v context=%v", err, ctx.Err())
				}
			})
		}
	}
}

func TestIsolatedComponentsPermitSiblingsAndFiniteTargetCalls(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.options.Jobs = 2
	f.write(t, "shared.coopr", `extend as="leaf" { env leaf="yes"; }
extend as="wrapper" { component "./shared.coopr#leaf"; env wrapper="yes"; }`)
	_, layout := buildComponentContinuationFixture(t, f, "finite-siblings", `from "scratch" as="left" { component "./shared.coopr#wrapper"; component "./shared.coopr#wrapper"; }
from "scratch" as="right" { component "./shared.coopr#wrapper"; }
from "left" after="right" {}`)
	_, image := readPlanImage(t, layout)
	for _, expected := range []string{"leaf=yes", "wrapper=yes"} {
		if !strings.Contains("\n"+strings.Join(image.Config.Env, "\n")+"\n", "\n"+expected+"\n") {
			t.Fatalf("finite sibling output missing %s: %v", expected, image.Config.Env)
		}
	}
}
