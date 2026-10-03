package planner

import (
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/onbuildparse"
)

func TestAuthoredAndInheritedOnBuildWordExpansionAreEquivalent(t *testing.T) {
	values := []struct {
		name  string
		value string
	}{
		{name: "literal single quotes around variable", value: `'$later'`},
		{name: "literal double quotes around variable", value: `"$later"`},
		{name: "literal quotes", value: `'literal' "literal"`},
		{name: "escaped dollar", value: `\$later`},
		{name: "escaped ordinary character", value: `\q`},
		{name: "escaped backslash", value: `\\path`},
		{name: "default expression", value: `${later:-fallback}`},
	}
	types := []struct {
		name        string
		instruction func(string) string
		observe     string
	}{
		{
			name: "env",
			instruction: func(value string) string {
				return "env VALUE=" + strconv.Quote(value)
			},
		},
		{
			name: "arg",
			instruction: func(value string) string {
				return `arg "VALUE" ` + strconv.Quote(value)
			},
			observe: `env RESULT="$VALUE"`,
		},
		{
			name: "workdir",
			instruction: func(value string) string {
				return "workdir " + strconv.Quote(value)
			},
		},
		{
			name: "copy source and destination",
			instruction: func(value string) string {
				return "copy " + strconv.Quote(value) + " " + strconv.Quote(value)
			},
		},
		{
			name: "add source and destination",
			instruction: func(value string) string {
				return "add " + strconv.Quote(value) + " " + strconv.Quote(value)
			},
		},
		{
			name: "volume",
			instruction: func(value string) string {
				return "volume " + strconv.Quote(value)
			},
		},
	}

	for _, instructionType := range types {
		for _, value := range values {
			t.Run(instructionType.name+"/"+value.name, func(t *testing.T) {
				instruction := instructionType.instruction(value.value)
				direct := makePlan(t, fmt.Sprintf("from \"scratch\"\nenv later=\"child\"\n%s\n%s\n", instruction, instructionType.observe), Options{Mode: Build})
				inheritedDefinition := parse(t, fmt.Sprintf("from \"scratch\" as=\"base\"\nenv later=\"child\"\nonbuild { %s }\nfrom \"base\"\n%s\n", instruction, instructionType.observe))
				inherited, err := CreateDemandDriven(inheritedDefinition, Options{Mode: Build}, unexpectedBindResolver(t))
				if err != nil {
					t.Fatal(err)
				}

				directOperations := direct.Stages[len(direct.Stages)-1].Operations
				directOperation := directOperations[len(directOperations)-1].Instruction
				inheritedOperation := inherited.Stages[len(inherited.Stages)-1].Operations[0].Instruction
				if !reflect.DeepEqual(canonicalDeferredTestInstruction(inheritedOperation), canonicalDeferredTestInstruction(directOperation)) {
					t.Fatalf("inherited operation = %#v, want direct operation %#v", inheritedOperation, directOperation)
				}
				if inheritedOperation.ProcessQuotes {
					t.Fatalf("inherited operation retained deferred quote marker: %#v", inheritedOperation)
				}
			})
		}
	}
}

func canonicalDeferredTestInstruction(inst definition.Instruction) definition.Instruction {
	if inst.Name == "env" && len(inst.Arguments) == 2 {
		inst.Properties = map[string]string{inst.Arguments[0]: inst.Arguments[1]}
		inst.Arguments = nil
	}
	return inst
}

func TestDeferredDockerOnBuildWordsExpandOnceInChildScope(t *testing.T) {
	triggers := []string{
		`ARG quoted="hello $later"`,
		`ARG literal='${later}'`,
		`ARG escaped=\$later`,
		`ARG selected=${later:-fallback}`,
		`ARG empty=""`,
		`ENV SINGLE='$later'`,
		`ENV ESCAPED=\$later`,
		`ENV ESCAPED_DOUBLE="\$later"`,
		`ENV DOUBLE="$later"`,
		`ENV FALLBACK=${missing:-fallback}`,
		`ENV ARG_VALUES="$quoted|$literal|$escaped|$selected|x${empty}x"`,
		"COPY <<EOF /unquoted\n\\$later\n$later\nEOF",
		"COPY <<'EOF' /quoted\n\\$later\n$later\nEOF",
	}

	var inherited []definition.Instruction
	for _, trigger := range triggers {
		parsed, err := onbuildparse.ParseDeferred(trigger)
		if err != nil {
			t.Fatalf("parse %q: %v", trigger, err)
		}
		inherited = append(inherited, parsed...)
	}

	plan := makePlan(t, `from "registry.example/parent:latest"`, Options{
		Mode: Build,
		StageBinds: map[string]StageBind{
			"0": {
				Inherited:       inherited,
				BaseEnvironment: map[string]string{"later": "child"},
			},
		},
	})
	operations := plan.Stages[0].Operations
	if len(operations) != 8 {
		t.Fatalf("operations = %#v, want six ENV and two COPY operations", operations)
	}

	wantEnvironment := [][2]string{
		{"SINGLE", "$later"},
		{"ESCAPED", "$later"},
		{"ESCAPED_DOUBLE", "$later"},
		{"DOUBLE", "child"},
		{"FALLBACK", "fallback"},
		{"ARG_VALUES", "hello child|${later}|$later|child|xx"},
	}
	for i, want := range wantEnvironment {
		got := operations[i].Instruction
		if got.ProcessQuotes || !reflect.DeepEqual(got.Arguments, []string{want[0], want[1]}) {
			t.Fatalf("ENV operation %d = %#v, want %q=%q without deferred marker", i+1, got, want[0], want[1])
		}
	}

	if got, want := operations[6].InlineFiles, []definition.InlineFile{{Path: "EOF", Data: "$later\nchild\n"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unquoted heredoc = %#v, want %#v", got, want)
	}
	if got, want := operations[7].InlineFiles, []definition.InlineFile{{Path: "EOF", Data: "\\$later\n$later\n"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("quoted heredoc = %#v, want %#v", got, want)
	}
	for i, operation := range operations {
		if operation.ProcessQuotes {
			t.Fatalf("operation %d retained deferred quote marker: %#v", i+1, operation.Instruction)
		}
	}
}

func TestDeferredDockerOnBuildReferenceCollectionRespectsQuoting(t *testing.T) {
	values := scope{"later": {value: "child", present: true}}
	for _, test := range []struct {
		name    string
		trigger string
		want    bool
	}{
		{name: "single quoted", trigger: `ENV VALUE='$later'`},
		{name: "escaped", trigger: `ENV VALUE=\$later`},
		{name: "escaped double quoted", trigger: `ENV VALUE="\$later"`},
		{name: "double quoted", trigger: `ENV VALUE="$later"`, want: true},
		{name: "fallback", trigger: `ENV VALUE=${later:-fallback}`, want: true},
		{name: "unquoted heredoc escaped", trigger: "COPY <<EOF /value\n\\$later\nEOF"},
		{name: "unquoted heredoc expanded", trigger: "COPY <<EOF /value\n$later\nEOF", want: true},
		{name: "quoted heredoc", trigger: "COPY <<'EOF' /value\n$later\nEOF"},
	} {
		t.Run(test.name, func(t *testing.T) {
			instructions, err := onbuildparse.ParseDeferred(test.trigger)
			if err != nil {
				t.Fatal(err)
			}
			used := map[string]bool{}
			new(graph).collectArgumentReferences(instructions[0], values, used)
			if got := used["later"]; got != test.want {
				t.Fatalf("reference collected = %t, want %t for %#v", got, test.want, instructions[0])
			}
		})
	}
}
