package planner

import (
	"reflect"
	"slices"
	"testing"

	"github.com/strio-dev/coopr/internal/definition"
)

func TestLayerPlanningSharesArgumentScopeAndDependencyBindings(t *testing.T) {
	p := makePlan(t, `from "scratch" as="producer" {
    copy "source" "/source"
}
from "scratch" as="output" {
    layer {
     arg "destination" "/before"
     copy "/source" "${destination}" from="producer"
     layer { env "destination" "/after"; run "true" { mount "bind" from="producer" source="/" target="/mnt" } }
     copy "/source" "${destination}" from="producer"
     component "./shared.coopr" { arg "dest" "${destination}" }
    }
    copy "/source" "${destination}" from="producer"
}
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
		if op.Name == "component" && op.Children[0].Arguments[1] != "/after" {
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
	publication := makePlan(t, `from "scratch" as="unused" {}
extend as="caller" {}
extend as="output" {
    from "caller" {
        layer {
         arg "destination" "/artifact"
         layer { copy "/source" "${destination}" from="1" }
         run "true" { mount "bind" from="1" target="/input" }
        }
    }
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
				if op.Properties["from"] != "1" {
					t.Fatalf("numeric grouped copy changed original identity: %+v", op)
				}
			}
			if op.Name == "run" {
				mounts++
				if op.Children[0].Properties["from"] != "1" {
					t.Fatalf("numeric grouped mount changed original identity: %+v", op)
				}
			}
		}
	}
	if copies != 1 || mounts != 1 {
		t.Fatalf("grouped ops lost: copies=%d mounts=%d", copies, mounts)
	}
}

func TestReplacementOutputRetainsMandatoryExtendWithoutDormantBodyExecution(t *testing.T) {
	source := `extend as="caller" {
    copy "${missing}" "/unused"
}
extend as="replacement" {
    from "scratch" {
        env "output" "replacement"
    }
}
`
	publication, err := Create(parse(t, source), Options{Mode: Publish})
	if err != nil {
		t.Fatal(err)
	}
	if len(publication.Component.Definition.Instructions) != 2 || publication.Component.Definition.Instructions[0].Properties["as"] != "caller" {
		t.Fatalf("original EXTEND namespace lost: %+v", publication.Component)
	}
	decoded := roundTripPublished(t, publication.Component)
	invocation, err := InstantiateDemandDriven(&decoded, Options{Mode: Invoke}, nil, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Definition.Instructions) != 2 || decoded.Definition.Instructions[0].Name != "extend" || len(invocation.Stages) != 1 || invocation.Stages[0].Kind != "from" {
		t.Fatalf("mandatory EXTEND or selected execution changed: publication=%+v invocation=%+v", decoded, invocation)
	}
	if len(invocation.Outputs) != 1 || invocation.Outputs[0] != "2" {
		t.Fatalf("selected output changed: %+v", invocation.Outputs)
	}
}

