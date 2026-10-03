package planner

import (
	"testing"

	"coopr/internal/definition"
	"coopr/internal/onbuildparse"
)

func TestNormalizePreservesOnBuildTriggerForChildStage(t *testing.T) {
	trigger := "RUN printf '%s' '$CHILD_VALUE' > /generated"
	normalized, err := normalize(definition.Instruction{
		Name: "onbuild", Arguments: []string{trigger},
	}, scope{"CHILD_VALUE": {value: "parent-value", present: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized.Arguments) != 1 || normalized.Arguments[0] != trigger {
		t.Fatalf("ONBUILD trigger changed at author time: %#v", normalized.Arguments)
	}
}

func TestNormalizeDeferredOnBuildHeredocRespectsQuotedDelimiter(t *testing.T) {
	for _, test := range []struct {
		name    string
		trigger string
		values  scope
		want    string
	}{
		{"unquoted", "COPY <<EOF /message\n\"$NAME\"\nEOF", scope{"NAME": {value: "child", present: true}}, "\"child\"\n"},
		{"unquoted undefined", "COPY <<EOF /message\nbefore$UNSET-after\nEOF", nil, "before-after\n"},
		{"quoted", "COPY <<'EOF' /message\n\"$NAME\" C:\\literal\nEOF", nil, "\"$NAME\" C:\\literal\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			inherited, err := onbuildparse.ParseDeferred(test.trigger)
			if err != nil {
				t.Fatal(err)
			}
			normalized, err := normalize(inherited[0], test.values)
			if err != nil {
				t.Fatal(err)
			}
			if got := normalized.InlineFiles[0].Data; got != test.want {
				t.Fatalf("inline heredoc content = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNormalizeLeavesRuntimeCommandsForTheirRuntime(t *testing.T) {
	for _, name := range []string{"run", "cmd", "entrypoint", "healthcheck"} {
		t.Run(name, func(t *testing.T) {
			inst := definition.Instruction{Name: name, Form: "shell", Arguments: []string{"echo $RUNTIME_VALUE"}}
			if name == "healthcheck" {
				inst.Form = ""
				inst.Arguments = []string{"CMD-SHELL", "echo $RUNTIME_VALUE"}
			}
			got, err := normalize(inst, scope{"RUNTIME_VALUE": {value: "build-value", present: true}})
			if err != nil {
				t.Fatal(err)
			}
			if got.Arguments[len(got.Arguments)-1] != "echo $RUNTIME_VALUE" {
				t.Fatalf("%s was expanded during planning: %#v", name, got.Arguments)
			}
		})
	}
}
