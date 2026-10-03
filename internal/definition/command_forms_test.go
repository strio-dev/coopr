package definition

import (
	"reflect"
	"strings"
	"testing"
)

func TestCommandTextAndExecChildren(t *testing.T) {
	for _, name := range []string{"run", "cmd", "entrypoint", "healthcheck"} {
		t.Run(name, func(t *testing.T) {
			for _, test := range []struct {
				source string
				form   string
				args   []string
			}{
				{` "echo $HOME"`, "shell", []string{"echo $HOME"}},
				{` { exec "/app" }`, "exec", []string{"/app"}},
				{` { exec "/app" "" "$HOME" "space here" }`, "exec", []string{"/app", "", "$HOME", "space here"}},
			} {
				def, err := Parse(strings.NewReader(name + test.source))
				if err != nil {
					t.Fatal(err)
				}
				want := Instruction{Name: name, Form: test.form, Arguments: test.args}
				if name == "healthcheck" {
					prefix := "CMD-SHELL"
					if test.form == "exec" {
						prefix = "CMD"
					}
					want.Form = ""
					want.Arguments = append([]string{prefix}, test.args...)
				}
				if got := def.Instructions[0]; !reflect.DeepEqual(got, want) {
					t.Fatalf("%s%s = %#v, want %#v", name, test.source, got, want)
				}
				if err := Validate(def); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestExecChildPreservesParentOptionsAndSiblingOrder(t *testing.T) {
	def, err := Parse(strings.NewReader(`
run network="none" {
    mount "cache" target="/cache"
    exec "/app" "arg"
    device "vendor.example/gpu=one"
    mount "secret" target="/secret" mode="0400"
}
healthcheck interval="30s" retries=2 { exec "/check" "--ready" }
`))
	if err != nil {
		t.Fatal(err)
	}
	want := Instruction{Name: "run", Form: "exec", Arguments: []string{"/app", "arg"}, Properties: map[string]string{"network": "none"}, Children: []Instruction{
		{Name: "mount", Arguments: []string{"cache"}, Properties: map[string]string{"target": "/cache"}},
		{Name: "device", Arguments: []string{"vendor.example/gpu=one"}},
		{Name: "mount", Arguments: []string{"secret"}, Properties: map[string]string{"target": "/secret", "mode": "0400"}},
	}}
	if got := def.Instructions[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("RUN = %#v, want %#v", got, want)
	}
	if got := def.Instructions[1]; !reflect.DeepEqual(got.Arguments, []string{"CMD", "/check", "--ready"}) || got.Properties["interval"] != "30s" || got.Properties["retries"] != "2" {
		t.Fatalf("HEALTHCHECK = %#v", got)
	}
	if err := Validate(def); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidExecChildrenAndRemovedCommandSelectors(t *testing.T) {
	for _, name := range []string{"run", "cmd", "entrypoint", "healthcheck"} {
		for _, suffix := range []string{
			` "one" "two"`,
			` "shell" { exec "/app" }`,
			` { exec "/app"; exec "/other" }`,
			` { (exec)exec "/app" }`,
			` { exec (word)"/app" }`,
			` { exec "/app" network="none" }`,
			` { exec "/app" { exec "/nested" } }`,
			` { exec #null }`,
			` "true" shell=#false`,
			` "true" shell=#true`,
			` "true" shell="false"`,
			` "true" form="exec"`,
		} {
			source := name + suffix
			if _, err := Parse(strings.NewReader(source)); err == nil {
				t.Errorf("accepted invalid command %s", source)
			}
		}
	}
	for _, source := range []string{
		`exec "/app"`,
		`from "scratch" { exec "/app" }`,
		`copy "a" "b" { exec "/app" }`,
		`onbuild { exec "/app" }`,
		`run { exec }`,
		`healthcheck { exec }`,
		`healthcheck #null { exec "/check" }`,
		`run { mount "cache" { exec "/app" } }`,
	} {
		if _, err := Parse(strings.NewReader(source)); err == nil {
			t.Errorf("accepted invalid exec placement %s", source)
		}
	}
	if err := Validate(&Definition{Instructions: []Instruction{{Name: "exec", Arguments: []string{"/app"}}}}); err == nil {
		t.Fatal("accepted standalone exec in decoded definition")
	}
	if err := Validate(&Definition{Instructions: []Instruction{{Name: "run", Form: "exec", Arguments: []string{"/app"}, Children: []Instruction{{Name: "exec", Arguments: []string{"/other"}}}}}}); err == nil {
		t.Fatal("accepted unnormalized exec child in decoded definition")
	}
}

func TestOrdinaryMultilineExecArgumentAndQuotedModes(t *testing.T) {
	def, err := Parse(strings.NewReader(`
run {
    exec "/bin/sh" "-c" """
        printf '%s\\n' "$value" &&
        echo done
        """
}
copy "source" "/dest" chmod="${permissions}"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := def.Instructions[0]; got.Form != "exec" || got.Arguments[2] != "printf '%s\\n' \"$value\" &&\necho done" || len(got.Children) != 0 {
		t.Fatalf("multiline exec = %#v", got)
	}
	if got := def.Instructions[1].Properties["chmod"]; got != "${permissions}" {
		t.Fatalf("chmod = %q", got)
	}
}
