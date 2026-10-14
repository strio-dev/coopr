package definition

import "testing"

func TestSerializedOnBuildRequiresNonemptyTrigger(t *testing.T) {
	for _, trigger := range []string{"", " ", "\t\n"} {
		if err := ValidateOperation(Instruction{Name: "onbuild", Arguments: []string{trigger}}); err == nil {
			t.Errorf("accepted empty ONBUILD trigger %q", trigger)
		}
	}
	if err := ValidateOperation(Instruction{Name: "onbuild", Arguments: []string{"RUN true"}}); err != nil {
		t.Fatal(err)
	}
}

func TestComponentArgumentsRejectNormalizationMetadata(t *testing.T) {
	for _, binding := range []Instruction{
		{Name: "arg", Arguments: []string{"name", "value"}, NativeContinuation: true},
		{Name: "arg", Arguments: []string{"name", "value"}, PropertyOrder: []string{"name"}},
	} {
		inst := Instruction{Name: "component", Arguments: []string{"./component.coopr"}, Children: []Instruction{binding}}
		if _, err := inst.ComponentArguments(); err == nil {
			t.Errorf("accepted internal normalization metadata on invocation binding: %#v", binding)
		}
	}
}