func TestLayerPublicationRetainsNestedSyntaxAndOperationOrder(t *testing.T) {
	publication := makePlan(t, `package as="assets" {
    layer { copy "input" "/source" }
}
extend {
    layer {
     layer { copy "/source" "/source" from="assets" }
     run "true" { mount "bind" from="assets" target="/input" }
    }
}
`, Options{Mode: Publish})
	if len(publication.Stages) != 1 || publication.Stages[0].Kind != "package" {
		t.Fatalf("group altered package producers: %+v", publication.Stages)
	}
	body := publication.Component.Definition.Instructions[1].Children
	if len(body) != 1 || body[0].Name != "layer" || len(body[0].Children) != 2 || body[0].Children[0].Name != "layer" {
		t.Fatalf("nested publication syntax flattened: %+v", body)
	}

	if _, err := ValidatePublished(publication.Component); err != nil {
		t.Fatal(err)
	}
	invocation := invokePublished(t, publication, Options{Mode: Invoke, TransientRunMounts: []definition.Instruction{{Name: "mount", Arguments: []string{"tmpfs"}, Properties: map[string]string{"target": "/tmp"}}}})
	if len(invocation.Stages) != 2 || invocation.Stages[0].Kind != "package-input" {
		t.Fatalf("package producer replayed: %+v", invocation.Stages)
	}
	var ordered []string
	for _, op := range invocation.Stages[1].Operations {
		name := op.Name
		if op.LayerBoundary != "" {
			name += "-" + op.LayerBoundary
		}
		ordered = append(ordered, name)
		if op.Name == "copy" && op.Properties["from"] != "assets" {
			t.Fatalf("grouped copy source lost: %+v", op)
		}
	}
	if !slices.Equal(ordered, []string{"layer-begin", "layer-begin", "copy", "layer-end", "run", "layer-end"}) {
		t.Fatalf("layer boundaries/operation order changed: %v", ordered)
	}

	for _, op := range invocation.Stages[1].Operations {
		if op.Name == "run" && (len(op.Children) != 2 || op.TransientMountCount != 1) {
			t.Fatalf("grouped published run lost global mount: %+v", op)
		}
	}
}

func TestReplacementScratchOutputSkipsUnselectedBodyAndPackageInputs(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `from "unreachable.invalid/image" as="producer" {}
package as="assets" {
    copy "/source" "/source" from="producer"
}
extend as="caller" {
    copy "/source" "/source" from="assets"
}
extend as="replacement" {
    from "scratch" {}
}
`), Options{Mode: Publish}, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(publication.Stages) != 0 || len(publication.Outputs) != 0 {
		t.Fatalf("unused package producers selected: %+v", publication)
	}
	component := publication.Component
	if component.Output != "4" || len(component.Definition.Instructions) != 4 || !component.Definition.Instructions[1].UnavailablePackage {
		t.Fatalf("replacement residual lost original namespace/unavailable package: %+v", component)
	}
	if _, err := ValidatePublished(component); err != nil {
		t.Fatal(err)
	}
	invocation := invokePublished(t, publication, Options{Mode: Invoke})
	if len(invocation.Stages) != 1 || invocation.Stages[0].Kind != "from" {
		t.Fatalf("replacement scheduled dormant caller root: %+v", invocation)
	}
}

