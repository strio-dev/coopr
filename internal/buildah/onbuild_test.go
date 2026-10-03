package buildah

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"coopr/internal/planner"
)

func TestStageBindFromImageConfigPreservesRawTriggersAndEnvironment(t *testing.T) {
	logical, err := imageconfig.Parse([]byte(`{"config":{"Env":["PARENT=old","KEEP=value","PARENT=last=part"],"OnBuild":["ARG SOURCE=$PARENT", "COPY --from=$SOURCE /payload /installed"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	before, err := logical.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}

	got, err := stageBindFromImageConfig(logical)
	if err != nil {
		t.Fatal(err)
	}
	want := planner.StageBind{
		Inherited: []definition.Instruction{
			{Name: "arg", Arguments: []string{"SOURCE", "$PARENT"}, ProcessQuotes: true},
			{Name: "copy", Arguments: []string{"/payload", "/installed"}, Properties: map[string]string{"from": "$SOURCE"}, ProcessQuotes: true},
		},
		BaseEnvironment: map[string]string{"PARENT": "last=part", "KEEP": "value"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stage bind = %#v, want %#v", got, want)
	}
	after, err := logical.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("stage bind extraction mutated config:\nbefore: %s\nafter:  %s", before, after)
	}
	triggers, err := logical.OnBuild()
	if err != nil {
		t.Fatal(err)
	}
	if len(triggers) != 2 {
		t.Fatalf("stage bind extraction cleared ONBUILD: %v", triggers)
	}
}

func TestStageBindFromImageConfigRejectsInvalidTriggerWithoutMutation(t *testing.T) {
	logical, err := imageconfig.Parse([]byte(`{"config":{"OnBuild":["FROM scratch"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	before, err := logical.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stageBindFromImageConfig(logical); err == nil || !strings.Contains(err.Error(), "ONBUILD trigger 1") {
		t.Fatalf("expected invalid trigger error, got %v", err)
	}
	after, err := logical.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("failed extraction mutated config:\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestStageBindFromImageConfigAcceptsWhitespaceAroundInheritedTrigger(t *testing.T) {
	logical, err := imageconfig.Parse([]byte(`{"config":{"OnBuild":["  RUN echo inherited  "]}}`))
	if err != nil {
		t.Fatal(err)
	}
	bind, err := stageBindFromImageConfig(logical)
	if err != nil {
		t.Fatal(err)
	}
	if len(bind.Inherited) != 1 || bind.Inherited[0].Name != "run" || len(bind.Inherited[0].Arguments) != 1 || bind.Inherited[0].Arguments[0] != "echo inherited" {
		t.Fatalf("inherited ONBUILD = %#v", bind.Inherited)
	}
}

func TestConsumeOnBuildPreservesOrderAndContext(t *testing.T) {
	parent, err := imageconfig.Parse([]byte(`{"config":{"OnBuild":["ENV FLAG=ready", "COPY fixture /fixture", "RUN --network=none --mount=type=bind,source=fixture,target=/mnt cat /mnt"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	child := parent.Clone()
	operations, err := consumeOnBuild(child)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 3 || operations[0].Name != "env" || operations[1].Name != "copy" || operations[2].Name != "run" {
		t.Fatalf("unexpected ONBUILD operations: %#v", operations)
	}
	if operations[1].InputContext != "build" || operations[2].MountContexts[0] != "build" {
		t.Fatalf("ONBUILD input context missing: %#v", operations)
	}
	if !operations[2].NetworkExplicit || operations[2].Properties["network"] != "none" {
		t.Fatalf("ONBUILD explicit network missing: %#v", operations[2])
	}
	parentTriggers, err := parent.OnBuild()
	if err != nil {
		t.Fatal(err)
	}
	childTriggers, err := child.OnBuild()
	if err != nil {
		t.Fatal(err)
	}
	if len(parentTriggers) != 3 || len(childTriggers) != 0 {
		t.Fatalf("parent triggers %v, child triggers %v", parentTriggers, childTriggers)
	}
}

func TestConsumeOnBuildPreservesHeredocTriggerOrder(t *testing.T) {
	logical := imageconfig.New()
	for _, trigger := range []string{
		"COPY <<EOF /message\nhello\nEOF",
		"RUN <<EOF\ncat /message > /result\nEOF",
		"ADD <<'EOF' /literal\n$UNEXPANDED\nEOF",
	} {
		if err := logical.Apply(definition.Instruction{Name: "onbuild", Arguments: []string{trigger}}); err != nil {
			t.Fatal(err)
		}
	}
	operations, err := consumeOnBuild(logical)
	if err != nil {
		t.Fatal(err)
	}
	want := []definition.Instruction{
		{Name: "copy", Arguments: []string{"/message"}, InlineFiles: []definition.InlineFile{{Path: "EOF", Data: "hello\n"}}},
		{Name: "run", Form: "shell", Arguments: []string{"cat /message > /result\n"}},
		{Name: "add", Arguments: []string{"/literal"}, InlineFiles: []definition.InlineFile{{Path: "EOF", Data: "$UNEXPANDED\n"}}},
	}
	if len(operations) != len(want) {
		t.Fatalf("operations = %#v", operations)
	}
	for index := range want {
		if !reflect.DeepEqual(operations[index].Instruction, want[index]) {
			t.Fatalf("operation %d = %#v, want %#v", index, operations[index].Instruction, want[index])
		}
	}
}

func TestConsumeOnBuildRejectsStageReferenceWithoutMutatingConfig(t *testing.T) {
	logical, err := imageconfig.Parse([]byte(`{"config":{"OnBuild":["COPY --from=builder /value /value"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := consumeOnBuild(logical); err == nil || !strings.Contains(err.Error(), "--from stage references") {
		t.Fatalf("expected unsupported ONBUILD dependency, got %v", err)
	}
	encoded, err := logical.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(encoded) || !strings.Contains(string(encoded), `"OnBuild"`) {
		t.Fatalf("failed ONBUILD consumption mutated config: %s", encoded)
	}
}

func TestConsumeOnBuildRejectsARGWithoutMutatingConfig(t *testing.T) {
	logical, err := imageconfig.Parse([]byte(`{"config":{"OnBuild":["ARG SOURCE=$PARENT"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := consumeOnBuild(logical); err == nil || !strings.Contains(err.Error(), "ARG requires child-stage planning") {
		t.Fatalf("expected deferred ONBUILD ARG planning error, got %v", err)
	}
	encoded, err := logical.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"OnBuild"`) {
		t.Fatalf("failed ONBUILD consumption mutated config: %s", encoded)
	}
}

func TestConsumeOnBuildExpandsAgainstParentAndEarlierTriggers(t *testing.T) {
	logical, err := imageconfig.Parse([]byte(`{"config":{"Env":["PARENT=fixture"],"OnBuild":["ENV SOURCE=$PARENT", "COPY $SOURCE /inherited", "RUN echo $SOURCE"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	operations, err := consumeOnBuild(logical)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 3 {
		t.Fatalf("ONBUILD operations = %#v", operations)
	}
	if got := operations[0].Arguments; !reflect.DeepEqual(got, []string{"SOURCE", "fixture"}) {
		t.Fatalf("expanded ENV = %#v", got)
	}
	if got := operations[1].Arguments; !reflect.DeepEqual(got, []string{"fixture", "/inherited"}) {
		t.Fatalf("expanded COPY = %#v", got)
	}
	if got := operations[2].Arguments; !reflect.DeepEqual(got, []string{"echo $SOURCE"}) {
		t.Fatalf("RUN shell command expanded too early: %#v", got)
	}
}

func TestConsumeOnBuildHandlesMultiValueConfigTriggers(t *testing.T) {
	logical, err := imageconfig.Parse([]byte(`{"config":{"OnBuild":["EXPOSE 80 443", "VOLUME /one /two"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	operations, err := consumeOnBuild(logical)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 4 || operations[0].Name != "expose" || operations[1].Name != "expose" || operations[2].Name != "volume" || operations[3].Name != "volume" {
		t.Fatalf("multi-value ONBUILD config operations = %#v", operations)
	}
}

func TestConsumeOnBuildExpandsMultiValueENVAgainstPriorEnvironment(t *testing.T) {
	logical, err := imageconfig.Parse([]byte(`{"config":{"Env":["A=base"],"OnBuild":["ENV A=one B=$A"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	operations, err := consumeOnBuild(logical)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "one", "B": "base"}
	if len(operations) != 1 || !reflect.DeepEqual(operations[0].Properties, want) {
		t.Fatalf("multi-value ONBUILD ENV = %#v, want %#v", operations, want)
	}
}

func TestParseOnBuildTriggerUsesDockerInstructionSyntax(t *testing.T) {
	for _, test := range []struct {
		trigger string
		want    []definition.Instruction
	}{
		{
			trigger: `RUN printf inherited >/proof`,
			want:    []definition.Instruction{{Name: "run", Form: "shell", Arguments: []string{"printf inherited >/proof"}}},
		},
		{
			trigger: `RUN --network=none ["/bin/sh","-c","echo ready"]`,
			want: []definition.Instruction{{
				Name: "run", Form: "exec", Arguments: []string{"/bin/sh", "-c", "echo ready"},
				Properties: map[string]string{"network": "none"},
			}},
		},
		{
			trigger: `COPY --from=producer --link /payload /installed`,
			want: []definition.Instruction{{
				Name: "copy", Arguments: []string{"/payload", "/installed"},
				Properties:    map[string]string{"from": "producer", "link": "true"},
				ProcessQuotes: true,
			}},
		},
		{
			trigger: `ENV A=first B=second`,
			want:    []definition.Instruction{{Name: "env", Properties: map[string]string{"A": "first", "B": "second"}, ProcessQuotes: true}},
		},
		{
			trigger: `ARG SOURCE=$PARENT EMPTY`,
			want: []definition.Instruction{
				{Name: "arg", Arguments: []string{"SOURCE", "$PARENT"}, ProcessQuotes: true},
				{Name: "arg", Arguments: []string{"EMPTY"}, ProcessQuotes: true},
			},
		},
		{
			trigger: `EXPOSE 80 443`,
			want: []definition.Instruction{
				{Name: "expose", Arguments: []string{"443"}, ProcessQuotes: true},
				{Name: "expose", Arguments: []string{"80"}, ProcessQuotes: true},
			},
		},
		{
			trigger: `VOLUME /one /two`,
			want: []definition.Instruction{
				{Name: "volume", Arguments: []string{"/one"}, ProcessQuotes: true},
				{Name: "volume", Arguments: []string{"/two"}, ProcessQuotes: true},
			},
		},
	} {
		t.Run(test.trigger, func(t *testing.T) {
			got, err := parseOnBuildTrigger(test.trigger)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parsed trigger = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestParseOnBuildARGDefersDefaultExpansion(t *testing.T) {
	got, err := parseOnBuildTriggerWithExpander(`ARG SOURCE=$PARENT`, func(word string) (string, error) {
		return strings.ReplaceAll(word, "$PARENT", "parent-value"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []definition.Instruction{{Name: "arg", Arguments: []string{"SOURCE", "$PARENT"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inherited ARG was expanded before child planning: got %#v, want %#v", got, want)
	}
}

func TestParseOnBuildInsecureRunPreservesSecurityAndNetwork(t *testing.T) {
	got, err := parseOnBuildTrigger(`RUN --security=insecure --network=none printf inherited`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "run" || got[0].Properties["security"] != "insecure" || got[0].Properties["network"] != "none" {
		t.Fatalf("inherited RUN = %#v", got)
	}
}

func TestParseOnBuildHeredocUsesRawContentExpansion(t *testing.T) {
	const trigger = "COPY <<EOF /message\n\"$NAME\"\nEOF"
	got, err := parseOnBuildTriggerWithExpanders(trigger,
		func(word string) (string, error) { return strings.ReplaceAll(word, "$NAME", "normal"), nil },
		func(word string) (string, error) { return strings.ReplaceAll(word, "$NAME", "raw"), nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []definition.Instruction{{Name: "copy", Arguments: []string{"/message"}, InlineFiles: []definition.InlineFile{{Path: "EOF", Data: "\"raw\"\n"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed heredoc = %#v, want %#v", got, want)
	}
}

func TestParseOnBuildTriggerRejectsUnsafeOrUnsupportedSyntax(t *testing.T) {
	for _, trigger := range []string{
		`FROM scratch`, `ONBUILD RUN echo nested`, `MAINTAINER someone`,
		"RUN true\nRUN false",
		`RUN <<EOF`, `DOESNOTEXIST hello`,
	} {
		t.Run(strings.ReplaceAll(trigger, "\n", "_"), func(t *testing.T) {
			if _, err := parseOnBuildTrigger(trigger); err == nil {
				t.Fatalf("accepted unsupported ONBUILD trigger %q", trigger)
			}
		})
	}
}
