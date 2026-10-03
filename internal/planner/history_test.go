package planner

import (
	"reflect"
	"testing"
)

func TestStageHistoryRetainsARGOrderWithoutChangingOperationIndexes(t *testing.T) {
	plan := makePlan(t, `
arg "global" "outside"
from "scratch"
arg "first" "default"
env marker="set"
arg "unset"
run "true"
arg "last" "tail"
`, Options{Mode: Build, Arguments: map[string]string{"first": "override"}})
	stage := plan.Stages[0]
	if got := []string{stage.Operations[0].Name, stage.Operations[1].Name}; !reflect.DeepEqual(got, []string{"env", "run"}) {
		t.Fatalf("executable operation indexes changed: %v", got)
	}
	if len(stage.History) != 3 {
		t.Fatalf("history = %+v", stage.History)
	}
	wantBefore := []int{0, 1, 2}
	wantArguments := [][]string{{"first", "override"}, {"unset"}, {"last", "tail"}}
	for index, history := range stage.History {
		if history.Before != wantBefore[index] || history.Operation.Name != "arg" || !reflect.DeepEqual(history.Operation.Arguments, wantArguments[index]) {
			t.Errorf("history[%d] = %+v, want before=%d ARG %v", index, history, wantBefore[index], wantArguments[index])
		}
	}
}

func TestStageHistoryDistinguishesBlankAndUnsetARG(t *testing.T) {
	plan := makePlan(t, `
from "scratch"
arg "blank"
arg "unset"
run "true"
`, Options{Mode: Build, Arguments: map[string]string{"blank": ""}})
	history := plan.Stages[0].History
	if len(history) != 2 || !reflect.DeepEqual(history[0].Operation.Arguments, []string{"blank", ""}) || !reflect.DeepEqual(history[1].Operation.Arguments, []string{"unset"}) {
		t.Fatalf("ARG history lost blank/unset distinction: %+v", history)
	}
}
