package planner

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"coopr/internal/definition"
)

func TestLayerPlanningSharesArgumentScopeAndDependencyBindings(t *testing.T) {
	p := makePlan(t, `from "scratch" as="producer"
copy "source" "/source"
from "scratch" as="output"
layer {
 arg "destination" "/before"
 copy "/source" "${destination}" from="producer"
 layer { env "destination" "/after"; run "true" { mount "bind" from="producer" source="/" target="/mnt" } }
 copy "/source" "${destination}" from="producer"
 component "./shared.coopr" dest="${destination}"
}
copy "/source" "${destination}" from="producer"
`, Options{Mode: Build, TransientRunMounts: []definition.Instruction{{Name: "mount", Arguments: []string{"tmpfs"}, Properties: map[string]string{"target": "/tmp"}}}})
	stage := p.Stages[len(p.Stages)-1]
	if !slices.Equal(stage.Dependencies, []string{"0"}) {
		t.Fatalf("grouped source dependencies: %v", stage.Dependencies)
	}
	var destinations []string
	var run Operation
	for _, op := range stage.Operations {
		if op.Name == "copy" {
			destinations = append(destinations, op.Arguments[1])
		}
		if op.Name == "run" {
			run = op
		}
		if op.Name == "component" && op.Properties["dest"] != "/after" {
			t.Fatalf("grouped component argument: %+v", op)
		}
	}
	if !reflect.DeepEqual(destinations, []string{"/before", "/after", "/after"}) {
		t.Fatalf("grouped scope: %v", destinations)
	}
	if len(run.Children) != 2 || run.TransientMountCount != 1 {
		t.Fatalf("global mount omitted in layer: %+v", run)
	}
}

func TestLayerPublishedNumericReferencesAndNestedBoundaries(t *testing.T) {
	publication := makePlan(t, `from "scratch" as="unused"
extend as="caller"
from "caller" as="output"
layer {
 arg "destination" "/artifact"
 layer { copy "/source" "${destination}" from="1" }
 run "true" { mount "bind" from="1" target="/input" }
}
`, Options{Mode: Publish})
	if _, err := ValidatePublished(publication.Component); err != nil {
		t.Fatal(err)
	}
	invocation := invokePublished(t, publication, Options{Mode: Invoke})
	var copies, mounts int
	for _, stage := range invocation.Stages {
		for _, op := range stage.Operations {
			if op.Name == "copy" {
				copies++
				if op.Properties["from"] != "0" {
					t.Fatalf("numeric grouped copy not rebound: %+v", op)
				}
			}
			if op.Name == "run" {
				mounts++
				if op.Children[0].Properties["from"] != "0" {
					t.Fatalf("numeric grouped mount not rebound: %+v", op)
				}
			}
		}
	}
	if copies != 1 || mounts != 1 {
		t.Fatalf("grouped ops lost: copies=%d mounts=%d", copies, mounts)
	}
}

