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

func TestDeferredOnBuildRunKeepsTransientMounts(t *testing.T) {
	for _, trigger := range []string{
		`RUN true`,
		`RUN --mount=type=cache,id=authored,from=assets,target=${target}/authored true`,
	} {
		t.Run(trigger, func(t *testing.T) {
			inherited, err := onbuildparse.ParseDeferred(trigger)
			if err != nil {
				t.Fatal(err)
			}
			transient := []definition.Instruction{
				{Name: "mount", Arguments: []string{"bind"}, Properties: map[string]string{"from": "${source}", "source": "/", "target": "${target}/bind"}},
				{Name: "mount", Arguments: []string{"tmpfs"}, Properties: map[string]string{"target": "${target}/tmpfs"}},
			}
			beforeInherited := fmt.Sprintf("%#v", inherited)
			beforeTransient := fmt.Sprintf("%#v", transient)
			opts := Options{
				Mode: Build, TransientRunMounts: transient,
				StageBinds: map[string]StageBind{"1": {Inherited: inherited}},
			}
			source := `arg "source" "assets"
from "scratch" as="assets"
from "example.invalid/base:latest"
arg "target" "/input"
run "true"
`
			// Inherited triggers run before authored ARG declarations, so use the
			// base environment for their ordinary mount-property expansion.
			bind := opts.StageBinds["1"]
			bind.BaseEnvironment = map[string]string{"target": "/input"}
			opts.StageBinds["1"] = bind
			for range 2 {
				plan := makePlan(t, source, opts)
				stage := plan.Stages[len(plan.Stages)-1]
				for index, op := range stage.Operations {
					authoredCount := 0
					if index == 0 && trigger != "RUN true" {
						authoredCount = 1
					}
					if len(op.Children) != authoredCount+2 || op.TransientMountCount != 2 {
						t.Fatalf("operation %d mounts = %#v, transient count = %d", index, op.Children, op.TransientMountCount)
					}
					if authoredCount != 0 && op.Children[0].Properties["target"] != "/input/authored" {
						t.Fatalf("authored mount was changed or misordered: %#v", op.Children)
					}
					bindMount := op.Children[authoredCount]
					if bindMount.Properties["from"] != "assets" || bindMount.Properties["target"] != "/input/bind" || op.Children[authoredCount+1].Properties["target"] != "/input/tmpfs" {
						t.Fatalf("transient mount expansion/order = %#v", op.Children)
					}
				}
				if !reflect.DeepEqual(stage.Dependencies, []string{"0"}) {
					t.Fatalf("transient source dependency = %v", stage.Dependencies)
				}
			}
			if fmt.Sprintf("%#v", inherited) != beforeInherited || fmt.Sprintf("%#v", transient) != beforeTransient {
				t.Fatal("planning mutated inherited triggers or transient mount options")
			}
		})
	}
}

func TestDeferredOnBuildTransientMountsInPackageBuild(t *testing.T) {
	inherited, err := onbuildparse.ParseDeferred(`RUN --mount=type=bind,from=assets,target=/authored true`)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := CreateDemandDriven(parse(t, `from "scratch" as="assets"
from "example.invalid/base:latest" as="producer"
arg "destination" "/transient"
run "true"
package as="payload"
copy "/output" "/output" from="producer"
extend
copy "/output" "/output" from="payload"
`), Options{
		Mode:               Publish,
		TransientRunMounts: []definition.Instruction{{Name: "mount", Arguments: []string{"bind"}, Properties: map[string]string{"from": "assets", "target": "${destination:-/transient}"}}},
	}, func(source FromSource) (StageBind, error) {
		if source.StageID != "1" {
			t.Fatalf("unexpected base resolver source: %#v", source)
		}
		return StageBind{Inherited: inherited}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range publication.Stages {
		if stage.Name != "producer" {
			continue
		}
		if len(stage.Operations[0].Children) != 2 || stage.Operations[0].TransientMountCount != 1 {
			t.Fatalf("package producer lost inherited/transient mount distinction: %#v", stage.Operations[0])
		}
	}
	if got := publication.PackageArguments["destination"]; got != "/transient" {
		t.Fatalf("transient mount argument not fixed in package: %q", got)
	}
	for _, ref := range publication.Component.StageReferences {
		if ref.MountIndex >= 0 {
			t.Fatalf("build-only source mount leaked into component metadata: %#v", ref)
		}
	}
	if _, err := ValidatePublished(publication.Component); err != nil {
		t.Fatal(err)
	}
}
