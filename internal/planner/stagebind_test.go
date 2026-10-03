package planner

import (
	"reflect"
	"strings"
	"testing"

	"coopr/internal/definition"
)

func TestStageBindARGAndEnvironmentOrder(t *testing.T) {
	source := `from "alpine" as="final"
run "before"
arg "flavor" "child"
env AFTER="${flavor}-${SEEN}"
run "middle"
env flavor="${AFTER}"
run "last"
`
	bind := StageBind{
		BaseEnvironment: map[string]string{"BASE": "base", "flavor": "base-image"},
		Inherited: []definition.Instruction{
			{Name: "arg", Arguments: []string{"flavor", "inherited"}},
			{Name: "env", Arguments: []string{"SEEN", "${BASE}-${flavor}"}},
			{Name: "env", Arguments: []string{"flavor", "${flavor}-env"}},
		},
	}

	for _, test := range []struct {
		name       string
		arguments  map[string]string
		argValue   string
		seenValue  string
		afterValue string
	}{
		{name: "defaults", argValue: "child", seenValue: "base-inherited", afterValue: "child-base-inherited"},
		{name: "override", arguments: map[string]string{"flavor": "cli"}, argValue: "cli", seenValue: "base-cli", afterValue: "cli-base-cli"},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, err := Create(parse(t, source), Options{
				Mode: Build, Arguments: test.arguments,
				StageBinds: map[string]StageBind{"0": bind},
			})
			if err != nil {
				t.Fatal(err)
			}
			operations := plan.Stages[0].Operations
			if len(operations) != 7 {
				t.Fatalf("operations = %+v", operations)
			}
			if got := operations[0].Arguments[1]; got != test.seenValue {
				t.Fatalf("inherited ENV = %q, want %q", got, test.seenValue)
			}
			if got := operations[1].Arguments[1]; got != strings.TrimPrefix(test.seenValue, "base-")+"-env" {
				t.Fatalf("shadowing ENV = %q", got)
			}
			if _, ok := operations[2].ArgumentsInScope["flavor"]; ok {
				t.Fatalf("base/inherited ENV did not shadow ARG: %v", operations[2].ArgumentsInScope)
			}
			if got := operations[3].Properties["AFTER"]; got != test.afterValue {
				t.Fatalf("authored ENV = %q, want %q", got, test.afterValue)
			}
			if got := operations[4].ArgumentsInScope["flavor"]; got != test.argValue {
				t.Fatalf("redeclared ARG = %q, want %q", got, test.argValue)
			}
			if _, ok := operations[6].ArgumentsInScope["flavor"]; ok {
				t.Fatalf("later ENV did not shadow ARG: %v", operations[6].ArgumentsInScope)
			}
		})
	}
}

