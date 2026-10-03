package build

import (
	"testing"

	"coopr/internal/definition"
)

func TestApplyFromOverrideReplacesOnlyFirstFrom(t *testing.T) {
	def := &definition.Definition{Instructions: []definition.Instruction{
		{Name: "arg", Arguments: []string{"base"}},
		{Name: "from", Arguments: []string{"first"}},
		{Name: "from", Arguments: []string{"second"}},
	}}
	applyFromOverride(def, "override")
	if got := def.Instructions[1].Arguments[0]; got != "override" {
		t.Fatalf("first FROM = %q", got)
	}
	if got := def.Instructions[2].Arguments[0]; got != "second" {
		t.Fatalf("second FROM = %q", got)
	}
}
