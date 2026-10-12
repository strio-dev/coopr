package definition

import (
	"reflect"
	"strings"
	"testing"
)

func TestScriptChildrenCanonicalShellForms(t *testing.T) {
	for _, name := range []string{"run", "cmd", "entrypoint", "healthcheck"} {
		t.Run(name, func(t *testing.T) {
			plain, err := parseFragment(strings.NewReader(name + ` "echo $HOME"`))
			if err != nil {
				t.Fatal(err)
			}
			child, err := parseFragment(strings.NewReader(name + ` { script "echo $HOME" }`))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(child, plain) {
				t.Fatalf("child = %#v, plain = %#v", child, plain)
			}
			if err := validateFragment(child); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestScriptExecSequenceAndMultiline(t *testing.T) {
	def, err := parseFragment(strings.NewReader(`run {
 script """
  #!/ignored/interpreter
  printf '%s\\n' "$HOME"
  """
 mount "tmpfs" target="/work"
 exec "cat" "/work/home"
 script "echo done"
}`))
	if err != nil {
		t.Fatal(err)
	}
	got := def.Instructions[0]
	if got.Form != "" || got.Arguments != nil || len(got.InlineFiles) != 0 || len(got.Commands) != 3 {
		t.Fatalf("sequence = %#v", got)
	}
	if got.Commands[0].Form != "shell" || !strings.HasPrefix(got.Commands[0].Arguments[0], "#!/ignored/interpreter") || got.Commands[1].Form != "exec" || got.Commands[2].Form != "shell" || len(got.Children) != 1 {
		t.Fatalf("commands = %#v", got)
	}
	single, err := parseFragment(strings.NewReader(`run { script """
  #!/ignored/interpreter
  echo done
  """ }`))
	if err != nil {
		t.Fatal(err)
	}
	if single.Instructions[0].Form != "shell" || len(single.Instructions[0].InlineFiles) != 0 {
		t.Fatalf("single = %#v", single)
	}
}

func TestScriptChildValidation(t *testing.T) {
	for _, name := range []string{"run", "cmd", "entrypoint", "healthcheck"} {
		for _, child := range []string{"script", "exec"} {
			if _, err := parseFragment(strings.NewReader(name + ` { ` + child + ` "a\u{0}b" }`)); err == nil {
				t.Errorf("accepted NUL in %s %s", name, child)
			}
		}
	}

	for _, name := range []string{"run", "cmd", "entrypoint", "healthcheck"} {
		suffixes := []string{
			` "text" { script "other" }`,
			` { script }`, ` { script "a" "b" }`, ` { script #true }`, ` { script 42 }`,
			` { script #null }`, ` { script (word)"text" }`, ` { (kind)script "text" }`,
			` { script "text" key="value" }`, ` { script "text" {} }`,
		}
		if name != "run" {
			suffixes = append(suffixes, ` { script "text"; exec "echo" }`, ` { exec "echo"; script "text" }`, ` { script "text"; script "other" }`)
		}
		for _, suffix := range suffixes {
			if _, err := parseFragment(strings.NewReader(name + suffix)); err == nil {
				t.Errorf("accepted %s%s", name, suffix)
			}
		}
	}
	for _, source := range []string{`script "text"`, `from "scratch" { script "text" }`, `copy "a" "b" { script "text" }`, `run { mount "cache" { script "text" } }`, `onbuild { script "text" }`} {
		if _, err := parseFragment(strings.NewReader(source)); err == nil {
			t.Errorf("accepted %s", source)
		}
	}
	for _, bad := range []Command{
		{Form: "shell"}, {Form: "shell", Arguments: []string{"a", "b"}},
		{Form: "shell", Arguments: []string{"a\x00b"}}, {Form: "exec", Arguments: []string{"a", "b\x00c"}},
		{Form: "script", Arguments: []string{"text"}}, {Arguments: []string{"text"}},
	} {
		inst := Instruction{Name: "run", Commands: []Command{{Form: "exec", Arguments: []string{"true"}}, bad}}
		if err := ValidateOperation(inst); err == nil {
			t.Errorf("accepted command %#v", bad)
		}
	}
	if err := ValidateOperation(Instruction{Name: "run", Form: "shell", Arguments: []string{"a"}, Children: []Instruction{{Name: "script", Arguments: []string{"b"}}}}); err == nil {
		t.Fatal("accepted unnormalized decoded script")
	}
}

func TestOnBuildScriptUsesExistingMetadataForm(t *testing.T) {
	def, err := Parse(strings.NewReader(`from "scratch" {
 onbuild { run { script "echo $HOME" } }
 onbuild { cmd { script "echo ready" } }
 onbuild { entrypoint { script "echo start" } }
 onbuild { healthcheck { script "test -f /ready" } }
}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"RUN echo $HOME", "CMD echo ready", "ENTRYPOINT echo start", "HEALTHCHECK CMD test -f /ready"}
	for i, inst := range def.Instructions[0].Children {
		if !reflect.DeepEqual(inst.Arguments, []string{want[i]}) {
			t.Fatalf("trigger %d = %#v", i, inst)
		}
	}
}

func TestOnBuildRejectsScriptSequences(t *testing.T) {
	for _, commands := range []string{`script "echo first"; script "echo second"`, `script "echo first"; exec "true"`, `exec "true"; script "echo second"`} {
		_, err := Parse(strings.NewReader(`from "scratch" { onbuild { run { ` + commands + ` } } }`))
		if err == nil || !strings.Contains(err.Error(), "ONBUILD metadata cannot represent a command sequence") {
			t.Fatalf("%s error = %v", commands, err)
		}
	}
}
