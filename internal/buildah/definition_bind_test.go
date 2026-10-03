package buildah

import (
	"context"
	"strings"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/planner"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestPlanDefinitionBindsExternalOnBuildBeforeClosure(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`
from "scratch" as="producer"
from "registry.invalid/coopr/parent:tag"
copy "$filename" "/selected"
`))
	if err != nil {
		t.Fatal(err)
	}
	opts := planner.Options{Mode: planner.Build, Platform: "linux/amd64"}
	unbound, err := planner.Create(def, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(unbound.Stages) != 1 || unbound.Stages[0].ID != "1" || len(unbound.Stages[0].Operations) == 0 {
		t.Fatalf("unbound stage graph = %+v", unbound.Stages)
	}
	if got := unbound.Stages[0].Operations[len(unbound.Stages[0].Operations)-1].Arguments[0]; got != "" {
		t.Fatalf("unbound COPY source = %q, want empty expansion", got)
	}
	const ref = "registry.invalid/coopr/parent:tag"
	const config = `{"architecture":"amd64","os":"linux","config":{"Env":["HOME=/opt"],"OnBuild":["ARG filename=payload","COPY --from=producer /artifact /inherited"]}}`
	called := 0
	plan, selected, err := planDefinitionWithExternalBases(context.Background(), def, opts, func(_ context.Context, reference string, platform v1.Platform) (ResolvedImageSource, error) {
		called++
		if reference != ref || platform.OS != "linux" || platform.Architecture != "amd64" {
			t.Fatalf("resolved %q for %+v", reference, platform)
		}
		return ResolvedImageSource{ImageID: "immutable-image", ConfigData: []byte(config)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 || len(selected) != 1 || selected[ResolvedBaseKey{Reference: ref, Platform: "linux/amd64"}].ImageID != "immutable-image" {
		t.Fatalf("selected images: calls=%d, entries=%+v", called, selected)
	}
	if len(plan.Stages) != 2 || plan.Stages[1].ID != "1" {
		t.Fatalf("bound stage graph = %+v", plan.Stages)
	}
	child := plan.Stages[1]
	if !child.InheritedOnBuildPlanned {
		t.Fatal("child stage did not mark inherited triggers as planned")
	}
	if len(child.Dependencies) != 1 || child.Dependencies[0] != "0" {
		t.Fatalf("inherited --from dependency = %v", child.Dependencies)
	}
	copies := []planner.Operation{}
	for _, operation := range child.Operations {
		if operation.Name == "copy" {
			copies = append(copies, operation)
		}
	}
	if len(copies) != 2 || copies[0].Properties["from"] != "producer" || copies[1].Arguments[0] != "payload" {
		t.Fatalf("inherited and authored COPY operations = %+v", copies)
	}
}

func TestPlanDefinitionDoesNotResolveUnselectedTargetBase(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`
from "scratch" as="selected"
from "registry.invalid/coopr/unselected:tag"
`))
	if err != nil {
		t.Fatal(err)
	}
	plan, selected, err := planDefinitionWithExternalBases(context.Background(), def, planner.Options{
		Mode: planner.Build, Target: "selected", Platform: "linux/amd64",
	}, func(_ context.Context, reference string, platform v1.Platform) (ResolvedImageSource, error) {
		t.Fatalf("unselected base %q for %+v was resolved", reference, platform)
		return ResolvedImageSource{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Stages) != 1 || plan.Stages[0].ID != "0" || len(selected) != 0 {
		t.Fatalf("selected target plan = %+v; base selections = %+v", plan.Stages, selected)
	}
}

func TestPlanDefinitionBindsExternalBaseRevealedByInheritedDependency(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`
from "registry.invalid/coopr/producer:tag" as="producer"
from "registry.invalid/coopr/child:tag"
`))
	if err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	plan, selected, err := planDefinitionWithExternalBases(context.Background(), def, planner.Options{
		Mode: planner.Build, Platform: "linux/amd64",
	}, func(_ context.Context, reference string, _ v1.Platform) (ResolvedImageSource, error) {
		calls = append(calls, reference)
		config := `{"architecture":"amd64","os":"linux","config":{"Env":["BASE=producer"]}}`
		if reference == "registry.invalid/coopr/child:tag" {
			config = `{"architecture":"amd64","os":"linux","config":{"OnBuild":["COPY --from=producer /source /inherited"]}}`
		}
		return ResolvedImageSource{ImageID: reference, ConfigData: []byte(config)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != "registry.invalid/coopr/child:tag" || calls[1] != "registry.invalid/coopr/producer:tag" {
		t.Fatalf("base resolution order = %v", calls)
	}
	if len(selected) != 2 || len(plan.Stages) != 2 || len(plan.Stages[1].Dependencies) != 1 || plan.Stages[1].Dependencies[0] != "0" {
		t.Fatalf("hidden dependency plan = %+v; selections = %+v", plan.Stages, selected)
	}
}

func TestPlanDefinitionBindsHiddenBaseBeforeValidatingItsBody(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`
from "registry.invalid/coopr/producer:tag" as="producer"
copy "$payload" "/artifact"
from "registry.invalid/coopr/child:tag"
`))
	if err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	plan, _, err := planDefinitionWithExternalBases(context.Background(), def, planner.Options{
		Mode: planner.Build, Platform: "linux/amd64",
	}, func(_ context.Context, reference string, _ v1.Platform) (ResolvedImageSource, error) {
		calls = append(calls, reference)
		config := `{"architecture":"amd64","os":"linux","config":{"OnBuild":["ARG payload=source"]}}`
		if reference == "registry.invalid/coopr/child:tag" {
			config = `{"architecture":"amd64","os":"linux","config":{"OnBuild":["COPY --from=producer /artifact /inherited"]}}`
		}
		return ResolvedImageSource{ImageID: reference, ConfigData: []byte(config)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != "registry.invalid/coopr/child:tag" || calls[1] != "registry.invalid/coopr/producer:tag" {
		t.Fatalf("base resolution order = %v", calls)
	}
	if len(plan.Stages) != 2 || plan.Stages[0].Operations[0].Arguments[0] != "source" {
		t.Fatalf("hidden base was not fully bound: %+v", plan.Stages)
	}
}

func TestPlanDefinitionSkipsUnselectedBaseRequiringInheritedArgument(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`
from "registry.invalid/coopr/unselected:tag" as="unused"
copy "$only_in_base" "/unused"
from "scratch" as="selected"
`))
	if err != nil {
		t.Fatal(err)
	}
	plan, selected, err := planDefinitionWithExternalBases(context.Background(), def, planner.Options{
		Mode: planner.Build, Target: "selected", Platform: "linux/amd64",
	}, func(_ context.Context, reference string, _ v1.Platform) (ResolvedImageSource, error) {
		t.Fatalf("unselected base %q was resolved", reference)
		return ResolvedImageSource{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 0 || len(plan.Stages) != 1 || plan.Stages[0].ID != "1" {
		t.Fatalf("selected plan = %+v; base selections = %+v", plan.Stages, selected)
	}
}

func TestPlanDefinitionRejectsUnsupportedPlatformBeforeResolvingBase(t *testing.T) {
	def, err := definition.Parse(strings.NewReader("from \"registry.invalid/coopr/base:tag\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = planDefinitionWithExternalBases(context.Background(), def, planner.Options{
		Mode: planner.Build, Platform: "windows/amd64",
	}, func(_ context.Context, reference string, _ v1.Platform) (ResolvedImageSource, error) {
		t.Fatalf("unsupported platform base %q was resolved", reference)
		return ResolvedImageSource{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "requires Linux") {
		t.Fatalf("expected Linux-platform validation, got %v", err)
	}
}

func TestPlanPublicationDoesNotResolveInvocationOnlyImage(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`
from "registry.invalid/coopr/invocation-only:tag" as="helper"
package as="payload"
copy "fixture" "/fixture"
extend
copy "/fixture" "/fixture" from="payload"
copy "/other" "/other" from="helper"
`))
	if err != nil {
		t.Fatal(err)
	}
	plan, selected, err := planDefinitionWithExternalBases(context.Background(), def, planner.Options{
		Mode: planner.Publish, Platform: "linux/amd64",
	}, func(_ context.Context, reference string, _ v1.Platform) (ResolvedImageSource, error) {
		t.Fatalf("invocation-only image %q was resolved during publication", reference)
		return ResolvedImageSource{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 0 || len(plan.Stages) != 1 || plan.Stages[0].Kind != "package" {
		t.Fatalf("publication plan = %+v; base selections = %+v", plan.Stages, selected)
	}
}

func TestPlanDefinitionSelectsExternalCopyImage(t *testing.T) {
	const reference = "registry.invalid/coopr/tool:tag"
	def, err := definition.Parse(strings.NewReader("from \"scratch\"\ncopy \"/bin/tool\" \"/tool\" from=\"" + reference + "\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	selectedImage := ResolvedImageSource{ImageID: "immutable-tool-image", ConfigData: []byte(`{"architecture":"amd64","os":"linux"}`)}
	plan, selected, err := planDefinitionWithExternalBases(context.Background(), def, planner.Options{
		Mode: planner.Build, Platform: "linux/amd64",
	}, func(_ context.Context, got string, platform v1.Platform) (ResolvedImageSource, error) {
		if got != reference || platform.OS != "linux" || platform.Architecture != "amd64" {
			t.Fatalf("resolve %q for %+v", got, platform)
		}
		return selectedImage, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	key := ResolvedBaseKey{Reference: reference, Platform: "linux/amd64"}
	if selected[key].ImageID != selectedImage.ImageID {
		t.Fatalf("selected sources = %+v", selected)
	}
	if len(plan.Inputs) != 1 || plan.Inputs[0] != (planner.Input{Kind: "image", Reference: reference}) {
		t.Fatalf("plan inputs = %+v", plan.Inputs)
	}
}

func TestPlanDefinitionDoesNotInterpretExternalCopyImageConfig(t *testing.T) {
	const reference = "registry.invalid/coopr/payload:tag"
	def, err := definition.Parse(strings.NewReader("from \"scratch\"\ncopy \"/payload\" \"/payload\" from=\"" + reference + "\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	plan, selected, err := planDefinitionWithExternalBases(context.Background(), def, planner.Options{
		Mode: planner.Build, Platform: "linux/amd64",
	}, func(context.Context, string, v1.Platform) (ResolvedImageSource, error) {
		return ResolvedImageSource{ImageID: "immutable-payload", ConfigData: []byte("not image config")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	key := ResolvedBaseKey{Reference: reference, Platform: "linux/amd64"}
	if selected[key].ImageID != "immutable-payload" || len(plan.Stages) != 1 {
		t.Fatalf("plan=%+v selected=%+v", plan, selected)
	}
}

func TestDynamicReplanReportsNewBaseSelectionAsExplicitDelta(t *testing.T) {
	const reference = "registry.invalid/coopr/onbuild-input:tag"
	def, err := definition.Parse(strings.NewReader(`from "scratch" as="base"
component "local:config"
from "base"
`))
	if err != nil {
		t.Fatal(err)
	}
	selectedImage := ResolvedImageSource{ImageID: "immutable-onbuild-input"}
	plan, selections, err := planDefinitionWithExternalBaseState(context.Background(), def, planner.Options{
		Mode: planner.Build, Platform: "linux/amd64",
	}, func(_ context.Context, got string, _ v1.Platform) (ResolvedImageSource, error) {
		if got != reference {
			t.Fatalf("resolved %q, want %q", got, reference)
		}
		return selectedImage, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selections.all) != 0 || len(plan.Stages) != 2 || plan.Stages[1].DynamicBaseStage != "0" {
		t.Fatalf("initial plan=%+v selections=%+v", plan.Stages, selections.all)
	}
	if _, err := plan.ReplanDynamicStage("1", planner.StageBind{Inherited: []definition.Instruction{{
		Name: "copy", Arguments: []string{"/payload", "/payload"}, Properties: map[string]string{"from": reference},
	}}}); err != nil {
		t.Fatal(err)
	}
	key := ResolvedBaseKey{Reference: reference, Platform: "linux/amd64"}
	delta := selections.takeDelta()
	if len(delta) != 1 || delta[key].ImageID != selectedImage.ImageID {
		t.Fatalf("replan selection delta = %+v", delta)
	}
	if next := selections.takeDelta(); len(next) != 0 {
		t.Fatalf("selection delta was not consumed: %+v", next)
	}
}
