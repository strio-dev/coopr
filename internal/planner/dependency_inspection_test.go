package planner

import (
	"reflect"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/strio-dev/coopr/internal/definition"
)

func TestPublishedDependencyFirstCallPreservesSelector(t *testing.T) {
	for _, test := range []struct {
		name      string
		reference string
		local     bool
	}{
		{"tag", "registry.example/components/tool:stable", false},
		{"digest", "registry.example/components/tool@" + digest.FromString("tool").String(), false},
		{"local target", "./shared.coopr#configured", true},
		{"parent path", "../shared.coopr", true},
		{"absolute path", "/source/shared.coopr", true},
		{"registry suffix", "registry.example/components/shared.coopr:stable", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			publication := makePlan(t, `extend { component "`+test.reference+`"; }`, Options{Mode: Publish})
			invocation := invokePublished(t, publication, Options{AwaitComponentReturns: true})
			assertPublishedDependencyInputs(t, invocation, []string{test.reference})
			if len(invocation.Stages) != 1 || invocation.Stages[0].AwaitingComponent == nil {
				t.Fatalf("first call must await its actual return: %+v", invocation.Stages)
			}
			if got := definition.IsLocalComponentReference(test.reference); got != test.local {
				t.Fatalf("local classification = %t, want %t", got, test.local)
			}
		})
	}
}

func TestPublishedDependencyDefaultAndFixedPackageArguments(t *testing.T) {
	t.Run("stage consumes global default", func(t *testing.T) {
		publication := makePlan(t, `arg "dependency" "registry.example/child:stable"
extend {
    arg "dependency"
    component "${dependency}"
}
`, Options{Mode: Publish})
		invocation := invokePublished(t, publication, Options{AwaitComponentReturns: true})
		assertPublishedDependencyInputs(t, invocation, []string{"registry.example/child:stable"})
	})
	t.Run("package input is fixed and producer is absent", func(t *testing.T) {
		publication := makePlan(t, `arg "channel" "stable"
package as="assets" {
    component "registry.example/package-builder:latest" { arg "channel" "${channel}"; }
}
extend {
    arg "channel"
    copy "/asset" "/asset" from="assets"
    component "registry.example/child:${channel}"
}
`, Options{Mode: Publish})
		if publication.Component.PackageArguments["channel"] != "stable" {
			t.Fatalf("fixed package arguments = %v", publication.Component.PackageArguments)
		}
		projected, err := ProjectPublished(publication.Component)
		if err != nil {
			t.Fatal(err)
		}
		for _, stage := range projected.Stages {
			if stage.Kind == "package" {
				for _, inst := range stage.Body {
					if inst.Name == "component" {
						t.Fatal("published package retained its already-executed component call")
					}
				}
			}
		}
		invocation := invokePublished(t, publication, Options{AwaitComponentReturns: true})
		assertPublishedDependencyInputs(t, invocation, []string{"registry.example/child:stable"})
		if _, err := Instantiate(publication.Component, Options{Arguments: map[string]string{"channel": "changed"}}); err == nil || !strings.Contains(err.Error(), "fixed at publication") {
			t.Fatalf("fixed argument override = %v", err)
		}
	})
}

