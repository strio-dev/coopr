package planner

import (
	"reflect"
	"testing"

	"coopr/internal/definition"
)

func TestComponentArgumentBindingsExpandCallerValuesWithoutChangingScope(t *testing.T) {
	plan := makePlan(t, `from "scratch" {
    arg "channel" "caller"
    arg "name" "renamed"
    env environment="from-env"
    component "./shared.coopr#configured" {
     arg "channel" "${channel}"
     arg "from_env" "${environment}"
     arg "target" "ordinary"
    }
    copy "${channel}" "/caller"
}
`, Options{})
	operations := plan.Stages[0].Operations
	args, err := operations[1].ComponentArguments()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"channel": "caller", "from_env": "from-env", "target": "ordinary"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("bindings = %v, want %v", args, want)
	}
	if operations[2].Arguments[0] != "caller" {
		t.Fatalf("binding leaked into caller: %+v", operations[2])
	}
	if _, leaked := operations[2].ArgumentsInScope["from_env"]; leaked {
		t.Fatal("component child ARG entered caller scope")
	}
}

func TestComponentArgumentValuesFixPackageInputs(t *testing.T) {
	publication := makePlan(t, `arg "channel" "stable"
package as="assets" {
    component "./prepare.coopr" { arg "channel" "${channel}" }
}
extend {
    copy "/asset" "/asset" from="assets"
}
`, Options{Mode: Publish})
	if publication.PackageArguments["channel"] != "stable" {
		t.Fatalf("nested argument input not frozen: %v", publication.PackageArguments)
	}
	if _, err := Instantiate(publication.Component, Options{Arguments: map[string]string{"channel": "changed"}}); err == nil {
		t.Fatal("fixed nested argument input overridden")
	}
	invocation := invokePublished(t, publication, Options{})
	if invocation.PackageArguments["channel"] != "stable" {
		t.Fatalf("nested argument input lost: %v", invocation.PackageArguments)
	}
}

func TestNormalizeComponentArgumentNamesRemainLiteral(t *testing.T) {
	inst := definition.Instruction{Name: "component", Arguments: []string{"./shared.coopr"}, Children: []definition.Instruction{{Name: "arg", Arguments: []string{"${name}", "${value}"}}}}
	normalized, err := normalize(inst, scope{"name": {value: "changed", present: true}, "value": {value: "expanded", present: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalized.Children[0].Arguments, []string{"${name}", "expanded"}) {
		t.Fatalf("binding name expanded: %+v", normalized)
	}
}

func TestPublishedComponentRejectsMalformedArgumentBindings(t *testing.T) {
	for _, bindings := range [][]definition.Instruction{
		{{Name: "arg", Arguments: []string{"channel", "one"}}, {Name: "arg", Arguments: []string{"channel", "two"}}},
		{{Name: "arg", Arguments: []string{"channel"}}},
		{{Name: "env", Arguments: []string{"channel", "one"}}},
	} {
		publication := makePlan(t, `extend {
    component "./shared.coopr"
}
`, Options{Mode: Publish})
		publication.Component.Definition.Instructions[0].Children[0].Children = bindings
		if _, err := Instantiate(publication.Component, Options{}); err == nil {
			t.Fatalf("accepted malformed decoded bindings: %+v", bindings)
		}
	}
}

func TestComponentBindingNamesDoNotFixCallerArguments(t *testing.T) {
	publication := makePlan(t, `arg "channel" "caller"
package as="assets" {
    component "./prepare.coopr" { arg "channel" "fixed-child-value" }
}
extend {
    copy "/asset" "/asset" from="assets"
}
`, Options{Mode: Publish})
	if _, fixed := publication.PackageArguments["channel"]; fixed {
		t.Fatalf("literal child argument name froze caller value: %v", publication.PackageArguments)
	}
	if _, err := Instantiate(publication.Component, Options{Arguments: map[string]string{"channel": "changed"}}); err != nil {
		t.Fatalf("unreferenced caller argument became fixed: %v", err)
	}
}
