package buildah

import (
	"reflect"
	"slices"
	"testing"

	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/planner"
)

func TestMixedRunCommandsUseSelectedShell(t *testing.T) {
	plan := testPlan(t, `from "scratch" {
 shell "/custom/shell" "-euc"
 run {
  script "echo $value > /proof"
  mount "tmpfs" target="/temporary"
  exec "/tool" "$value" ""
  script "test -s /proof"
 }
}`)
	request, err := RequestFromPlan(plan, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Operations) != 2 {
		t.Fatalf("operations = %v, want SHELL and one RUN", request.Operations)
	}
	run := request.Operations[1].(Run)
	want := [][]string{
		{"/custom/shell", "-euc", "echo $value > /proof"},
		{"/tool", "$value", ""},
		{"/custom/shell", "-euc", "test -s /proof"},
	}
	if !reflect.DeepEqual(run.Commands, want) || len(run.Command) != 0 || len(run.Mounts) != 1 {
		t.Fatalf("RUN = %#v", run)
	}
}

func TestScriptChildLowersLikeShellText(t *testing.T) {
	for _, name := range []string{"run", "cmd", "entrypoint", "healthcheck"} {
		t.Run(name, func(t *testing.T) {
			lower := func(instruction string) Request {
				plan := testPlan(t, `from "scratch" {
 shell "/custom/shell" "-ec"
 `+instruction+`
}`)
				request, err := RequestFromPlan(plan, PlanOptions{})
				if err != nil {
					t.Fatal(err)
				}
				return request
			}
			text := lower(name + ` "echo $value"`)
			child := lower(name + ` { script "echo $value" }`)
			if !reflect.DeepEqual(child.Operations, text.Operations) {
				t.Fatalf("script child = %#v, shell text = %#v", child.Operations, text.Operations)
			}
		})
	}
}

func TestMixedRunCacheAndHistoryPreserveFormsAndOrder(t *testing.T) {
	inst := definition.Instruction{Name: "run", Commands: []definition.Command{
		{Form: "shell", Arguments: []string{"echo $value > /proof"}},
		{Form: "exec", Arguments: []string{"/tool", "$value"}},
	}}
	key := func(value definition.Instruction) string {
		result, cacheable, err := instructionCacheKey(instructionCacheTestInput(planner.Operation{Instruction: value}))
		if err != nil || !cacheable {
			t.Fatalf("key = %s cacheable = %v err = %v", result, cacheable, err)
		}
		return result.String()
	}
	original := key(inst)
	changed := inst
	changed.Commands = slices.Clone(inst.Commands)
	changed.Commands[0].Form = "exec"
	reversed := inst
	reversed.Commands = []definition.Command{inst.Commands[1], inst.Commands[0]}
	if original == key(changed) || original == key(reversed) || key(changed) == key(reversed) {
		t.Fatal("cache key omitted command forms or order")
	}
	history, ok := plannedOperationHistory(planner.Operation{Instruction: inst}, nil)
	if !ok || history != `RUN script ["echo $value \u003e /proof"] exec ["/tool","$value"]` {
		t.Fatalf("history = %q ok = %v", history, ok)
	}
}

func TestLowerMixedRunRejectsInvalidDecodedCommands(t *testing.T) {
	for _, command := range []definition.Command{
		{Form: "shell", Arguments: []string{"one", "two"}},
		{Form: "shell"},
		{Form: "exec", Arguments: []string{""}},
		{Form: "unknown", Arguments: []string{"true"}},
	} {
		inst := definition.Instruction{Name: "run", Commands: []definition.Command{command, {Form: "exec", Arguments: []string{"/tool"}}}}
		_, _, err := lowerOperation(planner.Operation{Instruction: inst}, []string{"/bin/sh", "-c"})
		if err == nil {
			t.Fatalf("invalid command %#v: err = %v", command, err)
		}
	}
}
