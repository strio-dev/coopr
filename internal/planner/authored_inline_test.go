package planner

import (
	"reflect"
	"testing"

	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/onbuildparse"
)

func TestAuthoredInlineScopeAndFilename(t *testing.T) {
	plan := makePlan(t, `from "scratch" {
 arg "name" "world"
 arg "filename" "renamed"
 env prefix="Hello"
 copy "/out/" { file "${filename}" "${prefix} '${name}'\n\n"; file "raw" #"${prefix} ${name}"#; file "empty" ""; }
}`, Options{})
	got := plan.Stages[0].Operations[len(plan.Stages[0].Operations)-1].InlineFiles
	want := []definition.InlineFile{{Path: "${filename}", Data: "Hello '${name}'\n\n"}, {Path: "raw", Data: "${prefix} ${name}"}, {Path: "empty"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %#v, want %#v", got, want)
	}
}

func TestAuthoredInlinePackageAndInvocationScopes(t *testing.T) {
	publication := makePlan(t, `arg "fixed" "packaged"
arg "dynamic" "default"
arg "raw" "unused"
arg "filename" "unused"
package as="assets" {
 copy "/out/" { file "${filename}" "${fixed}\n"; file "raw" #"${raw}"#; }
}
extend {
 copy "/out/" "/packaged/" from="assets"
 copy "/out/" { file "dynamic" "${dynamic}\n"; }
}`, Options{Mode: Publish})
	if !reflect.DeepEqual(publication.PackageArguments, map[string]string{"fixed": "packaged"}) {
		t.Fatalf("fixed arguments = %#v", publication.PackageArguments)
	}
	invocation := invokePublished(t, publication, Options{Arguments: map[string]string{"dynamic": "caller", "raw": "changed", "filename": "changed"}})
	found := false
	for _, stage := range invocation.Stages {
		for _, op := range stage.Operations {
			for _, file := range op.InlineFiles {
				if file.Path == "dynamic" {
					found = true
					if file.Data != "caller\n" {
						t.Fatalf("invocation body = %q", file.Data)
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("invocation inline file absent")
	}
	if _, err := Instantiate(publication.Component, Options{Arguments: map[string]string{"fixed": "changed"}}); err == nil {
		t.Fatal("package body argument was not fixed")
	}
}

func TestAuthoredInlineOnBuildExpandsOnlyInChildScope(t *testing.T) {
	doc := parse(t, `from "scratch" { onbuild { copy "/out/" { file "normal" "${name}\n"; file "raw" #"""
${name}

"""#; } }; }`)
	op := doc.Instructions[0].Children[0]
	parent, err := normalize(op, scope{"name": {value: "parent", present: true}})
	if err != nil {
		t.Fatal(err)
	}
	inherited, err := onbuildparse.ParseDeferred(parent.Arguments[0])
	if err != nil {
		t.Fatal(err)
	}
	child, err := normalize(inherited[0], scope{"name": {value: "child", present: true}})
	if err != nil {
		t.Fatal(err)
	}
	want := []definition.InlineFile{{Path: "normal", Data: "child\n"}, {Path: "raw", Data: "${name}\n"}}
	if !reflect.DeepEqual(child.InlineFiles, want) {
		t.Fatalf("child files = %#v", child.InlineFiles)
	}
}

func TestAuthoredInlineBodyExpansionEscapesAndQuotes(t *testing.T) {
	for _, test := range []struct{ body, want string }{
		{`'$name' "$name"`, `'$name' "world"`},
		{`\$name \\$name`, `\$name \\world`},
		{"first\n$name\n\n", "first\nworld\n\n"},
		{`C:\literal`, `C:\literal`},
	} {
		for _, expanding := range []bool{false, true} {
			want := test.body
			if expanding {
				want = test.want
			}
			got, err := normalize(definition.Instruction{Name: "copy", Arguments: []string{"/out"}, InlineFiles: []definition.InlineFile{{Path: "file", Data: test.body, Expand: expanding}}}, scope{"name": {value: "world", present: true}})
			if err != nil {
				t.Fatal(err)
			}
			if got.InlineFiles[0].Data != want {
				t.Errorf("body %q expand %t = %q, want %q", test.body, expanding, got.InlineFiles[0].Data, want)
			}
		}
	}
}

func TestAuthoredInlinePackageBodyDependencyUsesNativeQuotes(t *testing.T) {
	publication := makePlan(t, `arg "unquoted" "used"
arg "quoted" "unused"
arg "escaped" "unused"
package as="assets" { copy "/out" { file "out" "${unquoted} '${quoted}' \\${escaped}"; }; }
extend { copy "/out" "/out" from="assets"; }`, Options{Mode: Publish})
	if !reflect.DeepEqual(publication.PackageArguments, map[string]string{"unquoted": "used"}) {
		t.Fatalf("package arguments = %#v", publication.PackageArguments)
	}
}

func TestAuthoredInlineOnBuildMixedHeaderWords(t *testing.T) {
	doc := parse(t, `from "scratch" { onbuild { copy "has\"quote" "has'quote" "back\\\\slash" "${source}" "/out-${destination}/" { file "inline" "content\n"; }; }; }`)
	inherited, err := onbuildparse.ParseDeferred(doc.Instructions[0].Children[0].Arguments[0])
	if err != nil {
		t.Fatal(err)
	}
	got, err := normalize(inherited[0], scope{"source": {value: "child-source", present: true}, "destination": {value: "child-dest", present: true}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`has"quote`, "has'quote", `back\slash`, "child-source", "/out-child-dest/"}
	if !reflect.DeepEqual(got.Arguments, want) {
		t.Fatalf("mixed header = %#v, want %#v", got.Arguments, want)
	}
}

func TestLiteralShellAndMaintainerDoNotFixPackageArguments(t *testing.T) {
	publication := makePlan(t, `arg "literal_shell" "first"
arg "literal_maintainer" "first"
arg "runtime_run" "fixed-run"
arg "runtime_cmd" "fixed-cmd"
package as="assets" {
 shell "/bin/${literal_shell}" "-c"
 maintainer "${literal_maintainer}"
 cmd "echo ${runtime_cmd}"
}
extend { from "assets" {}; }`, Options{Mode: Publish})
	want := map[string]string{"runtime_cmd": "fixed-cmd"}
	if !reflect.DeepEqual(publication.PackageArguments, want) {
		t.Fatalf("fixed package args = %#v, want %#v", publication.PackageArguments, want)
	}
	if _, err := Instantiate(publication.Component, Options{Arguments: map[string]string{"literal_shell": "changed", "literal_maintainer": "changed"}}); err != nil {
		t.Fatal(err)
	}
}

func TestLiteralConfigCollectorRetainsRuntimeDependencies(t *testing.T) {
	used := map[string]bool{}
	graph := &graph{}
	values := scope{"shell": {value: "literal", present: true}, "maintainer": {value: "literal", present: true}, "run": {value: "needed", present: true}, "cmd": {value: "needed", present: true}}
	for _, name := range []string{"shell", "maintainer", "run", "cmd"} {
		graph.collectArgumentReferences(definition.Instruction{Name: name, Arguments: []string{"${" + name + "}"}}, values, used)
	}
	if !reflect.DeepEqual(used, map[string]bool{"run": true, "cmd": true}) {
		t.Fatalf("dependencies = %#v", used)
	}
}
