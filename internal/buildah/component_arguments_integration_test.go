package buildah

import (
	"strings"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/definition"
)

func TestComponentArgumentDisplay(t *testing.T) {
	instruction := definition.Instruction{Name: "component", Arguments: []string{"./shared.coopr#selected"}, Children: []definition.Instruction{
		{Name: "arg", Arguments: []string{"channel", "preview"}},
		{Name: "arg", Arguments: []string{"target", "ordinary"}},
	}}
	if got, want := historyInstructionArguments(instruction), "--build-arg=channel=preview --build-arg=target=ordinary ./shared.coopr#selected"; got != want {
		t.Fatalf("component display = %q, want %q", got, want)
	}
}

func TestComponentArgumentBlocksPreserveCallerScopeAndCache(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "shared.coopr", `extend as="selected" {
    arg "channel"
    arg "target"
    arg "platform"
    arg "private" "callee"
    env selected_channel="${channel}" selected_target="${target}" selected_platform="${platform}"
}
`)
	for _, output := range []string{"cold", "warm"} {
		_, layout := f.build(t, output, `from "scratch" {
    arg "channel" "caller"
    env requested="preview"
    component "./shared.coopr#selected" {
        arg "channel" "${requested}"
        arg "target" "ordinary-target"
        arg "platform" "ordinary-platform"
    }
    env caller_channel="${channel}" private_after="${private}"
}
`)
		_, image := readPlanImage(t, layout)
		for _, expected := range []string{"selected_channel=preview", "selected_target=ordinary-target", "selected_platform=ordinary-platform", "caller_channel=caller", "private_after="} {
			if !strings.Contains("\n"+strings.Join(image.Config.Env, "\n")+"\n", "\n"+expected+"\n") {
				t.Fatalf("%s config missing %q: %v", output, expected, image.Config.Env)
			}
		}
	}
}

func TestComponentArgumentsKeepDeclarationOrderOutOfSelectionKeys(t *testing.T) {
	first := definition.Instruction{Name: "component", Arguments: []string{"./shared.coopr"}, Children: []definition.Instruction{
		{Name: "arg", Arguments: []string{"one", "1"}}, {Name: "arg", Arguments: []string{"two", "2"}},
	}}
	second := first
	second.Children = []definition.Instruction{first.Children[1], first.Children[0]}
	a, err := first.ComponentArguments()
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.ComponentArguments()
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	firstKey, err := componentInvocationSelectionKey(first.Arguments[0], a, platform)
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := componentInvocationSelectionKey(second.Arguments[0], b, platform)
	if err != nil {
		t.Fatal(err)
	}
	if firstKey != secondKey {
		t.Fatalf("argument order changed selection key: %q != %q", firstKey, secondKey)
	}
}
