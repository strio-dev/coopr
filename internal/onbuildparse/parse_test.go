package onbuildparse

import (
	"reflect"
	"strings"
	"testing"

	"coopr/internal/definition"
)

func TestParseRunHeredocAsMultilineShellCommand(t *testing.T) {
	instructions, err := Parse("RUN <<-EOF\n\techo inherited > /marker\nEOF")
	if err != nil {
		t.Fatal(err)
	}
	want := definition.Instruction{Name: "run", Form: "shell", Arguments: []string{"echo inherited > /marker\n"}}
	if !reflect.DeepEqual(instructions, []definition.Instruction{want}) {
		t.Fatalf("parsed ONBUILD RUN heredoc = %#v, want %#v", instructions, want)
	}
}

func TestParseComplexRunHeredocPreservesShellSyntax(t *testing.T) {
	trigger := "RUN <<'EOF' | cat > /marker\n$VALUE\nEOF"
	instructions, err := Parse(trigger)
	if err != nil {
		t.Fatal(err)
	}
	want := "<<'EOF' | cat > /marker\n$VALUE\nEOF"
	if len(instructions) != 1 || instructions[0].Form != "shell" || !reflect.DeepEqual(instructions[0].Arguments, []string{want}) {
		t.Fatalf("parsed complex ONBUILD RUN heredoc = %#v, want shell command %q", instructions, want)
	}
}

func TestParseCopyAndAddHeredocAsInlineSource(t *testing.T) {
	for _, test := range []struct {
		trigger string
		name    string
		want    string
	}{
		{"COPY <<EOF /message\nhello $name\nEOF", "copy", "hello child\n"},
		{"ADD <<'EOF' /literal\nhello $name\nEOF", "add", "hello $name\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			instructions, err := ParseExpandedRaw(test.trigger,
				func(word string) (string, error) { return strings.ReplaceAll(word, "$name", "child"), nil },
				func(word string) (string, error) { return strings.ReplaceAll(word, "$name", "child"), nil },
			)
			if err != nil {
				t.Fatal(err)
			}
			want := definition.Instruction{Name: test.name, Arguments: []string{"/" + map[string]string{"copy": "message", "add": "literal"}[test.name]}, InlineFiles: []definition.InlineFile{{Path: "EOF", Data: test.want}}}
			if !reflect.DeepEqual(instructions, []definition.Instruction{want}) {
				t.Fatalf("parsed ONBUILD %s heredoc = %#v, want %#v", test.name, instructions, want)
			}
		})
	}
}

