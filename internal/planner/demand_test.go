package planner

import (
	"reflect"
	"strings"
	"testing"

	"coopr/internal/definition"
)

func TestCreateDemandDrivenResolvesOnlySelectedClosure(t *testing.T) {
	def := parse(t, `from "assets.example/image:latest" as="assets"
env ASSET_READY="$asset_arg"
from "unused.example/image:latest" as="unused"
env BROKEN="$undeclared"
from "final.example/image:latest" as="final"
env FINAL_READY="$final_arg"
`)

	var calls []FromSource
	plan, err := CreateDemandDriven(def, Options{Target: "final"}, func(source FromSource) (StageBind, error) {
		calls = append(calls, source)
		switch source.Source {
		case "final.example/image:latest":
			return StageBind{Inherited: []definition.Instruction{
				{Name: "arg", Arguments: []string{"final_arg", "final-value"}},
				{Name: "copy", Arguments: []string{"/payload", "/payload"}, Properties: map[string]string{"from": "assets"}},
			}}, nil
		case "assets.example/image:latest":
			return StageBind{Inherited: []definition.Instruction{
				{Name: "arg", Arguments: []string{"asset_arg", "asset-value"}},
			}}, nil
		default:
			t.Fatalf("resolved unselected source %+v", source)
			return StageBind{}, nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := calls, []FromSource{
		{StageID: "2", Source: "final.example/image:latest", Platform: "linux/amd64", Kind: FromSourceImage},
		{StageID: "0", Source: "assets.example/image:latest", Platform: "linux/amd64", Kind: FromSourceImage},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("resolver calls = %+v, want %+v", got, want)
	}
	if got, want := stageIDs(plan), []string{"0", "2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stage closure = %v, want %v", got, want)
	}
	if got := plan.Stages[0].Operations[0].Properties["ASSET_READY"]; got != "asset-value" {
		t.Fatalf("nested base argument = %q", got)
	}
	if got := plan.Stages[1].Operations[1].Properties["FINAL_READY"]; got != "final-value" {
		t.Fatalf("selected base argument = %q", got)
	}
}

func TestCreateDemandDrivenDoesNotResolveLocalOrScratchBases(t *testing.T) {
	def := parse(t, `from "scratch" as="base"
run "prepare"
from "base" as="final"
run "finish"
`)
	plan, err := CreateDemandDriven(def, Options{Target: "final"}, func(source FromSource) (StageBind, error) {
		t.Fatalf("resolver called for non-external FROM: %+v", source)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stageIDs(plan), []string{"0", "1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stage closure = %v, want %v", got, want)
	}
}

func TestCreateDemandDrivenResolvesExternalCopyImageInSelectedClosure(t *testing.T) {
	const source = "registry.example/tools:latest"
	var calls []FromSource
	plan, err := CreateDemandDriven(parse(t, `from "scratch" as="unused"
copy "/ignored" "/ignored" from="registry.example/unused:latest"
from "scratch" as="final"
copy "/bin/tool" "/usr/bin/tool" from="registry.example/tools:latest"
`), Options{Target: "final"}, func(input FromSource) (StageBind, error) {
		calls = append(calls, input)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := calls, []FromSource{{StageID: "1", Source: source, Platform: "linux/amd64", Kind: FromSourceCopyImage}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("resolver calls = %+v, want %+v", got, want)
	}
	if got, want := plan.Inputs, []Input{{Kind: "image", Reference: source}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("inputs = %+v, want %+v", got, want)
	}
	if len(plan.Stages) != 1 || len(plan.Stages[0].Dependencies) != 0 {
		t.Fatalf("external copy became a stage dependency: %+v", plan.Stages)
	}
}

func TestCreateDemandDrivenPlansInheritedExternalAndForwardCopySources(t *testing.T) {
	const base = "registry.example/parent:latest"
	const external = "registry.example/asset:latest"
	var calls []FromSource
	plan, err := CreateDemandDriven(parse(t, `
from "registry.example/parent:latest" as="final"
from "scratch" as="later"
copy "fixture" "/fixture"
`), Options{Target: "final"}, func(input FromSource) (StageBind, error) {
		calls = append(calls, input)
		if input.Source == base {
			return StageBind{Inherited: []definition.Instruction{
				{Name: "copy", Arguments: []string{"/asset", "/external"}, Properties: map[string]string{"from": external}},
				{Name: "copy", Arguments: []string{"/fixture", "/forward"}, Properties: map[string]string{"from": "later"}},
			}}, nil
		}
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := calls, []FromSource{
		{StageID: "0", Source: base, Platform: "linux/amd64", Kind: FromSourceImage},
		{StageID: "0", Source: external, Platform: "linux/amd64", Kind: FromSourceCopyImage},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected image sources = %+v, want %+v", got, want)
	}
	if got, want := stageIDs(plan), []string{"1", "0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected stages = %v, want %v", got, want)
	}
	if got, want := plan.Stages[1].Dependencies, []string{"1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("inherited forward dependency = %v, want %v", got, want)
	}
}

func TestCreateDemandDrivenResolvesExternalRunMountImageInSelectedClosure(t *testing.T) {
	const source = "registry.example/tools:latest"
	var calls []FromSource
	plan, err := CreateDemandDriven(parse(t, `from "scratch" as="unused"
run "true" { mount "bind" from="registry.example/unused:latest" source="/" target="/unused" }
from "scratch" as="final"
run "true" {
  mount "bind" from="registry.example/tools:latest" source="/bin" target="/tools" readonly="true"
  mount "cache" from="registry.example/tools:latest" source="/var/cache" target="/cache" id="tools"
}
`), Options{Target: "final"}, func(input FromSource) (StageBind, error) {
		calls = append(calls, input)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCall := FromSource{StageID: "1", Source: source, Platform: "linux/amd64", Kind: FromSourceCopyImage}
	if got, want := calls, []FromSource{wantCall, wantCall}; !reflect.DeepEqual(got, want) {
		t.Fatalf("resolver calls = %+v, want %+v", got, want)
	}
	if got, want := plan.Inputs, []Input{{Kind: "image", Reference: source}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("inputs = %+v, want %+v", got, want)
	}
	if len(plan.Stages) != 1 || len(plan.Stages[0].Dependencies) != 0 {
		t.Fatalf("external RUN mount became a stage dependency: %+v", plan.Stages)
	}
}

func TestCreateDemandDrivenChecksHiddenDependencyCycles(t *testing.T) {
	def := parse(t, `from "first.example/image:latest" as="first"
from "first" as="final"
`)
	_, err := CreateDemandDriven(def, Options{Target: "final"}, func(source FromSource) (StageBind, error) {
		return StageBind{Inherited: []definition.Instruction{{
			Name: "copy", Arguments: []string{"/payload", "/payload"}, Properties: map[string]string{"from": "final"},
		}}}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("hidden dependency cycle error = %v", err)
	}
}

func TestCreateDemandDrivenValidatesResolvedBind(t *testing.T) {
	_, err := CreateDemandDriven(parse(t, `from "base.example/image:latest"
`), Options{}, func(FromSource) (StageBind, error) {
		return StageBind{BaseEnvironment: map[string]string{"BAD=NAME": "value"}}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "invalid environment name") {
		t.Fatalf("invalid bind error = %v", err)
	}
}

func TestCreateDemandDrivenInheritsLocalOnBuildArgument(t *testing.T) {
	plan, err := CreateDemandDriven(parse(t, `from "scratch" as="base"
onbuild { arg "flavor" "local" }
from "base" as="final"
env RESULT="$flavor"
`), Options{Target: "final"}, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stageIDs(plan), []string{"0", "1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stage closure = %v, want %v", got, want)
	}
	child := plan.Stages[1]
	if !child.InheritedOnBuildPlanned {
		t.Fatal("local ONBUILD was not marked as planned")
	}
	if got := child.Operations[0].Properties["RESULT"]; got != "local" {
		t.Fatalf("local ONBUILD ARG expansion = %q", got)
	}
}

func TestCreateDemandDrivenLocalOnBuildAddsHiddenDependency(t *testing.T) {
	plan, err := CreateDemandDriven(parse(t, `from "scratch" as="assets"
run "prepare"
from "scratch" as="base"
onbuild { copy "/payload" "/payload" from="assets" }
from "base" as="final"
run "finish"
`), Options{Target: "final"}, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stageIDs(plan), []string{"1", "0", "2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stage closure = %v, want %v", got, want)
	}
	if got, want := plan.Stages[2].Dependencies, []string{"1", "0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("child dependencies = %v, want %v", got, want)
	}
	if got := plan.Stages[2].Operations[0].Properties["from"]; got != "assets" {
		t.Fatalf("inherited COPY source = %q", got)
	}
}

func TestCreateDemandDrivenLocalOnBuildChainConsumesEachParentOnce(t *testing.T) {
	plan, err := CreateDemandDriven(parse(t, `from "scratch" as="base"
onbuild { env LEVEL="base" }
from "base" as="middle"
onbuild { env LEVEL="middle" }
from "middle" as="final"
env SEEN="$LEVEL"
`), Options{Target: "final"}, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stageIDs(plan), []string{"0", "1", "2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stage closure = %v, want %v", got, want)
	}
	middle, final := plan.Stages[1], plan.Stages[2]
	if !middle.InheritedOnBuildPlanned || !final.InheritedOnBuildPlanned {
		t.Fatalf("local consumption markers: middle=%t final=%t", middle.InheritedOnBuildPlanned, final.InheritedOnBuildPlanned)
	}
	if len(middle.Operations) != 2 || middle.Operations[0].Name != "env" || middle.Operations[0].Arguments[1] != "base" || middle.Operations[1].Name != "onbuild" {
		t.Fatalf("middle operations = %+v", middle.Operations)
	}
	if len(final.Operations) != 2 || final.Operations[0].Name != "env" || final.Operations[0].Arguments[1] != "middle" {
		t.Fatalf("final inherited operations = %+v", final.Operations)
	}
	if got := final.Operations[1].Properties["SEEN"]; got != "middle" {
		t.Fatalf("final environment saw %q", got)
	}
}

func TestCreateDemandDrivenDefersAndReplansLocalBaseWithDynamicComponentConfig(t *testing.T) {
	plan, err := CreateDemandDriven(parse(t, `from "scratch" as="helper"
run "make helper"
from "scratch" as="base"
component "registry.example/component:latest"
from "base" as="final"
env RESULT="$COMPONENT_VALUE"
`), Options{Target: "final"}, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stageIDs(plan), []string{"1", "2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("initial stage closure = %v, want %v", got, want)
	}
	if got := plan.Stages[1].DynamicBaseStage; got != "1" {
		t.Fatalf("dynamic base stage = %q, want 1", got)
	}
	if len(plan.Stages[1].Operations) != 0 {
		t.Fatalf("deferred operations were expanded before binding: %+v", plan.Stages[1].Operations)
	}

	plan, err = plan.ReplanDynamicStage("2", StageBind{
		BaseEnvironment: map[string]string{"COMPONENT_VALUE": "ready"},
		Inherited: []definition.Instruction{{
			Name: "copy", Arguments: []string{"/helper", "/inherited"}, Properties: map[string]string{"from": "helper"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stageIDs(plan), []string{"1", "0", "2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("replanned stage closure = %v, want %v", got, want)
	}
	var final Stage
	for _, stage := range plan.Stages {
		if stage.ID == "2" {
			final = stage
			break
		}
	}
	if final.DynamicBaseStage != "" || !final.InheritedOnBuildPlanned {
		t.Fatalf("replanned final stage remains deferred: %+v", final)
	}
	if len(final.Operations) != 2 || final.Operations[0].Name != "copy" || final.Operations[0].Properties["from"] != "helper" || final.Operations[1].Name != "env" || final.Operations[1].Properties["RESULT"] != "ready" {
		t.Fatalf("replanned operations = %+v", final.Operations)
	}
}

func TestCreateDemandDrivenDefersDynamicConfigInTransitiveLocalAncestry(t *testing.T) {
	plan, err := CreateDemandDriven(parse(t, `from "scratch" as="base"
component "registry.example/component:latest"
from "base" as="middle"
run "middle"
from "middle" as="final"
run "finish"
`), Options{Target: "final"}, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stageIDs(plan), []string{"0", "1", "2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stage closure = %v, want %v", got, want)
	}
	if plan.Stages[1].DynamicBaseStage != "0" || plan.Stages[2].DynamicBaseStage != "1" {
		t.Fatalf("transitive deferred stages = %+v", plan.Stages)
	}
}

func TestCreateDemandDrivenPromotesSharedStageToPackagePhase(t *testing.T) {
	plan, err := CreateDemandDriven(parse(t, `from "scratch" as="helper"
package as="pkg"
copy "/payload" "/payload" from="helper"
extend as="base"
from "base" as="branch"
copy "/payload" "/payload" from="pkg"
from "registry.invalid/coopr/invocation-only:tag" as="remote"
from "base" as="output"
copy "/helper" "/helper" from="helper"
copy "/branch" "/branch" from="branch"
copy "/remote" "/remote" from="remote"
`), Options{Mode: Publish, Target: "output"}, func(source FromSource) (StageBind, error) {
		t.Fatalf("invocation-only image was resolved during publication: %+v", source)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stageIDs(plan), []string{"0", "1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("package production closure = %v, want %v", got, want)
	}
	if got, want := plan.Outputs, []string{"1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("package outputs = %v, want %v", got, want)
	}
}

func TestCreateDemandDrivenReplansLocalDescendantAfterPackagePromotion(t *testing.T) {
	var calls []FromSource
	plan, err := CreateDemandDriven(parse(t, `from "registry.example/helper:latest" as="helper"
from "helper" as="child"
arg "inherited"
env CHILD_VALUE="$inherited"
package as="pkg"
copy "/payload" "/payload" from="helper"
extend as="base"
from "base" as="branch"
copy "/payload" "/payload" from="pkg"
from "base" as="output"
copy "/child" "/child" from="child"
copy "/branch" "/branch" from="branch"
`), Options{Mode: Publish, Target: "output"}, func(source FromSource) (StageBind, error) {
		calls = append(calls, source)
		if source.Source != "registry.example/helper:latest" {
			t.Fatalf("unexpected base resolution: %+v", source)
		}
		return StageBind{Inherited: []definition.Instruction{{
			Name: "arg", Arguments: []string{"inherited", "resolved"},
		}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := calls, []FromSource{{
		StageID: "0", Source: "registry.example/helper:latest", Platform: "linux/amd64", Kind: FromSourceImage,
	}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("base resolution calls = %+v, want %+v", got, want)
	}
	if got, want := stageIDs(plan), []string{"0", "2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("package production closure = %v, want %v", got, want)
	}
}

func TestCreateDemandDrivenRetriesActiveLocalDescendantAfterPackagePromotion(t *testing.T) {
	def := parse(t, `from "registry.example/helper:latest" as="helper"
from "helper" as="child"
arg "flag"
env CHILD_VALUE="$flag"
copy "/payload" "/payload" from="pkg"
package as="pkg"
copy "/payload" "/payload" from="helper"
extend as="base"
from "base" as="output"
copy "/child" "/child" from="child"
`)
	var calls []FromSource
	resolver := func(source FromSource) (StageBind, error) {
		calls = append(calls, source)
		if source.Source != "registry.example/helper:latest" {
			t.Fatalf("unexpected base resolution: %+v", source)
		}
		return StageBind{Inherited: []definition.Instruction{{
			Name: "env", Arguments: []string{"flag", "from-onbuild"},
		}}}, nil
	}
	plan, err := CreateDemandDriven(def, Options{Mode: Publish, Target: "output"}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := calls, []FromSource{{
		StageID: "0", Source: "registry.example/helper:latest", Platform: "linux/amd64", Kind: FromSourceImage,
	}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("base resolution calls = %+v, want %+v", got, want)
	}
	if got, want := stageIDs(plan), []string{"0", "2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("package production closure = %v, want %v", got, want)
	}

	graph := demandGraphForTest(t, def, Options{Mode: Publish, Target: "output"}, resolver)
	if err := graph.resolve(4); err != nil {
		t.Fatal(err)
	}
	if got := graph.finalEnvironments[1]["CHILD_VALUE"]; got != "from-onbuild" {
		t.Fatalf("active local child retained stale base environment: CHILD_VALUE=%q", got)
	}
}

func TestCreateDemandDrivenSettlesResetChildBehindCompletedConsumer(t *testing.T) {
	def := parse(t, `from "registry.example/base:latest" as="base-image"
from "base-image" as="child"
arg "flag"
env CHILD_VALUE="$flag"
from "scratch" as="consumer"
copy "/child" "/child" from="child"
package as="pkg"
copy "/payload" "/payload" from="base-image"
extend as="base"
from "base" as="branch"
copy "/payload" "/payload" from="pkg"
from "base" as="output"
copy "/consumer" "/consumer" from="consumer"
copy "/branch" "/branch" from="branch"
`)
	resolver := func(source FromSource) (StageBind, error) {
		if source.Source != "registry.example/base:latest" {
			t.Fatalf("unexpected base resolution: %+v", source)
		}
		return StageBind{Inherited: []definition.Instruction{{
			Name: "env", Arguments: []string{"flag", "promoted"},
		}}}, nil
	}
	if _, err := CreateDemandDriven(def, Options{Mode: Publish, Target: "output"}, resolver); err != nil {
		t.Fatal(err)
	}

	graph := demandGraphForTest(t, def, Options{Mode: Publish, Target: "output"}, resolver)
	if err := graph.resolve(6); err != nil {
		t.Fatal(err)
	}
	if graph.states[1] != 2 {
		t.Fatalf("reset local child state = %d, want resolved", graph.states[1])
	}
	if graph.states[2] != 2 {
		t.Fatalf("completed consumer state = %d, want resolved", graph.states[2])
	}
	if got := graph.finalEnvironments[1]["CHILD_VALUE"]; got != "promoted" {
		t.Fatalf("replanned child environment = %q", got)
	}
	if got, want := graph.deps[2], []int{1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("consumer dependencies = %v, want %v", got, want)
	}
}

func demandGraphForTest(t *testing.T, def *definition.Definition, opts Options, resolver StageBindResolver) *graph {
	t.Helper()
	if opts.Platform == "" {
		opts.Platform = "linux/amd64"
	}
	g := &graph{
		opts: opts, globals: scope{}, automatic: automaticPlatformScope(opts),
		aliases: map[string]int{}, declared: map[string]bool{}, bindResolver: resolver,
	}
	if err := g.split(def); err != nil {
		t.Fatal(err)
	}
	n := len(g.raw)
	g.stages = make([]Stage, n)
	g.states = make([]int, n)
	g.finalScopes = make([]scope, n)
	g.finalEnvironments = make([]map[string]string, n)
	g.finalShadows = make([]map[string]bool, n)
	g.finalOnBuild = make([][]string, n)
	g.dynamicConfig = make([]bool, n)
	g.verifiedConfig = make([]bool, n)
	g.packageDerived = make([]bool, n)
	g.inheritedOperations = make([]int, n)
	g.packageResolved = make([]bool, n)
	g.resolutionEpoch = make([]uint64, n)
	g.scopes = make([]scope, n)
	g.deps = make([][]int, n)
	g.bases = make([]int, n)
	for i := range g.bases {
		g.bases[i] = -1
	}
	return g
}

func unexpectedBindResolver(t *testing.T) StageBindResolver {
	t.Helper()
	return func(source FromSource) (StageBind, error) {
		t.Fatalf("unexpected external base resolution: %+v", source)
		return StageBind{}, nil
	}
}