func TestPublishedDependencySelectedClosureDeduplicatesPrefixes(t *testing.T) {
	publication := makePlan(t, `from "scratch" as="first" {
    component "registry.example/shared:stable"
}
from "scratch" as="second" {
    component "registry.example/shared:stable"
}
extend as="selected" {
    copy "/first" "/first" from="first"
    copy "/second" "/second" from="second"
    layer {
        layer { component "registry.example/leaf:stable"; }
    }
}
extend as="unused" { component "registry.example/unselected:stable"; }
`, Options{Mode: Publish, Target: "selected"})
	projected, err := ProjectPublished(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range projected.Stages {
		if stage.Name == "unused" {
			t.Fatal("unselected target survived closure pruning")
		}
	}
	invocation := invokePublished(t, publication, Options{AwaitComponentReturns: true})
	assertPublishedDependencyInputs(t, invocation, []string{"registry.example/shared:stable", "registry.example/leaf:stable"})
	awaiting := 0
	for _, stage := range invocation.Stages {
		if stage.AwaitingComponent != nil {
			awaiting++
		}
	}
	if awaiting != 3 {
		t.Fatalf("awaiting stages = %d, want both shared calls and the selected call", awaiting)
	}
}

func TestPublishedDependencyMissingValueIsNotACompleteInventory(t *testing.T) {
	publication := makePlan(t, `extend {
    component "registry.example/child:${missing}stable"
}
`, Options{Mode: Publish})
	projected, err := ProjectPublished(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	if got := projected.Stages[0].Body[0].Arguments[0]; got != "registry.example/child:${missing}stable" {
		t.Fatalf("publication lost the authored selector: %q", got)
	}
	// An absent caller value produces a syntactically ordinary selector. That
	// observed default prefix cannot stand for all invocation environments.
	neutral := invokePublished(t, publication, Options{AwaitComponentReturns: true})
	assertPublishedDependencyInputs(t, neutral, []string{"registry.example/child:stable"})
	caller := invokePublished(t, publication, Options{
		AwaitComponentReturns: true,
		CallerEnvironment:     map[string]string{"missing": "caller-"},
	})
	assertPublishedDependencyInputs(t, caller, []string{"registry.example/child:caller-stable"})
	if neutral.Stages[0].AwaitingComponent == nil || caller.Stages[0].AwaitingComponent == nil {
		t.Fatal("an observed selector must not remove the component-return boundary")
	}
}

func TestPublishedDependencyReturnedEnvironmentChangesNextSelector(t *testing.T) {
	publication := makePlan(t, `arg "dependency" "registry.example/child:default"
extend {
    arg "dependency"
    layer {
        component "registry.example/setup:stable"
        component "${dependency}"
    }
}
`, Options{Mode: Publish})
	prefix := invokePublished(t, publication, Options{AwaitComponentReturns: true})
	assertPublishedDependencyInputs(t, prefix, []string{"registry.example/setup:stable"})
	boundary := prefix.Stages[0].AwaitingComponent
	if boundary == nil {
		t.Fatal("first component did not stop planning")
	}
	continued, err := prefix.ReplanComponentReturn(*boundary, map[string]string{"dependency": "registry.example/child:returned"})
	if err != nil {
		t.Fatal(err)
	}
	assertPublishedDependencyInputs(t, continued, []string{"registry.example/setup:stable", "registry.example/child:returned"})
	if continued.Stages[0].AwaitingComponent == nil || *continued.Stages[0].AwaitingComponent == *boundary {
		t.Fatal("replay did not advance to the second component boundary")
	}
}

func TestPublishedDependencyExternalBaseConfigurationIsDeferred(t *testing.T) {
	publication := makePlan(t, `extend {
    from "registry.example/base:stable" {
        arg "dependency" "registry.example/child:default"
        component "${dependency}"
    }
}
`, Options{Mode: Publish})
	prefix := invokePublished(t, publication, Options{
		AwaitComponentReturns: true,
		DeferImageSource: func(reference string) (bool, error) {
			return reference == "registry.example/base:stable", nil
		},
	})
	assertPublishedDependencyInputs(t, prefix, nil)
	var deferred *Stage
	for i := range prefix.Stages {
		if prefix.Stages[i].DeferredImageSource {
			deferred = &prefix.Stages[i]
		}
	}
	if deferred == nil || len(deferred.Operations) != 0 {
		t.Fatalf("external configuration was expanded prematurely: %+v", prefix.Stages)
	}
	continued, err := prefix.ReplanDynamicStage(deferred.ID, StageBind{
		BaseEnvironment: map[string]string{"dependency": "registry.example/child:from-base"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertPublishedDependencyInputs(t, continued, []string{"registry.example/child:from-base"})
	last := continued.Stages[len(continued.Stages)-1]
	if last.DeferredImageSource || last.AwaitingComponent == nil {
		t.Fatalf("replay did not move from image configuration to component return: %+v", last)
	}
}

func TestPublishedDependencyComponentProducedBaseIsDeferred(t *testing.T) {
	publication := makePlan(t, `from "scratch" as="prepared" {
    component "registry.example/setup:stable"
}
extend {
    from "prepared" {
        arg "dependency" "registry.example/child:default"
        component "${dependency}"
    }
}
`, Options{Mode: Publish})
	prefix, err := InstantiateDemandDriven(publication.Component, Options{AwaitComponentReturns: true}, nil, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	assertPublishedDependencyInputs(t, prefix, []string{"registry.example/setup:stable"})
	if len(prefix.Stages) != 2 || prefix.Stages[0].AwaitingComponent == nil {
		t.Fatalf("setup prefix = %+v", prefix.Stages)
	}
	deferred := prefix.Stages[1]
	if deferred.DynamicBaseStage != prefix.Stages[0].ID || len(deferred.Operations) != 0 {
		t.Fatalf("dependent selector expanded before the component-produced base: %+v", deferred)
	}
	returned, err := prefix.ReplanComponentReturn(*prefix.Stages[0].AwaitingComponent, map[string]string{
		"dependency": "registry.example/child:from-component",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertPublishedDependencyInputs(t, returned, []string{"registry.example/setup:stable"})
	continued, err := returned.ReplanDynamicStage(deferred.ID, StageBind{
		BaseEnvironment: map[string]string{"dependency": "registry.example/child:from-component"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertPublishedDependencyInputs(t, continued, []string{"registry.example/setup:stable", "registry.example/child:from-component"})
	last := continued.Stages[len(continued.Stages)-1]
	if last.DynamicBaseStage != "" || last.AwaitingComponent == nil {
		t.Fatalf("replay did not advance to the dependent component: %+v", last)
	}
}

func TestPublishedDependencyInspectionIncludesRetainedLiteralRequirements(t *testing.T) {
	publication := makePlan(t, `package as="assets" {
    component "registry.example/package-builder:stable"
}
extend as="selected" {
    copy "/asset" "/asset" from="assets"
    layer {
        component "registry.example/first:stable"
        component "registry.example/second:stable"
        run "true" { mount "bind" from="registry.example/mounted:stable" target="/input"; }
        copy "/source" "/destination" from="registry.example/copied:stable"
    }
}
extend as="unused" { component "registry.example/unselected:stable"; }
`, Options{Mode: Publish, Target: "selected"})
	inspection, err := InspectPublishedDependencies(publication.Component, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertInspectedDependencyInputs(t, inspection, []Input{
		{Kind: "component", Reference: "registry.example/first:stable"},
		{Kind: "component", Reference: "registry.example/second:stable"},
		{Kind: "image", Reference: "registry.example/mounted:stable"},
		{Kind: "image", Reference: "registry.example/copied:stable"},
	})
	if inspection.Incomplete {
		t.Fatal("literal dependency inventory was marked incomplete")
	}
}

func TestPublishedDependencyInspectionPreservesDefaultPrefixAndUncertainty(t *testing.T) {
	publication := makePlan(t, `arg "dependency" "registry.example/child:default"
extend {
    arg "dependency"
    component "${dependency}"
    component "registry.example/later:stable"
    component "registry.example/selected:${missing}stable"
}
`, Options{Mode: Publish})
	inspection, err := InspectPublishedDependencies(publication.Component, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertInspectedDependencyInputs(t, inspection, []Input{
		{Kind: "component", Reference: "registry.example/child:default"},
		{Kind: "component", Reference: "registry.example/later:stable"},
	})
	if !inspection.Incomplete {
		t.Fatal("caller/runtime-dependent selectors were reported as complete")
	}
}

func TestPublishedDependencyInspectionIncludesDormantRequirementsOffline(t *testing.T) {
	publication := makePlan(t, `from "scratch" as="dormant" {
    component "registry.example/dormant:stable"
}
extend {
    from "registry.example/base:stable" {
        component "registry.example/direct:stable"
    }
}
`, Options{Mode: Publish})
	if len(publication.Component.DormantRoots) == 0 {
		t.Fatal("fixture did not retain a possible ONBUILD producer")
	}
	inspection, err := InspectPublishedDependencies(publication.Component, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertInspectedDependencyInputs(t, inspection, []Input{
		{Kind: "component", Reference: "registry.example/dormant:stable"},
		{Kind: "image", Reference: "registry.example/base:stable"},
		{Kind: "component", Reference: "registry.example/direct:stable"},
	})
	if !inspection.Incomplete {
		t.Fatal("unavailable external-image configuration was reported as complete")
	}
}

func TestPublishedDependencyInspectionUsesVerifiedPackageEnvironment(t *testing.T) {
	publication := makePlan(t, `package as="base" {
    env dependency="registry.example/child:from-package"
}
extend {
    from "base" {
        arg "dependency" "registry.example/child:default"
        component "${dependency}"
    }
}
`, Options{Mode: Publish})
	projection, err := ProjectPublished(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	binds := map[string]StageBind{}
	for _, stage := range projection.Stages {
		if stage.Kind == "from" {
			binds[stage.ID] = StageBind{BaseEnvironment: map[string]string{"dependency": "registry.example/child:from-package"}}
		}
	}
	inspection, err := InspectPublishedDependencies(publication.Component, binds)
	if err != nil {
		t.Fatal(err)
	}
	assertInspectedDependencyInputs(t, inspection, []Input{{Kind: "component", Reference: "registry.example/child:from-package"}})
	if !inspection.Incomplete {
		t.Fatal("parameterized selector was reported as unconditional")
	}
	if _, err := InspectPublishedDependencies(publication.Component, nil); err == nil {
		t.Fatal("required verified package bind was omitted")
	}
	for key := range binds {
		invalid := map[string]StageBind{key: {Inherited: []definition.Instruction{{Name: "component"}}}}
		if _, err := InspectPublishedDependencies(publication.Component, invalid); err == nil {
			t.Fatal("invalid inherited package metadata became an incomplete success")
		}
	}
}

func TestPublishedDependencyInspectionRetainsLiteralsWhenDefaultsCannotPlan(t *testing.T) {
	publication := makePlan(t, `extend {
    component "${missing}"
    component "registry.example/child:stable"
}
`, Options{Mode: Publish})
	if _, err := InstantiateDemandDriven(publication.Component, Options{AwaitComponentReturns: true}, nil, unexpectedBindResolver(t)); err == nil {
		t.Fatal("fixture's missing first selector did not prevent default invocation")
	}
	inspection, err := InspectPublishedDependencies(publication.Component, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertInspectedDependencyInputs(t, inspection, []Input{{Kind: "component", Reference: "registry.example/child:stable"}})
	if !inspection.Incomplete {
		t.Fatal("default invocation failure was not marked incomplete")
	}
}

func TestPublishedDependencyInspectionRejectsInvalidMetadata(t *testing.T) {
	publication := makePlan(t, `extend { component "registry.example/child:stable"; }`, Options{Mode: Publish})
	publication.Component.Output = "999"
	if _, err := InspectPublishedDependencies(publication.Component, nil); err == nil {
		t.Fatal("invalid publication metadata became an incomplete success")
	}
}

func TestPublishedDependencyInspectionExcludesLocalInputs(t *testing.T) {
	publication := makePlan(t, `extend {
    component "./shared.coopr#configured"
    component "registry.example/child:stable"
}
`, Options{Mode: Publish})
	inspection, err := InspectPublishedDependencies(publication.Component, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertInspectedDependencyInputs(t, inspection, []Input{{Kind: "component", Reference: "registry.example/child:stable"}})
	if !inspection.Incomplete {
		t.Fatal("unknown local-component content was reported as complete")
	}
}

func TestPublishedDependencyInspectionIncludesPackageTriggers(t *testing.T) {
	publication := makePlan(t, `package as="base" {
    onbuild { copy "/asset" "/asset" from="registry.example/asset:stable"; }
}
extend {
    from "base" {
        component "registry.example/child:stable"
    }
}
`, Options{Mode: Publish})
	projection, err := ProjectPublished(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	binds := map[string]StageBind{}
	for _, stage := range projection.Stages {
		if stage.Kind == "from" {
			binds[stage.ID] = StageBind{Inherited: []definition.Instruction{{
				Name: "copy", Arguments: []string{"/asset", "/asset"},
				Properties: map[string]string{"from": `"registry.example/asset:stable"`}, ProcessQuotes: true,
			}}}
		}
	}
	inspection, err := InspectPublishedDependencies(publication.Component, binds)
	if err != nil {
		t.Fatal(err)
	}
	assertInspectedDependencyInputs(t, inspection, []Input{
		{Kind: "image", Reference: "registry.example/asset:stable"},
		{Kind: "component", Reference: "registry.example/child:stable"},
	})
	for _, bind := range binds {
		if !bind.Inherited[0].ProcessQuotes || bind.Inherited[0].Properties["from"] != `"registry.example/asset:stable"` {
			t.Fatal("inspection mutated the caller's inherited instruction")
		}
	}
}

func TestPublishedDependencyInspectionLeavesRegistryClassificationToConsumer(t *testing.T) {
	publication := makePlan(t, `extend {
    from "oci:output" {
        component "registry.example/child:stable"
    }
}
`, Options{Mode: Publish})
	inspection, err := InspectPublishedDependencies(publication.Component, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertInspectedDependencyInputs(t, inspection, []Input{
		{Kind: "image", Reference: "oci:output"},
		{Kind: "component", Reference: "registry.example/child:stable"},
	})
	if !inspection.Incomplete {
		t.Fatal("unavailable generated image configuration was reported as complete")
	}
}

func assertInspectedDependencyInputs(t *testing.T, inspection DependencyInspection, want []Input) {
	t.Helper()
	seen := make(map[Input]bool, len(inspection.Inputs))
	for _, input := range inspection.Inputs {
		if seen[input] {
			t.Fatalf("duplicate inspected input: %+v", input)
		}
		seen[input] = true
	}
	if len(seen) != len(want) {
		t.Fatalf("inspected inputs = %+v, want %+v", inspection.Inputs, want)
	}
	for _, input := range want {
		if !seen[input] {
			t.Fatalf("missing inspected input %+v in %+v", input, inspection.Inputs)
		}
	}
}

func assertPublishedDependencyInputs(t *testing.T, plan *Plan, want []string) {
	t.Helper()
	var got []string
	for _, input := range plan.Inputs {
		if input.Kind == "component" {
			got = append(got, input.Reference)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("observed component inputs = %v, want %v; all inputs = %+v", got, want, plan.Inputs)
	}
}
