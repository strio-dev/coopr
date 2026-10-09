package planner

import (
	"reflect"
	"testing"

	"coopr/internal/buildcontext"
	"coopr/internal/definition"
)

func TestNamedContextReplacesMatchingStageAndSkipsBody(t *testing.T) {
	context := buildcontext.Spec{Name: "base", Kind: buildcontext.DockerImage, Reference: "registry.example/replacement:latest"}
	var calls []FromSource
	plan, err := CreateDemandDriven(parse(t, `from "registry.example/original:latest" as="base"
run "exit 99"
`), Options{Mode: Build, Target: "base", BuildContexts: []buildcontext.Spec{context}}, func(source FromSource) (StageBind, error) {
		calls = append(calls, source)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantSource := FromSource{StageID: "0", Source: "base", Platform: "linux/amd64", Kind: FromSourceContext, Context: &context}
	if !reflect.DeepEqual(calls, []FromSource{wantSource}) {
		t.Fatalf("resolver calls = %+v, want %+v", calls, []FromSource{wantSource})
	}
	if len(plan.Stages) != 1 || len(plan.Stages[0].Operations) != 0 {
		t.Fatalf("replacement stage retained authored body: %+v", plan.Stages)
	}
	if plan.Stages[0].SourceContext != "base" || plan.Stages[0].Source != "base" {
		t.Fatalf("replacement source = %+v", plan.Stages[0])
	}
	if got, want := plan.Contexts, []ContextInput{{Spec: context, Bindings: []ContextBinding{{Role: "from", StageID: "0", Operation: -1, MountIndex: -1}}}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("contexts = %+v, want %+v", got, want)
	}
	if got, want := plan.Inputs, []Input{{Kind: "context", Reference: "base"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("inputs = %+v, want %+v", got, want)
	}
}

func TestNamedContextsBindFromCopyAndRunMountBeforeImageFallback(t *testing.T) {
	base := buildcontext.Spec{Name: "base", Kind: buildcontext.DockerImage, Reference: "registry.example/base:latest"}
	assets := buildcontext.Spec{Name: "assets", Kind: buildcontext.Local, Path: "/context/assets"}
	unused := buildcontext.Spec{Name: "unused", Kind: buildcontext.DockerImage, Reference: "registry.example/unused:latest"}
	var calls []FromSource
	plan, err := CreateDemandDriven(parse(t, `from "base" as="final"
copy "/payload" "/payload" from="assets"
run "true" { mount "bind" from="assets" source="/" target="/assets" readonly="true" }
`), Options{Mode: Build, Target: "final", BuildContexts: []buildcontext.Spec{unused, assets, base}}, func(source FromSource) (StageBind, error) {
		calls = append(calls, source)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCalls := []FromSource{
		{StageID: "0", Source: "base", Platform: "linux/amd64", Kind: FromSourceContext, Context: &base},
		{StageID: "0", Source: "assets", Platform: "linux/amd64", Kind: FromSourceContext, Context: &assets},
		{StageID: "0", Source: "assets", Platform: "linux/amd64", Kind: FromSourceContext, Context: &assets},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("resolver calls = %+v, want %+v", calls, wantCalls)
	}
	if got := plan.Stages[0].Operations[0].InputContext; got != "assets" {
		t.Fatalf("COPY input context = %q", got)
	}
	if got := plan.Stages[0].Operations[1].MountContexts[0]; got != "assets" {
		t.Fatalf("RUN mount context = %q", got)
	}
	wantContexts := []ContextInput{
		{Spec: assets, Bindings: []ContextBinding{
			{Role: "copy", StageID: "0", Operation: 0, MountIndex: -1},
			{Role: "run-mount", StageID: "0", Operation: 1, MountIndex: 0},
		}},
		{Spec: base, Bindings: []ContextBinding{{Role: "from", StageID: "0", Operation: -1, MountIndex: -1}}},
	}
	if !reflect.DeepEqual(plan.Contexts, wantContexts) {
		t.Fatalf("contexts = %+v, want %+v", plan.Contexts, wantContexts)
	}
}

func TestNumericStageReferencesRemainStageReferencesWithNamedContexts(t *testing.T) {
	plan, err := CreateDemandDriven(parse(t, `from "scratch" as="producer"
copy "payload" "/payload"
from "scratch" as="final"
copy "/payload" "/payload" from="0"
run "true" { mount "bind" from="0" source="/" target="/producer" readonly="true" }
`), Options{Mode: Build, Target: "final", BuildContexts: []buildcontext.Spec{{Name: "assets", Kind: buildcontext.Local, Path: "/context/assets"}}}, func(source FromSource) (StageBind, error) {
		t.Fatalf("numeric stage reference resolved externally: %+v", source)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stageIDs(plan), []string{"0", "1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stages = %v, want %v", got, want)
	}
	if got, want := plan.Stages[1].Dependencies, []string{"0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dependencies = %v, want %v", got, want)
	}
	if len(plan.Contexts) != 0 {
		t.Fatalf("unused named context retained: %+v", plan.Contexts)
	}
}

func TestInheritedOnBuildCanBindNamedContext(t *testing.T) {
	assets := buildcontext.Spec{Name: "assets", Kind: buildcontext.Local, Path: "/context/assets"}
	var calls []FromSource
	plan, err := CreateDemandDriven(parse(t, `from "registry.example/base:latest" as="final"
`), Options{Mode: Build, BuildContexts: []buildcontext.Spec{assets}}, func(source FromSource) (StageBind, error) {
		calls = append(calls, source)
		if source.Kind == FromSourceImage {
			return StageBind{Inherited: []definition.Instruction{{Name: "copy", Arguments: []string{"/payload", "/payload"}, Properties: map[string]string{"from": "assets"}}}}, nil
		}
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[1].Kind != FromSourceContext || calls[1].Source != "assets" {
		t.Fatalf("resolver calls = %+v", calls)
	}
	if got := plan.Stages[0].Operations[0].InputContext; got != "assets" {
		t.Fatalf("inherited COPY input context = %q", got)
	}
	if got, want := plan.Contexts, []ContextInput{{Spec: assets, Bindings: []ContextBinding{{Role: "copy", StageID: "0", Operation: 0, MountIndex: -1}}}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("contexts = %+v, want %+v", got, want)
	}
}

func TestPublicationUsesNamedContextsOnlyForPackageClosure(t *testing.T) {
	builder := buildcontext.Spec{Name: "builder", Kind: buildcontext.DockerImage, Reference: "registry.example/replacement:latest"}
	var calls []FromSource
	plan, err := CreateDemandDriven(parse(t, `from "registry.example/original:latest" as="builder"
run "exit 99"
package as="bundle"
copy "/payload" "/payload" from="builder"
extend as="base"
from "base" as="final"
copy "/payload" "/payload" from="bundle"
`), Options{Mode: Publish, Target: "final", BuildContexts: []buildcontext.Spec{builder}}, func(source FromSource) (StageBind, error) {
		calls = append(calls, source)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := calls, []FromSource{{StageID: "1", Source: "builder", Platform: "linux/amd64", Kind: FromSourceContext, Context: &builder}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("resolver calls = %+v, want %+v", got, want)
	}
	if len(plan.Stages) != 1 || plan.Stages[0].Kind != "package" {
		t.Fatalf("package plan = %+v", plan.Stages)
	}
	if _, err := ValidatePublished(plan.Component); err != nil {
		t.Fatalf("published component metadata invalid: %v", err)
	}
	if got, want := plan.Contexts, []ContextInput{{Spec: builder, Bindings: []ContextBinding{{Role: "copy", StageID: "1", Operation: 0, MountIndex: -1}}}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("contexts = %+v, want %+v", got, want)
	}
}

func TestPublishedFromHonorsExplicitNamedContextOverrideOfCallerAlias(t *testing.T) {
	source := `extend as="caller"
from "caller" as="selected"
env "named" "replacement"
`
	publication := makePlan(t, source, Options{Mode: Publish})
	context := buildcontext.Spec{Name: "caller", Kind: buildcontext.DockerImage, Reference: "registry.example/replacement:latest"}
	calls := 0
	invocation, err := InstantiateDemandDriven(publication.Component, Options{BuildContexts: []buildcontext.Spec{context}}, nil, func(source FromSource) (StageBind, error) {
		calls++
		if source.Kind != FromSourceContext || source.Source != "caller" || source.Context == nil || !reflect.DeepEqual(*source.Context, context) {
			t.Fatalf("explicit caller context not selected: %+v", source)
		}
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(invocation.Stages) != 1 || invocation.Stages[0].SourceContext != "caller" || len(invocation.CompatibilityRoots) != 1 {
		t.Fatalf("context override changed invocation contract: %+v", invocation)
	}
	if invocation.Stages[0].Operations[0].Name != "env" {
		t.Fatal("source override lost selected stage body")
	}
	if _, err := ValidatePublished(publication.Component); err != nil {
		t.Fatalf("context invocation mutated immutable publication: %v", err)
	}
}
