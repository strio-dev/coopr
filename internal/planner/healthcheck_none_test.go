package planner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/onbuildparse"
)

func TestDisabledHealthcheckSkipsIgnoredExpansion(t *testing.T) {
	def := parse(t, `from "scratch" {
    healthcheck NONE interval="${MISSING:?ignored}" retries="${MISSING:?ignored}"
}
`)
	before, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Create(def, Options{Mode: Build})
	if err != nil {
		t.Fatalf("ignored NONE options were expanded: %v", err)
	}
	operation := plan.Stages[0].Operations[0]
	if len(operation.Properties) != 0 || len(operation.ArgumentsInScope) != 0 || len(plan.Arguments) != 0 || len(plan.Inputs) != 0 {
		t.Fatalf("ignored NONE options entered operation bindings: %+v; arguments=%+v inputs=%+v", operation, plan.Arguments, plan.Inputs)
	}
	after, err := json.Marshal(def)
	if err != nil || string(before) != string(after) {
		t.Fatalf("normalization changed the authored definition: %s, %v", after, err)
	}
}

func TestDisabledHealthcheckDoesNotFreezeIgnoredPackageArguments(t *testing.T) {
	publication, err := Create(parse(t, `arg "ignored" "first"
package as="assets" {
    arg "ignored"
    healthcheck NONE interval="$ignored"
}
extend {
    copy "/payload" "/payload" from="assets"
    healthcheck NONE interval="${MISSING:?ignored}"
}
`), Options{Mode: Publish})
	if err != nil {
		t.Fatalf("publication expanded ignored NONE options: %v", err)
	}
	if len(publication.PackageArguments) != 0 || len(publication.Component.PackageArguments) != 0 {
		t.Fatalf("ignored option froze publication arguments: %+v / %+v", publication.PackageArguments, publication.Component.PackageArguments)
	}
	for _, stage := range publication.Stages {
		for _, operation := range stage.Operations {
			if operation.Name == "healthcheck" && len(operation.Properties) != 0 {
				t.Fatalf("package cache operation retained ignored properties: %+v", operation)
			}
		}
	}
	raw, err := json.Marshal(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	var component PublishedComponent
	if err := json.Unmarshal(raw, &component); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidatePublished(&component); err != nil {
		t.Fatal(err)
	}
	invocation, err := Instantiate(&component, Options{Arguments: map[string]string{"ignored": "second"}})
	if err != nil {
		t.Fatalf("ignored package argument became immutable: %v", err)
	}
	for _, stage := range invocation.Stages {
		for _, operation := range stage.Operations {
			if operation.Name == "healthcheck" && len(operation.Properties) != 0 {
				t.Fatalf("invocation retained ignored NONE options: %+v", operation)
			}
		}
	}
}

func TestDisabledHealthcheckOnBuildIgnoresOptions(t *testing.T) {
	plan, err := Create(parse(t, `from "scratch" as="parent" {
    onbuild { healthcheck NONE interval="${MISSING:?ignored}" }
}
from "parent" {}
`), Options{Mode: Build})
	if err != nil {
		t.Fatal(err)
	}
	child := plan.Stages[len(plan.Stages)-1].Operations[0]
	if child.Name != "healthcheck" || len(child.Properties) != 0 || len(child.ArgumentsInScope) != 0 {
		t.Fatalf("authored ONBUILD retained ignored options: %+v", child)
	}
	inherited, err := onbuildparse.ParseDeferred("HEALTHCHECK --interval=${MISSING:?ignored} --retries=${MISSING:?ignored} NONE")
	if err != nil {
		t.Fatal(err)
	}
	plan, err = Create(parse(t, `from "example.invalid/base" {}`), Options{Mode: Build, StageBinds: map[string]StageBind{"0": {Inherited: inherited}}})
	if err != nil {
		t.Fatalf("inherited ONBUILD expanded ignored flags: %v", err)
	}
	child = plan.Stages[0].Operations[0]
	if child.Name != "healthcheck" || len(child.Properties) != 0 || len(child.ArgumentsInScope) != 0 {
		t.Fatalf("inherited ONBUILD retained ignored flags: %+v", child)
	}
}

func TestDisabledHealthcheckUnknownPropertyStillFails(t *testing.T) {
	if _, err := definition.Parse(strings.NewReader(`from "scratch" { healthcheck NONE bogus="${MISSING:?ignored}" }`)); err == nil || !strings.Contains(err.Error(), `unsupported property "bogus"`) {
		t.Fatalf("authored unknown property accepted: %v", err)
	}
	def := parse(t, `from "scratch" { healthcheck NONE }`)
	def.Instructions[0].Children[0].Properties = map[string]string{"bogus": "${MISSING:?ignored}"}
	if _, err := Create(def, Options{Mode: Build}); err == nil || !strings.Contains(err.Error(), `unsupported property "bogus"`) {
		t.Fatalf("decoded unknown property accepted: %v", err)
	}
}