func TestReplacementOutputRetainsMandatoryCompatibilityWithoutDormantBodyExecution(t *testing.T) {
	source := `arg "distribution" "ubuntu"
extend distro="${distribution}" as="caller"
copy "${missing}" "/unused"
from "scratch" as="replacement"
env "output" "replacement"
`
	publication, err := Create(parse(t, source), Options{Mode: Publish})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(publication.Component.DormantRoots, []string{"0"}) {
		t.Fatalf("mandatory extend missing: %+v", publication.Component)
	}
	encoded, _ := json.Marshal(publication.Component)
	var decoded PublishedComponent
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	invocation, err := InstantiateDemandDriven(&decoded, Options{Mode: Invoke}, nil, func(FromSource) (StageBind, error) {
		t.Fatal("scratch output must not resolve external inputs")
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, stage := range invocation.CompatibilityRoots {
		if stage.Kind == "extend" {
			found = true
			if !slices.Equal(stage.Requirements["distro"], []string{"ubuntu"}) || len(stage.Operations) != 0 {
				t.Fatalf("dormant compatibility contract/body: %+v", stage)
			}
		}
	}
	if !found {
		t.Fatal("replacement output lost mandatory EXTEND compatibility")
	}
	if len(invocation.Outputs) != 1 || invocation.Outputs[0] != "1" {
		t.Fatalf("selected output changed: %+v", invocation.Outputs)
	}
}

func TestLayerPublicationRetainsNestedSyntaxAndOperationOrdinals(t *testing.T) {
	publication := makePlan(t, `package as="assets"
layer { copy "input" "/source" }
extend
layer {
 layer { copy "/source" "/source" from="assets" }
 run "true" { mount "bind" from="assets" target="/input" }
}
`, Options{Mode: Publish})
	if len(publication.Stages) != 1 || publication.Stages[0].Kind != "package" {
		t.Fatalf("group altered package producers: %+v", publication.Stages)
	}
	body := publication.Component.Definition.Instructions
	if len(body) != 3 || body[2].Name != "layer" || len(body[2].Children) != 2 || body[2].Children[0].Name != "layer" {
		t.Fatalf("nested publication syntax flattened: %+v", body)
	}
	for _, ref := range publication.Component.StageReferences {
		if ref.Operation != 2 && ref.Operation != 4 {
			t.Fatalf("group reference ordinal drift: %+v", ref)
		}
	}
	if _, err := ValidatePublished(publication.Component); err != nil {
		t.Fatal(err)
	}
	invocation := invokePublished(t, publication, Options{Mode: Invoke, TransientRunMounts: []definition.Instruction{{Name: "mount", Arguments: []string{"tmpfs"}, Properties: map[string]string{"target": "/tmp"}}}})
	if len(invocation.Stages) != 2 || invocation.Stages[0].Kind != "package-input" {
		t.Fatalf("package producer replayed: %+v", invocation.Stages)
	}
	for _, op := range invocation.Stages[1].Operations {
		if op.Name == "run" && (len(op.Children) != 2 || op.TransientMountCount != 1) {
			t.Fatalf("grouped published run lost global mount: %+v", op)
		}
	}
}

func TestReplacementScratchOutputPrunesDormantBodyAndItsPackageInputs(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `from "unreachable.invalid/image" as="producer"
package as="assets"
copy "/source" "/source" from="producer"
extend distro="ubuntu" as="caller"
copy "/source" "/source" from="assets"
from "scratch" as="replacement"
`), Options{Mode: Publish}, func(source FromSource) (StageBind, error) {
		t.Fatalf("dormant package source resolved: %+v", source)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(publication.Stages) != 0 || len(publication.Outputs) != 0 {
		t.Fatalf("unused package producers selected: %+v", publication)
	}
	component := publication.Component
	if !slices.Equal(component.DormantRoots, []string{"0"}) || component.Output != "1" || len(component.Definition.Instructions) != 2 {
		t.Fatalf("replacement contract retained unused body: %+v", component)
	}
	if _, err := ValidatePublished(component); err != nil {
		t.Fatal(err)
	}
	invocation := invokePublished(t, publication, Options{Mode: Invoke})
	if len(invocation.Stages) != 1 || len(invocation.CompatibilityRoots) != 1 || invocation.CompatibilityRoots[0].Requirements["distro"][0] != "ubuntu" {
		t.Fatalf("replacement compatibility/execution roots mixed: %+v", invocation)
	}
}

func TestReplacementGeneratedSourceKeepsAfterAndCompatibility(t *testing.T) {
	publication := makePlan(t, `extend distro="ubuntu" as="caller"
from "scratch" as="producer"
run "produce"
from "oci:out" after="producer" as="replacement"
`, Options{Mode: Publish})
	if _, err := ValidatePublished(publication.Component); err != nil {
		t.Fatal(err)
	}
	invocation, err := InstantiateDemandDriven(publication.Component, Options{Mode: Invoke}, nil, func(source FromSource) (StageBind, error) {
		t.Fatalf("deferred source resolved before producer: %+v", source)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stageIDs(invocation), []string{"1", "2"}) || !slices.Equal(invocation.Stages[1].Dependencies, []string{"1"}) || len(invocation.CompatibilityRoots) != 1 {
		t.Fatalf("generated replacement graph/contract: %+v", invocation)
	}
	if !invocation.Stages[1].DeferredImageSource {
		t.Fatal("generated source must be rebound after producer")
	}
	replanned, err := invocation.ReplanDynamicStage("2", StageBind{BaseEnvironment: map[string]string{"native": "selected"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(replanned.CompatibilityRoots) != 1 {
		t.Fatal("dynamic source replan dropped caller compatibility")
	}
}

func TestSelectedExtendDoesNotConjoinUnrelatedDormantTargetCompatibility(t *testing.T) {
	publication := makePlan(t, `extend distro="alpine" as="unused"
from "helper" as="prepared"
extend distro="ubuntu" as="selected"
copy "/asset" "/asset" from="prepared"
`, Options{Mode: Publish, Target: "selected"})
	if len(publication.Component.DormantRoots) == 0 {
		t.Fatal("fixture must retain dormant target for external ONBUILD indices")
	}
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(FromSource) (StageBind, error) { return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(invocation.CompatibilityRoots) != 0 {
		t.Fatalf("unrelated dormant target requirements conjoined: %+v", invocation.CompatibilityRoots)
	}
	selected := invocation.Stages[len(invocation.Stages)-1]
	if !slices.Equal(selected.Requirements["distro"], []string{"ubuntu"}) {
		t.Fatalf("selected target contract lost: %+v", selected)
	}
}

func TestComponentFromReplansActualConfigurationAfterEarlierComponentCall(t *testing.T) {
	publication := makePlan(t, `extend as="first"
env child="./setting.coopr"
component "./setting.coopr"
from "first"
component "$child"
`, Options{Mode: Publish})
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(FromSource) (StageBind, error) {
		t.Fatal("caller-based stages must not resolve external images")
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	last := invocation.Stages[len(invocation.Stages)-1]
	if last.DynamicBaseStage != "0" || len(last.Operations) != 0 {
		t.Fatalf("component-derived FROM expanded against stale authored configuration: %+v", last)
	}
	rebound, err := invocation.ReplanDynamicStage(last.ID, StageBind{BaseEnvironment: map[string]string{"child": "./replacement.coopr"}})
	if err != nil {
		t.Fatal(err)
	}
	last = rebound.Stages[len(rebound.Stages)-1]
	if len(last.Operations) != 1 || last.Operations[0].Arguments[0] != "./replacement.coopr" {
		t.Fatalf("component-derived FROM did not use executed selected configuration: %+v", last)
	}
}

func TestDynamicComponentFromDefersGroupedSourceBindingChecksUntilReplan(t *testing.T) {
	publication := makePlan(t, `package as="assets"
copy "input" "/asset"
extend as="first"
component "./setting.coopr"
from "first"
layer {
 copy "/asset" "/asset" from="assets"
 run "true" { mount "bind" from="assets" target="/input" }
}
`, Options{Mode: Publish})
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(FromSource) (StageBind, error) {
		t.Fatal("caller-based stages must not resolve images")
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	output := invocation.Stages[len(invocation.Stages)-1]
	if output.DynamicBaseStage != "1" || len(output.Operations) != 0 {
		t.Fatalf("grouped derived body must defer: %+v", output)
	}
	rebound, err := invocation.ReplanDynamicStage(output.ID, StageBind{BaseEnvironment: map[string]string{"native": "selected"}})
	if err != nil {
		t.Fatal(err)
	}
	packageFound := false
	for _, stage := range rebound.Stages {
		packageFound = packageFound || stage.ID == "0" && stage.Kind == "package-input"
	}
	if !packageFound {
		t.Fatalf("replan dropped grouped package dependency: %+v", rebound.Stages)
	}
	output = rebound.Stages[len(rebound.Stages)-1]
	if len(output.Operations) != 4 || output.Operations[1].Properties["from"] != "assets" || output.Operations[2].Children[0].Properties["from"] != "assets" {
		t.Fatalf("replan changed grouped publication bindings: %+v", output)
	}
}

func TestDynamicComponentFromStillRejectsChangedPublishedSourceRole(t *testing.T) {
	publication := makePlan(t, `package as="assets"
copy "input" "/asset"
extend as="first"
env source="registry.example/input:latest"
component "./setting.coopr"
from "first"
copy "/asset" "/kept" from="assets"
layer { copy "/asset" "/asset" from="${source}" }
`, Options{Mode: Publish})
	calls := 0
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(FromSource) (StageBind, error) {
		calls++
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("unbound dynamic base resolved %d authored external sources", calls)
	}
	output := invocation.Stages[len(invocation.Stages)-1]
	if _, err := invocation.ReplanDynamicStage(output.ID, StageBind{BaseEnvironment: map[string]string{"source": "assets"}}); err == nil {
		t.Fatal("dynamic replan changed published image source into package-stage source")
	}
}

func TestIndependentComponentOutputUsesOnlyNearestPrecedingExtendContract(t *testing.T) {
	for _, base := range []string{"scratch", "registry.example/output:latest"} {
		t.Run(base, func(t *testing.T) {
			publication := makePlan(t, `extend distro="alpine" as="earlier"
extend distro="ubuntu" as="caller"
from "`+base+`" as="selected"
env "output" "selected"
extend distro="fedora" as="later"
`, Options{Mode: Publish, Target: "selected"})
			invocation, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(FromSource) (StageBind, error) { return StageBind{}, nil })
			if err != nil {
				t.Fatal(err)
			}
			if len(invocation.CompatibilityRoots) != 1 || !slices.Equal(invocation.CompatibilityRoots[0].Requirements["distro"], []string{"ubuntu"}) {
				t.Fatalf("independent target conjoined unrelated contracts: %+v", invocation.CompatibilityRoots)
			}
			if len(invocation.Stages) != 1 || invocation.Stages[0].Kind != "from" {
				t.Fatalf("independent target scheduled unused bodies: %+v", invocation.Stages)
			}
		})
	}
}

func TestComponentCannotSelectProducerBeforeMandatoryExtend(t *testing.T) {
	if _, err := Create(parse(t, `from "scratch" as="producer"
extend as="caller"
`), Options{Mode: Publish, Target: "producer"}); err == nil {
		t.Fatal("pre-EXTEND producer selected as component output")
	}
	malformed := &PublishedComponent{Definition: parse(t, `from "scratch" as="producer"
extend as="caller"
`), Output: "0", Platform: "linux/amd64", FromBindings: map[string]FromBinding{"0": {Kind: "scratch"}}, ReservedStageNames: []string{"caller", "producer"}, DormantRoots: []string{"1"}}
	if _, err := ValidatePublished(malformed); err == nil {
		t.Fatal("published pre-EXTEND producer accepted as component output")
	}
}
