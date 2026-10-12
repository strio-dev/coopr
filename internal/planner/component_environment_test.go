package planner

import (
	"reflect"
	"strings"
	"testing"
)

func TestInvocationCallerEnvironmentOnlyBindsExtend(t *testing.T) {
	publication := makePlan(t, `arg "value" "global"
from "scratch" as="independent" {
    env captured="${value}" caller="${caller}"
}
extend {
    copy "/proof" "/proof" from="independent"
    env captured="${value}" caller="${caller}"
    arg "default" "${value}"
    env selected="${default}"
}
`, Options{Mode: Publish})
	environment := map[string]string{"value": "image", "caller": "current"}
	plan := invokePublished(t, publication, Options{CallerEnvironment: environment})
	environment["value"] = "mutated"
	independent, extend := plan.Stages[0], plan.Stages[1]
	if got := independent.Operations[0].Properties; got["captured"] != "" || got["caller"] != "" {
		t.Fatalf("independent FROM inherited caller ENV: %v", got)
	}
	if got := extend.Operations[1].Properties; got["captured"] != "image" || got["caller"] != "current" {
		t.Fatalf("EXTEND failed to inherit caller ENV: %v", got)
	}
	if got := extend.Operations[2].Properties["selected"]; got != "image" {
		t.Fatalf("ARG default did not use caller ENV: %q", got)
	}
}

func TestCallerEnvironmentRejectsInvalidEntriesAndNonInvocation(t *testing.T) {
	for _, environment := range []map[string]string{{"": "value"}, {"bad=name": "value"}, {"valid": "nul\x00value"}} {
		publication := makePlan(t, `extend { env proof="yes"; }`, Options{Mode: Publish})
		if _, err := Instantiate(publication.Component, Options{CallerEnvironment: environment}); err == nil || !strings.Contains(err.Error(), "caller environment") {
			t.Fatalf("caller environment %v: %v", environment, err)
		}
	}
	if _, err := Create(parse(t, `from "scratch" { env proof="yes"; }`), Options{CallerEnvironment: map[string]string{"value": "unexpected"}}); err == nil {
		t.Fatal("standalone build accepted caller environment")
	}
}