func TestStageBindBaseEnvironmentSatisfiesUnsetARG(t *testing.T) {
	plan, err := Create(parse(t, `from "alpine"
env COPIED="${base_value}"
run "check"
`), Options{Mode: Build, StageBinds: map[string]StageBind{"0": {
		BaseEnvironment: map[string]string{"base_value": "from-image"},
		Inherited:       []definition.Instruction{{Name: "arg", Arguments: []string{"base_value"}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Stages[0].Operations[0].Properties["COPIED"]; got != "from-image" {
		t.Fatalf("base environment expansion = %q", got)
	}
	if _, ok := plan.Stages[0].Operations[1].ArgumentsInScope["base_value"]; ok {
		t.Fatal("unset ARG was emitted as a build argument")
	}
}

func TestStageBindMultiValueENVUsesPriorEnvironment(t *testing.T) {
	plan, err := Create(parse(t, `from "alpine"
run "check"
`), Options{Mode: Build, StageBinds: map[string]StageBind{"0": {
		BaseEnvironment: map[string]string{"A": "base"},
		Inherited: []definition.Instruction{{Name: "env", Properties: map[string]string{
			"A": "one", "B": "$A",
		}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Stages[0].Operations[0].Properties["B"]; got != "base" {
		t.Fatalf("second ENV assignment saw earlier assignment: %q", got)
	}
}

func TestStageBindDoesNotExposeUndeclaredAutomaticArguments(t *testing.T) {
	plan, err := Create(parse(t, `from "scratch"
env TARGET="$TARGETARCH"
`), Options{Mode: Build, StageBinds: map[string]StageBind{"0": {}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Stages[0].Operations[0].Properties["TARGET"]; got != "" {
		t.Fatalf("undeclared automatic ARG leaked into stage as %q", got)
	}
}

func TestStageBindRejectsComponentWithoutFrozenMetadata(t *testing.T) {
	_, err := Create(parse(t, `from "scratch" as="builder"
extend
`), Options{Mode: Publish, StageBinds: map[string]StageBind{"0": {}}})
	if err == nil || !strings.Contains(err.Error(), "require frozen publication metadata") {
		t.Fatalf("component stage bind was accepted without publication binding: %v", err)
	}
}

func TestStageBindAddsHiddenStageDependencyBeforeClosure(t *testing.T) {
	plan, err := Create(parse(t, `from "scratch" as="assets"
run "prepare"
from "scratch" as="unused"
run "unused"
from "alpine" as="final"
run "finish"
`), Options{Mode: Build, Target: "final", StageBinds: map[string]StageBind{"2": {
		Inherited: []definition.Instruction{{Name: "copy", Arguments: []string{"/artifact", "/artifact"}, Properties: map[string]string{"from": "assets"}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stageIDs(plan), []string{"0", "2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stage closure = %v, want %v", got, want)
	}
	if got, want := plan.Stages[1].Dependencies, []string{"0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hidden dependencies = %v, want %v", got, want)
	}
	if got := plan.Stages[1].Operations[0].Properties["from"]; got != "assets" {
		t.Fatalf("inherited copy source = %q", got)
	}
}

func TestStageBindRunMountAddsHiddenStageDependency(t *testing.T) {
	plan, err := Create(parse(t, `from "scratch" as="assets"
from "alpine" as="final"
`), Options{Mode: Build, Target: "final", StageBinds: map[string]StageBind{"1": {
		Inherited: []definition.Instruction{{
			Name: "run", Form: "shell", Arguments: []string{"cat /mnt"},
			Properties: map[string]string{"network": "none"},
			Children: []definition.Instruction{{Name: "mount", Arguments: []string{"bind"},
				Properties: map[string]string{"from": "assets", "source": "/artifact", "target": "/mnt"}}},
		}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stageIDs(plan), []string{"0", "1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stage closure = %v, want %v", got, want)
	}
	if got, want := plan.Stages[1].Dependencies, []string{"0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hidden mount dependencies = %v, want %v", got, want)
	}
}

func TestStageBindHiddenDependencyCycleFails(t *testing.T) {
	_, err := Create(parse(t, `from "scratch" as="first"
from "first" as="second"
`), Options{Mode: Build, StageBinds: map[string]StageBind{"0": {
		Inherited: []definition.Instruction{{Name: "copy", Arguments: []string{"/artifact", "/artifact"},
			Properties: map[string]string{"from": "second"}}},
	}}})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("hidden dependency cycle was accepted: %v", err)
	}
}

func TestStageBindValidation(t *testing.T) {
	container := parse(t, `from "alpine"
run "ok"
`)
	component := parse(t, "extend\nrun \"ok\"\n")
	tests := []struct {
		name    string
		def     *definition.Definition
		opts    Options
		message string
	}{
		{name: "noncanonical ID", def: container, opts: Options{Mode: Build, StageBinds: map[string]StageBind{"00": StageBind{}}}, message: "does not identify"},
		{name: "unknown ID", def: container, opts: Options{Mode: Build, StageBinds: map[string]StageBind{"1": StageBind{}}}, message: "does not identify"},
		{name: "non-FROM stage", def: component, opts: Options{Mode: Publish, StageBinds: map[string]StageBind{"0": StageBind{}}}, message: "requires a from stage"},
		{name: "invalid environment", def: container, opts: Options{Mode: Build, StageBinds: map[string]StageBind{"0": StageBind{BaseEnvironment: map[string]string{"BAD=NAME": "x"}}}}, message: "invalid environment name"},
		{name: "structural inherited instruction", def: container, opts: Options{Mode: Build, StageBinds: map[string]StageBind{"0": StageBind{Inherited: []definition.Instruction{{Name: "from", Arguments: []string{"busybox"}}}}}}, message: "cannot be"},
		{name: "malformed inherited instruction", def: container, opts: Options{Mode: Build, StageBinds: map[string]StageBind{"0": StageBind{Inherited: []definition.Instruction{{Name: "env", Arguments: []string{"ONLY_KEY"}}}}}}, message: "expected properties or a key and value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Create(test.def, test.opts)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("want %q, got %v", test.message, err)
			}
		})
	}
}

func TestStageBindInputsAreNotMutatedOrRetained(t *testing.T) {
	bind := StageBind{
		BaseEnvironment: map[string]string{"BASE": "one"},
		Inherited: []definition.Instruction{{
			Name: "copy", Arguments: []string{"/src", "/dst"},
			Properties: map[string]string{"from": "assets"},
			Children:   []definition.Instruction{{Name: "exclude", Arguments: []string{"*.tmp"}}},
		}},
	}
	before := cloneInstruction(bind.Inherited[0])
	plan, err := Create(parse(t, `from "scratch" as="assets"
run "prepare"
from "alpine" as="final"
run "finish"
`), Options{Mode: Build, Target: "final", StageBinds: map[string]StageBind{"1": bind}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bind.Inherited[0], before) || bind.BaseEnvironment["BASE"] != "one" {
		t.Fatalf("planner mutated caller bind: %+v", bind)
	}

	bind.BaseEnvironment["BASE"] = "changed"
	bind.Inherited[0].Arguments[0] = "/changed"
	bind.Inherited[0].Properties["from"] = "changed"
	bind.Inherited[0].Children[0].Arguments[0] = "changed"
	operation := plan.Stages[1].Operations[0]
	if operation.Arguments[0] != "/src" || operation.Properties["from"] != "assets" || operation.Children[0].Arguments[0] != "*.tmp" {
		t.Fatalf("plan retained caller bind storage: %+v", operation)
	}
}

func TestStageLocalARGCanBeRedeclared(t *testing.T) {
	plan, err := Create(parse(t, `from "scratch"
arg "value" "one"
arg "value" "two"
env RESULT="${value}"
`), Options{Mode: Build})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Stages[0].Operations[0].Properties["RESULT"]; got != "two" {
		t.Fatalf("redeclared ARG = %q", got)
	}
}

func TestPackageARGRedeclarationStillRejectsConflictingSnapshot(t *testing.T) {
	_, err := Create(parse(t, `package as="payload"
arg "value" "one"
arg "value" "two"
run "prepare"
extend
copy "/" "/" from="payload"
`), Options{Mode: Publish})
	if err == nil || !strings.Contains(err.Error(), "changes value within a stage") {
		t.Fatalf("conflicting package ARG snapshot was accepted: %v", err)
	}
}

func TestAuthoredENVShadowsARGWithoutStageBind(t *testing.T) {
	plan, err := Create(parse(t, `from "scratch"
arg "value" "argument"
env value="environment"
run "check"
`), Options{Mode: Build})
	if err != nil {
		t.Fatal(err)
	}
	run := plan.Stages[0].Operations[1]
	if _, ok := run.ArgumentsInScope["value"]; ok {
		t.Fatalf("authored ENV did not shadow ARG: %v", run.ArgumentsInScope)
	}
}

func TestValuelessARGRedeclarationDoesNotClearENVShadow(t *testing.T) {
	plan, err := Create(parse(t, `from "scratch"
arg "value" "argument"
env value="environment"
arg "value"
run "check"
`), Options{Mode: Build})
	if err != nil {
		t.Fatal(err)
	}
	run := plan.Stages[0].Operations[1]
	if _, ok := run.ArgumentsInScope["value"]; ok {
		t.Fatalf("valueless ARG cleared ENV precedence: %v", run.ArgumentsInScope)
	}
}

func TestLocalFROMInheritsEnvironmentAndShadowOrder(t *testing.T) {
	plan, err := Create(parse(t, `from "scratch" as="base"
arg "flavor" "argument"
env inherited="${flavor}"
env flavor="environment"
from "base" as="child"
env child="${inherited}-${flavor}"
copy "${child}" "/result"
run "check"
`), Options{Mode: Build})
	if err != nil {
		t.Fatal(err)
	}
	child := plan.Stages[1]
	if got := child.Operations[0].Properties["child"]; got != "argument-environment" {
		t.Fatalf("inherited ENV expansion = %q", got)
	}
	if got := child.Operations[1].Arguments[0]; got != "argument-environment" {
		t.Fatalf("COPY did not see inherited ENV: %q", got)
	}
	if _, ok := child.Operations[2].ArgumentsInScope["flavor"]; ok {
		t.Fatalf("local FROM lost inherited ENV shadow: %v", child.Operations[2].ArgumentsInScope)
	}
}
