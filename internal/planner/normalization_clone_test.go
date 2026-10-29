package planner

import (
	"reflect"
	"testing"

	"github.com/strio-dev/coopr/internal/definition"
)

func TestCloneInstructionIsolatesPropertyOrder(t *testing.T) {
	original := definition.Instruction{Name: "env", PropertyOrder: []string{"Z", "A"}, Children: []definition.Instruction{{Name: "env", PropertyOrder: []string{"B", "C"}}}}
	cloned := cloneInstruction(original)
	cloned.PropertyOrder[0] = "changed"
	cloned.Children[0].PropertyOrder[0] = "changed"
	if !reflect.DeepEqual(original.PropertyOrder, []string{"Z", "A"}) || !reflect.DeepEqual(original.Children[0].PropertyOrder, []string{"B", "C"}) {
		t.Fatalf("clone changed source instruction order: %#v", original)
	}
}