func TestReplacementGeneratedSourceKeepsAfterAndOwningExtend(t *testing.T) {
	publication := makePlan(t, `extend as="caller" {}
extend as="producer" {
    from "scratch" {
        run "produce"
    }
}
extend as="replacement" {
    from "oci:out" after="producer" {}
}
`, Options{Mode: Publish})
	if _, err := ValidatePublished(publication.Component); err != nil {
		t.Fatal(err)
	}
	invocation, err := InstantiateDemandDriven(publication.Component, Options{Mode: Invoke}, nil, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stageIDs(invocation), []string{"2", "4"}) || !slices.Equal(invocation.Stages[1].Dependencies, []string{"2"}) {
		t.Fatalf("generated replacement graph: %+v", invocation)
	}
	if !invocation.Stages[1].DeferredImageSource {
		t.Fatal("generated source must be rebound after producer")
	}
	replanned, err := invocation.ReplanDynamicStage("4", StageBind{BaseEnvironment: map[string]string{"native": "selected"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stageIDs(replanned), []string{"2", "4"}) {
		t.Fatalf("dynamic source replan changed execution stages: %+v", replanned.Stages)
	}
}

func TestSelectedExtendDoesNotScheduleUnrelatedDormantTarget(t *testing.T) {
	publication := makePlan(t, `extend as="unused" {}
extend as="prepared" {
    from "helper" {}
}
extend as="selected" {
    copy "/asset" "/asset" from="prepared"
}
`, Options{Mode: Publish, Target: "selected"})
	if publication.Component.Definition.Instructions[0].Properties["as"] != "unused" {
		t.Fatal("fixture lost original target namespace")
	}
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, emptyPackageBinds(t, publication.Component), func(FromSource) (StageBind, error) { return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	selected := invocation.Stages[len(invocation.Stages)-1]
	if selected.Kind != "extend" || selected.Name != "selected" {
		t.Fatalf("selected target lost: %+v", selected)
	}
	for _, stage := range invocation.Stages {
		if stage.Name == "unused" {
			t.Fatalf("dormant target scheduled: %+v", stage)
		}
	}
}

func TestComponentFromReplansActualConfigurationAfterEarlierComponentCall(t *testing.T) {
	publication := makePlan(t, `extend as="first" {
    env child="./setting.coopr"
    component "./setting.coopr"
}
extend {
    from "first" {
        component "$child"
    }
}
`, Options{Mode: Publish})
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, emptyPackageBinds(t, publication.Component), unexpectedBindResolver(t))
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
	publication := makePlan(t, `package as="assets" {
    copy "input" "/asset"
}
extend as="first" {
    component "./setting.coopr"
}
extend {
    from "first" {
        layer {
         copy "/asset" "/asset" from="assets"
         run "true" { mount "bind" from="assets" target="/input" }
        }
    }
}
`, Options{Mode: Publish})
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, emptyPackageBinds(t, publication.Component), unexpectedBindResolver(t))
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

func TestDynamicComponentFromResolvesReturnedSourceRole(t *testing.T) {
	publication := makePlan(t, `package as="assets" {
    copy "input" "/asset"
}
extend as="first" {
    env source="registry.example/input:latest"
    component "./setting.coopr"
}
extend {
    from "first" {
        copy "/asset" "/kept" from="assets"
        layer { copy "/asset" "/asset" from="${source}" }
    }
}
`, Options{Mode: Publish})
	calls := 0
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, emptyPackageBinds(t, publication.Component), func(FromSource) (StageBind, error) {
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
	replanned, err := invocation.ReplanDynamicStage(output.ID, StageBind{BaseEnvironment: map[string]string{"source": "assets"}})
	if err != nil {
		t.Fatal(err)
	}
	output = replanned.Stages[len(replanned.Stages)-1]
	if !slices.Contains(output.Dependencies, "0") || output.Operations[2].Properties["from"] != "assets" || calls != 0 {
		t.Fatalf("returned source did not resolve as captured package: %+v calls=%d", output, calls)
	}
}

func TestIndependentComponentOutputDoesNotSchedulePrecedingExtends(t *testing.T) {
	for _, base := range []string{"scratch", "registry.example/output:latest"} {
		t.Run(base, func(t *testing.T) {
			publication := makePlan(t, `extend as="earlier" {}
extend as="caller" {}
extend as="selected" {
    from "`+base+`" {
        env "output" "selected"
    }
}
extend as="later" {}
`, Options{Mode: Publish, Target: "selected"})
			invocation, err := InstantiateDemandDriven(publication.Component, Options{}, emptyPackageBinds(t, publication.Component), func(FromSource) (StageBind, error) { return StageBind{}, nil })
			if err != nil {
				t.Fatal(err)
			}
			if len(invocation.Stages) != 1 || invocation.Stages[0].Kind != "from" {
				t.Fatalf("independent target scheduled unused bodies: %+v", invocation.Stages)
			}
		})
	}
}

func TestComponentCannotSelectProducerBeforeMandatoryExtend(t *testing.T) {
	if _, err := Create(parse(t, `from "scratch" as="producer" {}
extend as="caller" {}
`), Options{Mode: Publish, Target: "producer"}); err == nil {
		t.Fatal("pre-EXTEND producer selected as component output")
	}
	malformed := &PublishedComponent{Definition: parse(t, `from "scratch" as="producer" {}
extend as="caller" {}
`), Output: "0", Platform: "linux/amd64", Format: ResidualFormat}
	if _, err := ValidatePublished(malformed); err == nil {
		t.Fatal("published pre-EXTEND producer accepted as component output")
	}
}