func TestComponentReturnReplansArgumentsAndNewDependencies(t *testing.T) {
	def := parse(t, `from "scratch" as="helper" { env proof="helper"; }
from "scratch" {
    arg "value" "caller"
    env removed="old"
    layer {
        component "./shared.coopr"
        arg "default" "${value}"
        copy "/source" "${destination}" from="${source}"
        run "echo $value" { mount "cache" target="${cache}"; }
        env captured="${value}" selected="${default}" removed_after="${removed}"
        component "./second.coopr" { arg "value" "${value}"; }
        env after="${value}"
    }
}
`)
	plan, err := Create(def, Options{Mode: Build, AwaitComponentReturns: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Stages) != 1 || plan.Stages[0].AwaitingComponent == nil {
		t.Fatalf("initial plan = %+v", plan)
	}
	before := plan.Stages[0]
	if got := before.AwaitingComponent.Instruction; got != 3 {
		t.Fatalf("authored instruction identity = %d", got)
	}
	returned := map[string]string{"value": "returned", "source": "helper", "destination": "/copied", "cache": "/cache"}
	next, err := plan.ReplanComponentReturn(*before.AwaitingComponent, returned)
	if err != nil {
		t.Fatal(err)
	}
	returned["value"] = "mutated"
	if len(next.Stages) != 2 {
		t.Fatalf("new dependency was not activated: %+v", next.Stages)
	}
	stage := next.Stages[1]
	if len(stage.Dependencies) != 1 || stage.Dependencies[0] != "0" {
		t.Fatalf("dependencies = %v", stage.Dependencies)
	}
	for i, old := range before.Operations {
		if !reflect.DeepEqual(old, stage.Operations[i]) {
			t.Fatalf("replay changed executed operation %d", i)
		}
	}
	copied := stage.Operations[3]
	if copied.Name != "copy" || copied.Arguments[1] != "/copied" || copied.Properties["from"] != "helper" {
		t.Fatalf("COPY = %+v", copied)
	}
	run := stage.Operations[4]
	if _, stale := run.ArgumentsInScope["value"]; stale {
		t.Fatalf("returned ENV failed to shadow caller ARG: %+v", run)
	}
	if run.Children[0].Properties["target"] != "/cache" {
		t.Fatalf("RUN mount = %+v", run.Children)
	}
	environment := stage.Operations[5].Properties
	if environment["captured"] != "returned" || environment["selected"] != "returned" || environment["removed_after"] != "" {
		t.Fatalf("returned/default/replacement ENV = %v", environment)
	}
	second := stage.Operations[6]
	bindings, err := second.ComponentArguments()
	if err != nil || bindings["value"] != "returned" {
		t.Fatalf("next component arguments=%v err=%v", bindings, err)
	}
	final, err := next.ReplanComponentReturn(*stage.AwaitingComponent, nil)
	if err != nil {
		t.Fatal(err)
	}
	output := final.Stages[len(final.Stages)-1]
	if output.AwaitingComponent != nil {
		t.Fatalf("final still awaiting return: %+v", output)
	}
	if got := output.Operations[7].Properties["after"]; got != "caller" {
		t.Fatalf("cleared returned ENV should reveal retained caller ARG: %q", got)
	}
	if output.Operations[len(output.Operations)-1].LayerBoundary != "end" {
		t.Fatal("replay lost open outer layer end")
	}
	if _, err := final.ReplanComponentReturn(*before.AwaitingComponent, nil); err == nil {
		t.Fatal("accepted return for already completed boundary")
	}
}

func TestEnvironmentAlwaysShadowsRedeclaredArguments(t *testing.T) {
	for _, declaration := range []string{`arg "value"`, `arg "value" "default"`} {
		for _, supplied := range []map[string]string{nil, {"value": "cli"}} {
			plan := makePlan(t, `from "scratch" {
    env value="environment"
    `+declaration+`
    env result="${value}"
    run "echo $value"
}
`, Options{Arguments: supplied})
			if got := plan.Stages[0].Operations[1].Properties["result"]; got != "environment" {
				t.Fatalf("%s args=%v result=%q", declaration, supplied, got)
			}
			if _, exists := plan.Stages[0].Operations[2].ArgumentsInScope["value"]; exists {
				t.Fatalf("%s args=%v RUN includes shadowed ARG", declaration, supplied)
			}
		}
	}
}

func TestComponentReturnPreservesSourceOrderAndPublicationBindings(t *testing.T) {
	t.Run("forward stage remains invalid", func(t *testing.T) {
		plan, err := Create(parse(t, `from "scratch" as="first" {
    component "./shared.coopr"
    copy "/proof" "/proof" from="${source}"
}
from "scratch" as="later" { env proof="later"; }
`), Options{Mode: Build, Target: "first", AwaitComponentReturns: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := plan.ReplanComponentReturn(*plan.Stages[0].AwaitingComponent, map[string]string{"source": "later"}); err == nil || !strings.Contains(err.Error(), "earlier") {
			t.Fatalf("forward source accepted: %v", err)
		}
	})
	t.Run("published numeric identity survives replay", func(t *testing.T) {
		publication := makePlan(t, `from "scratch" as="unused" { env proof="unused"; }
from "scratch" as="helper" { env proof="helper"; }
extend {
    component "nested"
    copy "/proof" "/proof" from="1"
    env captured="${value}"
}
`, Options{Mode: Publish})
		invocation := invokePublished(t, publication, Options{AwaitComponentReturns: true})
		output := invocation.Stages[len(invocation.Stages)-1]
		next, err := invocation.ReplanComponentReturn(*output.AwaitingComponent, map[string]string{"value": "returned"})
		if err != nil {
			t.Fatal(err)
		}
		output = next.Stages[len(next.Stages)-1]
		if len(output.Dependencies) != 1 || output.Dependencies[0] != "0" {
			t.Fatalf("replayed published dependency=%v", output.Dependencies)
		}
		if output.Operations[1].Properties["from"] != "0" {
			t.Fatalf("numeric source was not rebound: %+v", output.Operations[1])
		}
		if output.Operations[2].Properties["captured"] != "returned" {
			t.Fatalf("published suffix did not consume returned ENV: %+v", output.Operations[2])
		}
	})
}

func TestComponentExecutionRecipeRetainsAuthoredSuffix(t *testing.T) {
	original := makePlan(t, `arg "value" "caller"
from "scratch" {
    component "./shared.coopr"
    arg "result" "${value}"
    env result="${result}"
}
`, Options{Mode: Build, AwaitComponentReturns: true})
	source, options, available := original.ExecutionRecipe()
	if !available {
		t.Fatal("missing authored execution recipe")
	}
	// Altering a request's source/options must not mutate the planner's replay.
	source.Instructions[0].Arguments[1] = "changed"
	options.Arguments = map[string]string{"value": "changed"}
	freshSource, freshOptions, _ := original.ExecutionRecipe()
	if freshSource.Instructions[0].Arguments[1] != "caller" || len(freshOptions.Arguments) != 0 {
		t.Fatal("execution recipe aliases planner inputs")
	}
	rebuilt, err := Create(freshSource, freshOptions)
	if err != nil {
		t.Fatal(err)
	}
	boundary := rebuilt.Stages[0].AwaitingComponent
	if boundary == nil {
		t.Fatal("worker recipe lost pending component boundary")
	}
	continued, err := rebuilt.ReplanComponentReturn(*boundary, map[string]string{"value": "returned"})
	if err != nil {
		t.Fatal(err)
	}
	operations := continued.Stages[0].Operations
	if got := operations[len(operations)-1].Properties["result"]; got != "returned" {
		t.Fatalf("recipe rebound suffix to %q", got)
	}
	freshSource, freshOptions, _ = continued.ExecutionRecipe()
	if len(freshOptions.ComponentReturns) != 0 {
		t.Fatal("fresh execution recipe retained execution-produced return bindings")
	}
	restarted, err := Create(freshSource, freshOptions)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Stages[0].AwaitingComponent == nil {
		t.Fatal("fresh recipe skipped actual component execution")
	}
}

func TestComponentExecutionRecipeDropsDiscoveredDynamicBindings(t *testing.T) {
	original, err := CreateDemandDriven(parse(t, `from "scratch" as="base" { component "./shared.coopr"; }
from "base" { env captured="${value}"; }
`), Options{Mode: Build, AwaitComponentReturns: true}, func(FromSource) (StageBind, error) { return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	returned, err := original.ReplanComponentReturn(*original.Stages[0].AwaitingComponent, map[string]string{"value": "returned"})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := returned.ReplanDynamicStage("1", StageBind{BaseEnvironment: map[string]string{"value": "dynamic"}})
	if err != nil {
		t.Fatal(err)
	}
	source, options, available := bound.ExecutionRecipe()
	if !available || len(options.StageBinds) != 0 || len(options.ComponentReturns) != 0 {
		t.Fatalf("fresh recipe retained execution state: %+v", options)
	}
	restarted, err := Create(source, options)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Stages[0].AwaitingComponent == nil {
		t.Fatal("fresh execution lost component boundary")
	}
}
