package definition

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseInstructions(t *testing.T) {
	source := `
from "debian:bookworm" as="base"
run "echo one"
run """
    echo two
    echo three
    """ {
    mount "cache" target="/tmp/cache"
}
package as="assets"
copy "/out" "/" from="base"
extend as="consumer"
component "registry.example.com/team/setup:v1" channel="preview" target="literal-argument"
`
	def, err := Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Instructions) != 7 {
		t.Fatalf("got %d instructions, want 7", len(def.Instructions))
	}
	if def.Instructions[1].Name != "run" || def.Instructions[2].Name != "run" {
		t.Fatal("repeated instructions lost order")
	}
	if def.Instructions[2].Arguments[0] != "echo two\necho three" {
		t.Fatalf("multiline string = %q", def.Instructions[2].Arguments[0])
	}
	if got := def.Instructions[2].Children[0]; got.Name != "mount" || got.Properties["target"] != "/tmp/cache" {
		t.Fatalf("mount = %#v", got)
	}
	if got := def.Instructions[4]; got.Name != "copy" || got.Properties["from"] != "base" {
		t.Fatalf("copy = %#v", got)
	}
	if got := def.Instructions[6]; len(got.Arguments) != 1 || got.Properties["channel"] != "preview" || got.Properties["target"] != "literal-argument" || len(got.Children) != 0 {
		t.Fatalf("component = %#v", got)
	}
}

func TestComponentArguments(t *testing.T) {
	for _, source := range []string{
		`component "registry.example.com/team/setup:v1" channel="preview"`,
		`component "registry.example.com/team/setup:v1" channel="preview" target="literal-argument"`,
	} {
		def, err := Parse(strings.NewReader(source))
		if err != nil {
			t.Fatal(err)
		}
		if got := def.Instructions[0]; got.Name != "component" || got.Properties["channel"] != "preview" {
			t.Fatalf("component = %#v", got)
		}
	}
}

func TestExtendCompatibilityProperties(t *testing.T) {
	def, err := Parse(strings.NewReader(`extend as="base" distro="${distribution}" distro-version="3.20" package-manager="apk" architecture="amd64"`))
	if err != nil {
		t.Fatal(err)
	}
	if got := def.Instructions[0].Properties; got["distro"] != "${distribution}" || got["distro-version"] != "3.20" || got["package-manager"] != "apk" || got["architecture"] != "amd64" {
		t.Fatalf("compatibility properties lost: %v", got)
	}
	for _, source := range []string{`extend distro-version="3.20"`, `extend distro="alpine" unknown="value"`} {
		if _, err := Parse(strings.NewReader(source)); err == nil {
			t.Fatalf("accepted invalid extend %q", source)
		}
	}
}

func TestExtendCompatibilityLists(t *testing.T) {
	def, err := Parse(strings.NewReader(`
extend as="base" {
  distro "fedora" "rhel"
  distro-version "41" "42"
  package-manager "dnf" "yum"
  architecture "amd64" "arm64"
}
`))
	if err != nil {
		t.Fatal(err)
	}
	children := def.Instructions[0].Children
	want := []Instruction{
		{Name: "distro", Arguments: []string{"fedora", "rhel"}},
		{Name: "distro-version", Arguments: []string{"41", "42"}},
		{Name: "package-manager", Arguments: []string{"dnf", "yum"}},
		{Name: "architecture", Arguments: []string{"amd64", "arm64"}},
	}
	if !reflect.DeepEqual(children, want) {
		t.Fatalf("compatibility lists = %#v, want %#v", children, want)
	}
}

func TestExtendCompatibilityListRejectsDuplicateDeclarations(t *testing.T) {
	tests := []struct {
		source string
		want   string
	}{
		{`extend distro="fedora" { distro "rhel" }`, `duplicate compatibility requirement "distro"`},
		{`extend { architecture "amd64"; architecture "arm64" }`, `duplicate compatibility requirement "architecture"`},
		{`extend { distro-version "41" }`, "distro-version requires distro"},
		{`extend { distro }`, "needs at least one value"},
		{`extend { distro "fedora" unsupported="value" }`, "only string values"},
		{`extend { kernel "6" }`, `unknown compatibility requirement "kernel"`},
	}
	for _, test := range tests {
		if _, err := Parse(strings.NewReader(test.source)); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("Parse(%q) error = %v, want %q", test.source, err, test.want)
		}
	}
}

