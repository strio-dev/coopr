package planner

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"coopr/internal/buildcontext"
	"coopr/internal/definition"
)

func deliveredInput(kind, source string) (string, error) {
	return strings.Replace(source, "origin.example/", "delivered.example/", 1), nil
}

func TestExternalInputMappingUsesExpandedReferencesBeforeResolution(t *testing.T) {
	def := parse(t, `arg "registry" "origin.example"
from "${registry}/base:v1" {
    arg "registry"
    copy "/payload" "/copy" from="${registry}/copy:v1"
    add "/payload" "/add" from="${registry}/add:v1"
    run "true" { mount "bind" from="${registry}/mount:v1" target="/input"; }
    layer { component "${registry}/component:v1"; }
}
`)
	var selected []string
	var deferred []string
	var mapped []Input
	opts := Options{MapExternalInput: func(kind, source string) (string, error) {
		mapped = append(mapped, Input{Kind: kind, Reference: source})
		return deliveredInput(kind, source)
	}, DeferImageSource: func(source string) (bool, error) {
		deferred = append(deferred, source)
		return false, nil
	}}
	plan, err := CreateDemandDriven(def, opts, func(source FromSource) (StageBind, error) {
		selected = append(selected, source.Source)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantImages := []string{"delivered.example/base:v1", "delivered.example/copy:v1", "delivered.example/add:v1", "delivered.example/mount:v1"}
	if !reflect.DeepEqual(selected, wantImages) {
		t.Fatalf("resolved inputs = %v, want %v", selected, wantImages)
	}
	if !reflect.DeepEqual(deferred, wantImages[:1]) {
		t.Fatalf("defer selection saw original reference: %v", deferred)
	}
	var wantInputs []Input
	for _, image := range wantImages {
		wantInputs = append(wantInputs, Input{Kind: "image", Reference: image})
	}
	wantInputs = append(wantInputs, Input{Kind: "component", Reference: "delivered.example/component:v1"})
	if !reflect.DeepEqual(plan.Inputs, wantInputs) {
		t.Fatalf("plan inputs = %+v, want %+v", plan.Inputs, wantInputs)
	}
	if !slices.Contains(mapped, Input{Kind: "component", Reference: "origin.example/component:v1"}) ||
		!slices.Contains(mapped, Input{Kind: "image", Reference: "origin.example/base:v1"}) {
		t.Fatalf("mapper did not receive classified input kinds: %+v", mapped)
	}
	for _, operation := range plan.Stages[0].Operations {
		if operation.Name == "component" && operation.OriginalComponentReference != "origin.example/component:v1" {
			t.Fatalf("component lost its expanded selector: %+v", operation)
		}
	}
	if def.Instructions[1].Arguments[0] != "${registry}/base:v1" {
		t.Fatal("mapping modified the authored definition")
	}
	sources, err := ResolveFromSources(def, opts)
	if err != nil || len(sources) != 1 || sources[0].Source != wantImages[0] {
		t.Fatalf("FROM-only resolution = %+v, error %v", sources, err)
	}
}

func TestExternalInputMappingLeavesStagesContextsAndLocalComponents(t *testing.T) {
	def := parse(t, `from "scratch" as="producer" {}
from "producer" as="middle" {}
from "ignored.example/base:v1" as="overridden" {}
from "scratch" {
    copy "/payload" "/stage" from="middle"
    copy "/payload" "/context" from="assets"
    copy "/payload" "/overridden" from="overridden"
    run "true" { mount "bind" from="0" target="/input"; }
    run "true" { mount "bind" from="assets" target="/input"; }
    component "./component.coopr"
}
`)
	mapper := func(kind, source string) (string, error) {
		t.Errorf("mapper received local %s %q", kind, source)
		return "", errors.New("unexpected external input")
	}
	contexts := []buildcontext.Spec{
		{Name: "assets", Kind: buildcontext.Local, Path: "/assets"},
		{Name: "overridden", Kind: buildcontext.Local, Path: "/override"},
	}
	plan, err := CreateDemandDriven(def, Options{BuildContexts: contexts, MapExternalInput: mapper}, func(source FromSource) (StageBind, error) {
		if source.Kind != FromSourceContext {
			t.Fatalf("local input resolved externally: %+v", source)
		}
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(plan.Inputs, Input{Kind: "component", Reference: "./component.coopr"}) {
		t.Fatalf("local component was lost: %+v", plan.Inputs)
	}
	for _, stage := range plan.Stages {
		for _, operation := range stage.Operations {
			if operation.OriginalComponentReference != "" {
				t.Fatalf("unmapped operation acquired an original selector: %+v", operation)
			}
		}
	}
	if _, err := ResolveFromSources(def, Options{BuildContexts: contexts, MapExternalInput: mapper}); err != nil {
		t.Fatal(err)
	}
}

func TestExternalInputMappingRejectsChangingInputClass(t *testing.T) {
	for _, replacement := range []string{"", "scratch", "producer", "assets", "0", "./local.coopr", "/local", "bad source", "$unexpanded", "nul\x00source"} {
		t.Run(replacement, func(t *testing.T) {
			def := parse(t, `from "scratch" as="producer" {}
from "origin.example/base:v1" {}
`)
			opts := Options{
				MapExternalInput: func(string, string) (string, error) { return replacement, nil },
				BuildContexts:    []buildcontext.Spec{{Name: "assets", Kind: buildcontext.Local, Path: "/assets"}},
			}
			if _, err := Create(def, opts); err == nil || !strings.Contains(err.Error(), "mapped to") {
				t.Fatalf("mapped %q: %v", replacement, err)
			}
			if _, err := ResolveFromSources(def, opts); err == nil || !strings.Contains(err.Error(), "mapped to") {
				t.Fatalf("FROM-only mapped %q: %v", replacement, err)
			}
		})
	}
	for _, replacement := range []string{"./local.coopr", "../local.coopr", "/local.coopr"} {
		_, err := Create(parse(t, `from "scratch" { component "origin.example/component:v1"; }`), Options{
			MapExternalInput: func(string, string) (string, error) { return replacement, nil },
		})
		if err == nil || !strings.Contains(err.Error(), "mapped to") {
			t.Fatalf("component mapped %q: %v", replacement, err)
		}
	}
}

func TestExternalInputMappingPreservesErrors(t *testing.T) {
	failure := errors.New("ambiguous delivery")
	_, err := Create(parse(t, `from "origin.example/base:v1" {}`), Options{
		MapExternalInput: func(string, string) (string, error) { return "", failure },
	})
	if !errors.Is(err, failure) {
		t.Fatalf("mapping error lost identity: %v", err)
	}
}

func TestExternalInputMappingKeepsDistinctComponentSelectors(t *testing.T) {
	plan, err := Create(parse(t, `from "scratch" {
    component "origin.example/first:v1"
    component "origin.example/second:v1"
}
`), Options{MapExternalInput: func(string, string) (string, error) {
		return "delivered.example/shared:v1", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	operations := plan.Stages[0].Operations
	if len(operations) != 2 || operations[0].Arguments[0] != operations[1].Arguments[0] ||
		operations[0].OriginalComponentReference != "origin.example/first:v1" ||
		operations[1].OriginalComponentReference != "origin.example/second:v1" {
		t.Fatalf("mapped aliases lost their separate selection contexts: %+v", operations)
	}
}

func TestExternalInputMappingCoversInheritedOnBuild(t *testing.T) {
	var selected []string
	plan, err := CreateDemandDriven(parse(t, `from "origin.example/base:v1" {}`), Options{MapExternalInput: deliveredInput}, func(source FromSource) (StageBind, error) {
		selected = append(selected, source.Source)
		if source.Kind != FromSourceImage {
			return StageBind{}, nil
		}
		return StageBind{BaseEnvironment: map[string]string{"assets": "origin.example/assets:v1"}, Inherited: []definition.Instruction{
			{Name: "copy", Arguments: []string{"/payload", "/payload"}, Properties: map[string]string{"from": "${assets}"}},
			{Name: "run", Arguments: []string{"true"}, Children: []definition.Instruction{{Name: "mount", Arguments: []string{"bind"}, Properties: map[string]string{"from": "${assets}", "target": "/assets"}}}},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"delivered.example/base:v1", "delivered.example/assets:v1", "delivered.example/assets:v1"}
	if !reflect.DeepEqual(selected, want) {
		t.Fatalf("inherited external inputs = %v, want %v", selected, want)
	}
	if got := plan.Stages[0].Operations[0].Properties["from"]; got != want[1] {
		t.Fatalf("inherited COPY source = %q", got)
	}
}

func TestExternalInputMappingSurvivesComponentReturnAndExecutionRecipe(t *testing.T) {
	def := parse(t, `from "scratch" {
    component "./environment.coopr"
    layer {
        copy "/payload" "/payload" from="${assets}"
        component "${nested}"
    }
}
`)
	plan, err := Create(def, Options{AwaitComponentReturns: true, MapExternalInput: deliveredInput})
	if err != nil {
		t.Fatal(err)
	}
	for _, recipe := range []bool{false, true} {
		t.Run(map[bool]string{false: "original", true: "execution-recipe"}[recipe], func(t *testing.T) {
			current := plan
			if recipe {
				source, options, available := plan.ExecutionRecipe()
				if !available || options.MapExternalInput == nil {
					t.Fatal("execution recipe lost external mapping")
				}
				current, err = Create(source, options)
				if err != nil {
					t.Fatal(err)
				}
			}
			continued, err := current.ReplanComponentReturn(*current.Stages[0].AwaitingComponent, map[string]string{
				"assets": "origin.example/assets:v1", "nested": "origin.example/nested:v1",
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []Input{{Kind: "image", Reference: "delivered.example/assets:v1"}, {Kind: "component", Reference: "delivered.example/nested:v1"}} {
				if !slices.Contains(continued.Inputs, want) {
					t.Fatalf("continued input missing %+v: %+v", want, continued.Inputs)
				}
			}
		})
	}
}

func TestExternalInputMappingSurvivesDynamicBaseReplan(t *testing.T) {
	plan, err := CreateDemandDriven(parse(t, `from "oci:/tmp/base" {
    copy "/payload" "/payload" from="${assets}"
}
`), Options{MapExternalInput: deliveredInput}, func(FromSource) (StageBind, error) { return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Stages[0].DeferredImageSource {
		t.Fatal("expected deferred base")
	}
	continued, err := plan.ReplanDynamicStage("0", StageBind{BaseEnvironment: map[string]string{"assets": "origin.example/assets:v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := continued.Stages[0].Operations[0].Properties["from"]; got != "delivered.example/assets:v1" {
		t.Fatalf("dynamic replan source = %q", got)
	}
}
