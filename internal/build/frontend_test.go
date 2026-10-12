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

func TestApplyFromOverrideIncludesExtendOwnedFrom(t *testing.T) {
	def := &definition.Definition{Instructions: []definition.Instruction{
		{Name: "extend", Children: []definition.Instruction{
			{Name: "from", Arguments: []string{"first"}},
			{Name: "from", Arguments: []string{"second"}},
		}},
		{Name: "from", Arguments: []string{"producer"}},
	}}
	applyFromOverride(def, "override")
	if got := def.Instructions[0].Children[0].Arguments[0]; got != "override" {
		t.Fatalf("first nested FROM = %q", got)
	}
	if def.Instructions[0].Children[1].Arguments[0] != "second" || def.Instructions[1].Arguments[0] != "producer" {
		t.Fatal("override changed later bases")
	}
}