func TestParseExamples(t *testing.T) {
	count := 0
	err := filepath.WalkDir("../../examples", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".coopr" {
			return nil
		}
		count++
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(strings.NewReader(string(data))); err != nil {
				t.Fatal(err)
			}
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("no example definitions found")
	}
}

func TestComponentSymbolicReference(t *testing.T) {
	for _, source := range []string{
		"extend\narg \"inner\"\ncomponent \"${inner}\"\n",
		"extend\ncomponent \"$inner\"\n",
		"from \"scratch\"\ncomponent \"sha256:8a3f2ec95696c56dbeeb55c6ae2e7a0583d7d97ce72cd258774d814ad214aaa7\"\n",
		"from \"scratch\"\ncomponent \"local:company-config\"\n",
	} {
		if _, err := Parse(strings.NewReader(source)); err != nil {
			t.Fatalf("parse symbolic component %q: %v", source, err)
		}
	}
}

func TestInvalidDefinitions(t *testing.T) {
	tests := []struct{ name, source, want string }{
		{"empty", "// empty\n", "empty Coopr definition"},
		{"syntax", "run \"unterminated\n", "parse KDL v2"},
		{"invalid instruction name", "Nope \"x\"", "invalid instruction name"},
		{"arity", "from", "expected one source"},
		{"null argument", "run #null", "null is not supported"},
		{"null property", "from \"base\" platform=#null", "null is not supported"},
		{"nonfinite argument", "run #inf", "finite"},
		{"node annotation", "(exec)run \"true\"", "annotations are not supported"},
		{"argument annotation", "run (word)\"true\"", "annotations are not supported"},
		{"property annotation", "run \"true\" network=(mode)\"none\"", "annotations are not supported"},
		{"string shell selector", `run "/bin/true" shell="false"`, "shell property is not supported"},
		{"package name", "package", "nonempty as"},
		{"component child", "component \"registry.example.com/a:v1\" { with channel=\"x\" }", "children are only allowed"},
		{"component positional target", `component "registry.example.com/a:v1" "debug"`, "expected one component reference"},
		{"too many component arguments", `component "registry.example.com/a:v1" "debug" "extra"`, "expected one component reference"},
		{"mount child", "run \"x\" { mount \"cache\" target=\"/tmp\" { foo } }", "children are only allowed"},
		{"invalid mount", "run \"x\" { mount \"\" }", "mount type"},
		{"run arity", `run`, "expected one shell command"},
		{"empty chmod", `copy "a" "b" chmod=""`, "chmod property cannot be empty"},
		{"hidden run mount property", `run "x" mount="type=cache,target=/tmp"`, "use a mount child"},
		{"generic option child", `run "x" { option "mount" "type=bind,from=package" }`, "unsupported child"},
		{"injected mount type", `run "x" { mount "cache,from=package" target="/tmp" }`, "mount type"},
		{"injected mount key", `run "x" { mount "cache" "target,from"="/tmp" }`, "invalid mount property name"},
		{"label equals", `label "a=b" "value"`, "nonempty key without equals sign"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.source))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestStandardOptionsDeferToFrontend(t *testing.T) {
	parsed, err := Parse(strings.NewReader(`run "true" network="host" security="sandbox"`))
	if err != nil {
		t.Fatal(err)
	}
	inst := parsed.Instructions[0]
	if err := ValidateResolved(inst); err != nil {
		t.Fatal(err)
	}
}

func TestCommandFormsAndDirectRepeatedOptions(t *testing.T) {
	def, err := Parse(strings.NewReader(`
run { exec "/bin/echo" "hello" }
cmd "echo $HOME"
entrypoint { exec "/usr/bin/app" }
copy "src" "/dst" { exclude "*.md" "*.tmp" }
run "true" { device "vendor.example/gpu=one" "vendor.example/gpu=two" required=#true }
`))
	if err != nil {
		t.Fatal(err)
	}
	if def.Instructions[0].Form != "exec" || def.Instructions[1].Form != "shell" || def.Instructions[2].Form != "exec" {
		t.Fatalf("command forms = %#v", def.Instructions[:3])
	}
	if got := def.Instructions[3].Children; !reflect.DeepEqual(got, []Instruction{{Name: "exclude", Arguments: []string{"*.md", "*.tmp"}}}) {
		t.Fatalf("exclude children = %#v", got)
	}
	if got := def.Instructions[4].Children; !reflect.DeepEqual(got, []Instruction{{Name: "device", Arguments: []string{"vendor.example/gpu=one", "vendor.example/gpu=two"}, Properties: map[string]string{"required": "true"}}}) {
		t.Fatalf("device children = %#v", got)
	}
}

func TestNativeScalarNormalization(t *testing.T) {
	def, err := Parse(strings.NewReader(`
run "true" network="none" retry=3 enabled=#true ratio=1.5
copy "src" "/dst" chmod="0755"
run "true" { mount "secret" target="/run/secret" mode="0400" required=#true uid=1000 }
env "ENABLED" #true
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := def.Instructions[0]; got.Form != "shell" || got.Properties["retry"] != "3" || got.Properties["enabled"] != "true" || got.Properties["ratio"] != "1.5" {
		t.Fatalf("normalized RUN = %#v", got)
	}
	if got := def.Instructions[1].Properties["chmod"]; got != "0755" {
		t.Fatalf("chmod = %q, want 0755", got)
	}
	if got := def.Instructions[2].Children[0].Properties; got["mode"] != "0400" || got["required"] != "true" || got["uid"] != "1000" {
		t.Fatalf("mount properties = %#v", got)
	}
	if got := def.Instructions[3].Arguments; !reflect.DeepEqual(got, []string{"ENABLED", "true"}) {
		t.Fatalf("native argument = %#v", got)
	}
}

func TestRemovedAuthoredInlineAndOptionSyntax(t *testing.T) {
	for _, source := range []string{
		`(inline)copy "content" "/file"`,
		`copy "/target" { inline "file" "data" }`,
		`add "/target" { inline "file" "data" }`,
		`copy "src" "/target" { option "exclude" "*.txt" }`,
		`run "true" { option "device" "vendor.example/gpu" }`,
		`run "true" { device "vendor.example/gpu" required="yes" }`,
	} {
		if _, err := Parse(strings.NewReader(source)); err == nil {
			t.Errorf("accepted removed authored syntax %q", source)
		}
	}
}

func TestMultilineShebangRunBecomesExecutableInlineFile(t *testing.T) {
	def, err := Parse(strings.NewReader(`
run """
    #!/bin/sh
    printf hello >/message
    """
`))
	if err != nil {
		t.Fatal(err)
	}
	got := def.Instructions[0]
	want := Instruction{
		Name: "run", Form: "exec", Arguments: []string{"/run/coopr-heredoc/script"},
		InlineFiles: []InlineFile{{Path: "script", Data: "#!/bin/sh\nprintf hello >/message"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shebang RUN = %#v, want %#v", got, want)
	}
}

func TestValidateRejectsUnsafeInternalInlineFilePaths(t *testing.T) {
	for _, path := range []string{"../escape", "/absolute", "."} {
		def := &Definition{Instructions: []Instruction{{
			Name: "copy", Arguments: []string{"/target"}, InlineFiles: []InlineFile{{Path: path, Data: "payload"}},
		}}}
		if err := Validate(def); err == nil || !strings.Contains(err.Error(), "inline file path") {
			t.Fatalf("Validate inline path %q error = %v", path, err)
		}
	}
}

func TestValidateAcceptsImportedInlineFiles(t *testing.T) {
	def := &Definition{Instructions: []Instruction{
		{Name: "copy", Arguments: []string{"/target"}, InlineFiles: []InlineFile{{Path: "message", Data: "payload"}}},
		{Name: "run", Form: "exec", Arguments: []string{"/run/coopr-heredoc/script"}, InlineFiles: []InlineFile{{Path: "script", Data: "#!/bin/sh\ntrue"}}},
	}}
	if err := Validate(def); err != nil {
		t.Fatal(err)
	}
}

func TestHealthcheckAndOnBuildSyntax(t *testing.T) {
	def, err := Parse(strings.NewReader(`
healthcheck interval="30s" timeout="3s" start-period="5s" start-interval="1s" retries=4 { exec "/bin/check" "--ready" }
healthcheck "curl -f http://localhost/ || exit 1"
healthcheck NONE
healthcheck "NONE"
healthcheck { exec "NONE" }
onbuild { run "make generated" }
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := def.Instructions[0]; got.Name != "healthcheck" || len(got.Arguments) != 3 || got.Properties["start-interval"] != "1s" {
		t.Fatalf("healthcheck = %#v", got)
	}
	if got := def.Instructions[0]; !reflect.DeepEqual(got.Arguments, []string{"CMD", "/bin/check", "--ready"}) || got.Properties["retries"] != "4" {
		t.Fatalf("exec healthcheck = %#v", got)
	}
	if got := def.Instructions[1]; !reflect.DeepEqual(got.Arguments, []string{"CMD-SHELL", "curl -f http://localhost/ || exit 1"}) {
		t.Fatalf("shell healthcheck = %#v", got)
	}
	if got := def.Instructions[2]; !reflect.DeepEqual(got.Arguments, []string{"NONE"}) {
		t.Fatalf("disabled healthcheck = %#v", got)
	}
	if got := def.Instructions[3]; !reflect.DeepEqual(got.Arguments, []string{"CMD-SHELL", "NONE"}) {
		t.Fatalf("NONE shell command = %#v", got)
	}
	if got := def.Instructions[4]; !reflect.DeepEqual(got.Arguments, []string{"CMD", "NONE"}) {
		t.Fatalf("NONE exec command = %#v", got)
	}
	if got := def.Instructions[5]; got.Name != "onbuild" || !reflect.DeepEqual(got.Arguments, []string{"RUN make generated"}) || len(got.Children) != 0 {
		t.Fatalf("onbuild = %#v", got)
	}
}

func TestInvalidHealthcheckAndOnBuildSyntax(t *testing.T) {
	tests := []struct{ source, want string }{
		{`healthcheck`, "CMD, CMD-SHELL, or NONE"},
		{`healthcheck NONE interval="1s"`, "does not accept properties"},
		{`healthcheck NONE retries=3`, "does not accept properties"},
		{`healthcheck "one" "two"`, "exactly one command"},
		{`healthcheck "true" bogus="1s"`, "supported timing properties"},
		{`healthcheck "true" retries="three"`, "nonnegative integer"},
		{`healthcheck "true" interval="now"`, "duration"},
		{`healthcheck "true" interval="1us"`, "less than 1ms"},
		{`healthcheck "true" start-period="-1s"`, "less than 1ms"},
		{`healthcheck NONE { exec "/check" }`, "cannot combine"},
		{`onbuild`, "exactly one child"},
		{`onbuild "RUN true"`, "exactly one child"},
		{`onbuild { run "true"; copy "a" "b" }`, "exactly one child"},
		{`onbuild extra="no" { run "true" }`, "no arguments or properties"},
	}
	for _, test := range tests {
		_, err := Parse(strings.NewReader(test.source))
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("Parse(%q) error = %v, want containing %q", test.source, err, test.want)
		}
	}
	for _, source := range []string{`healthcheck "true" interval="0s" retries=4`, `healthcheck "true" start-period="1ms"`} {
		if _, err := Parse(strings.NewReader(source)); err != nil {
			t.Errorf("Parse(%q): %v", source, err)
		}
	}
}

func TestCommandResetForms(t *testing.T) {
	for _, source := range []string{"cmd", "entrypoint", `cmd { exec }`, `entrypoint { exec }`} {
		def, err := Parse(strings.NewReader(source))
		if err != nil {
			t.Fatalf("parse %q: %v", source, err)
		}
		if got := def.Instructions[0]; len(got.Arguments) != 0 || got.Form != "exec" {
			t.Fatalf("parse %q arguments = %#v, want empty reset", source, got.Arguments)
		}
		if err := Validate(def); err != nil {
			t.Fatalf("validate %q: %v", source, err)
		}
	}

	for _, source := range []string{`cmd "one" "two"`, `entrypoint "one" "two"`} {
		if _, err := Parse(strings.NewReader(source)); err == nil || !strings.Contains(err.Error(), "one shell command") {
			t.Fatalf("parse %q error = %v, want shell-form arity error", source, err)
		}
	}
}

func TestParameterizedMountTypeCheckedAfterResolution(t *testing.T) {
	def, err := Parse(strings.NewReader(`run "true" { mount "${mount_type}" target="/cache" }`))
	if err != nil {
		t.Fatal(err)
	}
	inst := def.Instructions[0]
	inst.Children[0].Arguments[0] = "cache"
	if err := ValidateResolved(inst); err != nil {
		t.Fatal(err)
	}
	inst.Children[0].Arguments[0] = "cache,from=hidden"
	if err := ValidateResolved(inst); err == nil || !strings.Contains(err.Error(), "mount type") {
		t.Fatalf("invalid resolved mount type: %v", err)
	}
}

func TestValidateDecodedDefinition(t *testing.T) {
	tests := []struct {
		name string
		def  *Definition
		want string
	}{
		{"nil", nil, "empty Coopr definition"},
		{"empty", &Definition{}, "empty Coopr definition"},
		{"missing from source", &Definition{Instructions: []Instruction{{Name: "from"}}}, "expected one source"},
		{"missing component reference", &Definition{Instructions: []Instruction{{Name: "component"}}}, "expected one component reference"},
		{"invalid child", &Definition{Instructions: []Instruction{{Name: "run", Arguments: []string{"true"}, Children: []Instruction{{Name: "copy", Arguments: []string{"a", "b"}}}}}}, "unsupported child"},
		{"invalid mount", &Definition{Instructions: []Instruction{{Name: "run", Arguments: []string{"true"}, Children: []Instruction{{Name: "mount", Arguments: []string{""}}}}}}, "mount type"},
		{"inline form", &Definition{Instructions: []Instruction{{Name: "copy", Form: "inline", Arguments: []string{"payload", "/target"}}}}, "unknown instruction form"},
		{"nested mount", &Definition{Instructions: []Instruction{{Name: "run", Arguments: []string{"true"}, Children: []Instruction{{Name: "mount", Arguments: []string{"cache"}, Properties: map[string]string{"target": "/tmp"}, Children: []Instruction{{Name: "mount"}}}}}}}, "children are only allowed"},
		{"empty env key", &Definition{Instructions: []Instruction{{Name: "env", Arguments: []string{"", "value"}}}}, "nonempty key"},
		{"invalid env key", &Definition{Instructions: []Instruction{{Name: "env", Arguments: []string{"A=B", "value"}}}}, "nonempty key"},
		{"nul env value", &Definition{Instructions: []Instruction{{Name: "env", Properties: map[string]string{"A": "a\x00b"}}}}, "cannot contain NUL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.def)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v; want %q", err, tt.want)
			}
		})
	}
	valid := &Definition{Instructions: []Instruction{
		{Name: "extend"},
		{Name: "run", Form: "shell", Arguments: []string{"true"}, Children: []Instruction{
			{Name: "mount", Arguments: []string{"cache"}, Properties: map[string]string{"target": "/tmp"}},
			{Name: "device", Arguments: []string{"vendor.example/gpu"}, Properties: map[string]string{"required": "true"}},
		}},
		{Name: "copy", Arguments: []string{"src", "/dst"}, Children: []Instruction{{Name: "exclude", Arguments: []string{"*.tmp"}}}},
		{Name: "onbuild", Arguments: []string{"RUN true"}},
	}}
	if err := Validate(valid); err != nil {
		t.Fatal(err)
	}
}
