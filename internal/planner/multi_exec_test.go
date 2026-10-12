package planner

import (
	"reflect"
	"testing"

	"coopr/internal/definition"
)

func TestCloneAndNormalizeRunExecSequence(t *testing.T) {
	original := definition.Instruction{Name: "run", Commands: []definition.Command{{Form: "shell", Arguments: []string{"echo $first"}}, {Form: "exec", Arguments: []string{"second", "${second}"}}}, Properties: map[string]string{"network": "${network}"}}
	cloned := cloneInstruction(original)
	cloned.Commands[0].Arguments[0] = "changed"
	cloned.Commands[1] = definition.Command{Form: "shell", Arguments: []string{"replacement"}}
	if original.Commands[0].Arguments[0] != "echo $first" || original.Commands[1].Arguments[0] != "second" {
		t.Fatalf("clone aliased original: %#v", original)
	}
	normalized, err := normalize(original, scope{"first": {value: "one", present: true}, "second": {value: "two", present: true}, "network": {value: "none", present: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalized.Commands, original.Commands) || normalized.Properties["network"] != "none" {
		t.Fatalf("normalized = %#v", normalized)
	}
	normalized.Commands[1].Arguments[1] = "changed"
	if original.Commands[1].Arguments[1] != "${second}" {
		t.Fatal("normalization aliased original commands")
	}
}

func TestRunExecSequenceCollectsEveryArgumentReference(t *testing.T) {
	inst := definition.Instruction{Name: "run", Commands: []definition.Command{{Form: "shell", Arguments: []string{"echo $first"}}, {Form: "exec", Arguments: []string{"second", "${second}", "$undeclared"}}}}
	used := map[string]bool{}
	new(graph).collectArgumentReferences(inst, scope{"first": {value: "one", present: true}, "second": {value: "two", present: true}, "unused": {value: "three", present: true}}, used)
	if !reflect.DeepEqual(used, map[string]bool{"first": true, "second": true}) {
		t.Fatalf("references = %#v", used)
	}
}

func TestComponentExecSequenceSurvivesPublicationAndInvocation(t *testing.T) {
	publication := makePlan(t, `arg "message" "hello"
package as="assets" {
 arg "message"
 run {
  script "echo prepared"
  exec "/write" "$message"
 }
}
extend {
 arg "suffix" "world"
 run {
  script "/install"
  exec "/configure" "$suffix"
 }
 copy "/message" "/message" from="assets"
}
`, Options{Mode: Publish})
	if publication.PackageArguments["message"] != "hello" {
		t.Fatalf("later command argument not frozen: %#v", publication.PackageArguments)
	}
	invocation := invokePublished(t, publication, Options{Arguments: map[string]string{"suffix": "caller"}})
	var got []definition.Command
	for _, stage := range invocation.Stages {
		for _, op := range stage.Operations {
			if op.Name == "run" {
				got = op.Commands
			}
		}
	}
	want := []definition.Command{{Form: "shell", Arguments: []string{"/install"}}, {Form: "exec", Arguments: []string{"/configure", "$suffix"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("invoked commands = %#v, want %#v", got, want)
	}
	if _, err := Instantiate(publication.Component, Options{Arguments: map[string]string{"message": "changed"}}); err == nil {
		t.Fatal("overrode package input referenced only by later exec")
	}
}

func TestScriptOnBuildInheritedAsShellCommand(t *testing.T) {
	plan, err := CreateDemandDriven(parse(t, `from "scratch" as="base" {
 onbuild { run { script "echo $message" } }
}

from "base" {
 arg "message" "child"
}`), Options{}, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	child := plan.Stages[len(plan.Stages)-1]
	if !child.InheritedOnBuildPlanned || child.Operations[0].Name != "run" || child.Operations[0].Form != "shell" || !reflect.DeepEqual(child.Operations[0].Arguments, []string{"echo $message"}) {
		t.Fatalf("inherited child = %#v", child)
	}
}

func TestScriptOnBuildShebangRetainsShellForm(t *testing.T) {
	plan, err := CreateDemandDriven(parse(t, `from "scratch" as="base" {
 shell "/busybox" "sh" "-ec"
 onbuild {
  run {
   script """
    #!/unavailable/interpreter
    printf inherited > /proof
    """
  }
 }
}
from "base" {}`), Options{}, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	child := plan.Stages[len(plan.Stages)-1]
	if !child.InheritedOnBuildPlanned || len(child.Operations) != 1 {
		t.Fatalf("inherited child = %#v", child)
	}
	got := child.Operations[0]
	if got.Name != "run" || got.Form != "shell" || len(got.InlineFiles) != 0 || !reflect.DeepEqual(got.Arguments, []string{"\n#!/unavailable/interpreter\nprintf inherited > /proof\n"}) {
		t.Fatalf("inherited script = %#v", got)
	}
}