func TestParseCopyHeredocRawExpansionPreservesQuotes(t *testing.T) {
	instructions, err := ParseExpandedRaw("COPY <<EOF /message\n\"$name\"\nEOF",
		func(word string) (string, error) { return word, nil },
		func(word string) (string, error) { return strings.ReplaceAll(word, "$name", "child"), nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := instructions[0].InlineFiles[0].Data; got != "\"child\"\n" {
		t.Fatalf("inline content = %q, want preserved quotes", got)
	}
}

func TestParsePreservesMixedMultipleAndExecutableHeredocs(t *testing.T) {
	copyInstructions, err := Parse("COPY local <<ONE <<'TWO' /messages/\none\nONE\ntwo\nTWO")
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := []definition.InlineFile{{Path: "ONE", Data: "one\n", Expand: true}, {Path: "TWO", Data: "two\n"}}
	if got := copyInstructions[0]; !reflect.DeepEqual(got.Arguments, []string{"local", "/messages/"}) || !reflect.DeepEqual(got.InlineFiles, wantFiles) {
		t.Fatalf("mixed COPY = %#v", got)
	}
	runInstructions, err := Parse("RUN <<EOF\n#!/bin/sh\necho hello\nEOF")
	if err != nil {
		t.Fatal(err)
	}
	if got := runInstructions[0]; got.Form != "exec" || !reflect.DeepEqual(got.Arguments, []string{"/run/coopr-heredoc/EOF"}) || got.InlineFiles[0].Path != "EOF" {
		t.Fatalf("shebang RUN = %#v", got)
	}
	empty, err := Parse("RUN <<EOF\nEOF")
	if err != nil || empty[0].Arguments[0] != "" {
		t.Fatalf("empty RUN = %#v, %v", empty, err)
	}
}

func TestParseHeredocPreservesAcceptedSourceFlags(t *testing.T) {
	copyInstructions, err := Parse("COPY --from=base --parents --exclude=*.tmp <<EOF /dest/\npayload\nEOF")
	if err != nil {
		t.Fatal(err)
	}
	copy := copyInstructions[0]
	if copy.Properties["from"] != "base" || copy.Properties["parents"] != "true" || len(copy.Children) != 1 || !reflect.DeepEqual(copy.Children[0], definition.Instruction{Name: "exclude", Arguments: []string{"*.tmp"}}) {
		t.Fatalf("COPY heredoc flags = %#v", copy)
	}
	addInstructions, err := Parse("ADD --keep-git-dir=true --unpack=false <<EOF /dest\npayload\nEOF")
	if err != nil {
		t.Fatal(err)
	}
	add := addInstructions[0]
	if add.Properties["keep-git-dir"] != "true" || add.Properties["unpack"] != "false" {
		t.Fatalf("ADD heredoc flags = %#v", add)
	}
}

func TestParseRunMountsPreservesSecretSSHAndWritableBindOptions(t *testing.T) {
	for _, test := range []struct {
		trigger string
		mount   definition.Instruction
	}{
		{
			trigger: `RUN --mount=type=secret,id=token,env=TOKEN,required=true,mode=0400,uid=1000,gid=1001 test -n "$TOKEN"`,
			mount: definition.Instruction{Name: "mount", Arguments: []string{"secret"}, Properties: map[string]string{
				"id": "token", "env": "TOKEN", "required": "true", "mode": "400", "uid": "1000", "gid": "1001",
			}},
		},
		{
			trigger: `RUN --mount=type=ssh,id=default,required=true ssh -T git@example.com`,
			mount: definition.Instruction{Name: "mount", Arguments: []string{"ssh"}, Properties: map[string]string{
				"id": "default", "required": "true",
			}},
		},
		{
			trigger: `RUN --mount=type=bind,from=source,source=/data,target=/mnt,rw test -e /mnt/file`,
			mount: definition.Instruction{Name: "mount", Arguments: []string{"bind"}, Properties: map[string]string{
				"from": "source", "source": "/data", "target": "/mnt", "readonly": "false",
			}},
		},
	} {
		t.Run(test.trigger, func(t *testing.T) {
			instructions, err := Parse(test.trigger)
			if err != nil {
				t.Fatal(err)
			}
			if len(instructions) != 1 || len(instructions[0].Children) != 1 {
				t.Fatalf("parsed ONBUILD RUN = %#v", instructions)
			}
			if !reflect.DeepEqual(instructions[0].Children[0], test.mount) {
				t.Fatalf("mount = %#v, want %#v", instructions[0].Children[0], test.mount)
			}
		})
	}
}

func TestParseAddPreservesGitAndUnpackOptions(t *testing.T) {
	for _, test := range []struct {
		trigger string
		want    map[string]string
	}{
		{
			trigger: `ADD --keep-git-dir=true https://example.com/source.git /source`,
			want:    map[string]string{"keep-git-dir": "true"},
		},
		{
			trigger: `ADD --unpack=false https://example.com/archive.tar.gz /archive.tar.gz`,
			want:    map[string]string{"unpack": "false"},
		},
	} {
		t.Run(test.trigger, func(t *testing.T) {
			instructions, err := Parse(test.trigger)
			if err != nil {
				t.Fatal(err)
			}
			if len(instructions) != 1 || instructions[0].Name != "add" || !reflect.DeepEqual(instructions[0].Properties, test.want) {
				t.Fatalf("parsed ONBUILD ADD = %#v, want properties %#v", instructions, test.want)
			}
		})
	}
}

func TestParseRepeatedENVKeyUsesLastValue(t *testing.T) {
	instructions, err := Parse("ENV A=first A=last B=kept")
	if err != nil {
		t.Fatal(err)
	}
	want := definition.Instruction{Name: "env", Properties: map[string]string{"A": "last", "B": "kept"}}
	if !reflect.DeepEqual(instructions, []definition.Instruction{want}) {
		t.Fatalf("parsed ENV = %#v, want %#v", instructions, want)
	}
}

func TestParseDeferredRetainsQuotesForChildStageExpansion(t *testing.T) {
	const trigger = `ENV MESSAGE="hello $later C:\\tmp path"`
	instructions, err := ParseDeferred(trigger)
	if err != nil {
		t.Fatal(err)
	}
	want := definition.Instruction{Name: "env", Arguments: []string{"MESSAGE", `"hello $later C:\\tmp path"`}, ProcessQuotes: true}
	if !reflect.DeepEqual(instructions, []definition.Instruction{want}) {
		t.Fatalf("deferred ENV = %#v, want %#v", instructions, want)
	}

	instructions, err = Parse(trigger)
	if err != nil {
		t.Fatal(err)
	}
	want = definition.Instruction{Name: "env", Arguments: []string{"MESSAGE", `hello $later C:\tmp path`}}
	if !reflect.DeepEqual(instructions, []definition.Instruction{want}) {
		t.Fatalf("normalized ENV = %#v, want %#v", instructions, want)
	}
}

func TestParseRunPreservesRepeatedDeviceOptions(t *testing.T) {
	instructions, err := Parse(`RUN --device=vendor.example/gpu=one,required --device=vendor.example/gpu=two true`)
	if err != nil {
		t.Fatal(err)
	}
	want := []definition.Instruction{
		{Name: "device", Arguments: []string{"vendor.example/gpu=one"}, Properties: map[string]string{"required": "true"}},
		{Name: "device", Arguments: []string{"vendor.example/gpu=two"}},
	}
	if len(instructions) != 1 || !reflect.DeepEqual(instructions[0].Children, want) {
		t.Fatalf("ONBUILD RUN devices = %#v, want %#v", instructions, want)
	}
}
