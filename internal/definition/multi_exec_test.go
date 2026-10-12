package definition

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRunExecSequencePreservesCommandAndMountOrder(t *testing.T) {
	def, err := Parse(strings.NewReader(`from "scratch" {
 run network="none" {
  mount "tmpfs" target="/temporary"
  exec "/first" "" "space here"
  device "vendor.example/gpu=one"
  exec "/second" "$value"
  mount "cache" target="/cache"
  exec "/third" #true 42
 }
}`))
	if err != nil {
		t.Fatal(err)
	}
	got := def.Instructions[0].Children[0]
	want := []Command{{Form: "exec", Arguments: []string{"/first", "", "space here"}}, {Form: "exec", Arguments: []string{"/second", "$value"}}, {Form: "exec", Arguments: []string{"/third", "true", "42"}}}
	if !reflect.DeepEqual(got.Commands, want) || got.Arguments != nil || got.Form != "" {
		t.Fatalf("RUN = %#v", got)
	}
	if len(got.Children) != 3 || got.Children[0].Name != "mount" || got.Children[1].Name != "device" || got.Children[2].Name != "mount" {
		t.Fatalf("mount/device order = %#v", got.Children)
	}
	encoded, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Definition
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := Validate(&decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Instructions[0].Children[0], got) {
		t.Fatalf("roundtrip = %#v", decoded)
	}
}

func TestInvalidDecodedRunExecSequences(t *testing.T) {
	for _, test := range []struct {
		name string
		inst Instruction
	}{
		{"wrong instruction", Instruction{Name: "cmd", Commands: []Command{{Form: "exec", Arguments: []string{"a"}}, {Form: "exec", Arguments: []string{"b"}}}}},
		{"wrong form", Instruction{Name: "run", Form: "shell", Commands: []Command{{Form: "exec", Arguments: []string{"a"}}, {Form: "exec", Arguments: []string{"b"}}}}},
		{"empty sequence", Instruction{Name: "run", Commands: []Command{}}},
		{"one command", Instruction{Name: "run", Commands: []Command{{Form: "exec", Arguments: []string{"a"}}}}},
		{"empty argv", Instruction{Name: "run", Commands: []Command{{Form: "exec", Arguments: []string{"a"}}, {Form: "exec"}}}},
		{"empty executable", Instruction{Name: "run", Commands: []Command{{Form: "exec", Arguments: []string{""}}, {Form: "exec", Arguments: []string{"b"}}}}},
		{"arguments", Instruction{Name: "run", Form: "exec", Arguments: []string{"a"}, Commands: []Command{{Form: "exec", Arguments: []string{"a"}}, {Form: "exec", Arguments: []string{"b"}}}}},
		{"deferred trigger", Instruction{Name: "run", Commands: []Command{{Form: "exec", Arguments: []string{"a"}}, {Form: "exec", Arguments: []string{"b"}}}, DeferredOnBuild: "RUN true"}},
		{"inline file", Instruction{Name: "run", Commands: []Command{{Form: "exec", Arguments: []string{"a"}}, {Form: "exec", Arguments: []string{"b"}}}, InlineFiles: []InlineFile{{Path: "script", Data: "true"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateOperation(test.inst); err == nil {
				t.Fatalf("accepted %#v", test.inst)
			}
		})
	}
	for _, source := range []string{
		`run { exec; exec "b" }`,
		`run { exec "a"; exec "" }`,
		`run { exec "a"; exec #null }`,
		`run { exec "a"; exec (word)"b" }`,
		`run { exec "a"; exec "b" key="value" }`,
	} {
		if _, err := parseFragment(strings.NewReader(source)); err == nil {
			t.Errorf("accepted %s", source)
		}
	}
}

func TestOnBuildRejectsExecSequence(t *testing.T) {
	_, err := Parse(strings.NewReader(`from "scratch" { onbuild { run { exec "a"; exec "b" } } }`))
	if err == nil || !strings.Contains(err.Error(), "ONBUILD metadata cannot represent a command sequence") {
		t.Fatalf("error = %v", err)
	}
}

func TestComponentArgumentRejectsHiddenExecSequence(t *testing.T) {
	for _, commands := range [][]Command{{}, {{Form: "exec", Arguments: []string{"first"}}, {Form: "exec", Arguments: []string{"second"}}}} {
		inst := Instruction{Name: "component", Arguments: []string{"./component.coopr"}, Children: []Instruction{{Name: "arg", Arguments: []string{"message", "hello"}, Commands: commands}}}
		if _, err := inst.ComponentArguments(); err == nil {
			t.Fatal("accepted commands on a component argument")
		}
		if err := ValidateOperation(inst); err == nil {
			t.Fatal("decoded component argument bypassed RUN-only validation")
		}
	}
}
