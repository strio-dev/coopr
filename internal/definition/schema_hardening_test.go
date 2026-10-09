package definition

import (
	"strings"
	"testing"
)

func TestQuotedPermissionFormatting(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"quoted mode", `copy "app" "/app" chmod="0755";`},
		{"quoted add mode", `add "app" "/app" chmod="0755";`},
		{"quoted mode newline", "copy \"app\" \"/app\" chmod=\"0755\"\n"},
		{"quoted mode line comment", "copy \"app\" \"/app\" chmod=\"0755\"// mode\n"},
		{"quoted mode block comment", `copy "app" "/app" chmod="0755"/* mode */;`},
		{"quoted mode closing brace", `layer { copy "app" "/app" chmod="0755"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, err := Parse(strings.NewReader(tc.source))
			if err != nil {
				t.Fatal(err)
			}
			inst := def.Instructions[0]
			if inst.Name == "layer" {
				inst = inst.Children[0]
			}
			if got := inst.Properties["chmod"]; got != "0755" {
				t.Fatalf("chmod = %q, want 0755", got)
			}
		})
	}
}

func TestQuotedPermissionDefaultsRemainParameterized(t *testing.T) {
	def, err := Parse(strings.NewReader(`
arg "mode" "0755"
copy "app" "/app" chmod="${mode}"
add "app" "/other-app" chmod="${mode:-0755}"
run "true" { mount "secret" target="/run/secret" mode="${secret_mode:-0400}" }
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := def.Instructions[0].Arguments[1]; got != "0755" {
		t.Fatalf("ARG default = %q, want 0755", got)
	}
	if got := def.Instructions[1].Properties["chmod"]; got != "${mode}" {
		t.Fatalf("COPY chmod = %q, want preserved parameter", got)
	}
	if got := def.Instructions[2].Properties["chmod"]; got != "${mode:-0755}" {
		t.Fatalf("ADD chmod = %q, want preserved default expression", got)
	}
	if got := def.Instructions[3].Children[0].Properties["mode"]; got != "${secret_mode:-0400}" {
		t.Fatalf("mount mode = %q, want preserved default expression", got)
	}
}

func TestSchemaRejectsStaticTypos(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{`from "scratch" as="unused"; frobnicate "x"; from "scratch"`, `unknown instruction "frobnicate"`},
		{`run "true" netwrok="none"`, `unsupported property "netwrok"`},
		{`copy "a" "/a" parnets=#true`, `unsupported property "parnets"`},
		{`from "scratch" platfrom="linux/amd64"`, `unsupported property "platfrom"`},
		{`RUN "true"`, `use "run"`},
		{`run "true" { mount "cahce" target="/tmp" }`, `mount type "cahce"`},
		{`run "true" { mount "cache" targte="/tmp" }`, `unsupported property "targte"`},
		{`run "true" { mount "$kind" targte="/tmp" }`, `unsupported property "targte"`},
		{`shell`, `at least one argument`},
		{`stopsignal "TERM" typo="x"`, `unsupported property "typo"`},
	} {
		t.Run(tc.source, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.source))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v; want %q", err, tc.want)
			}
		})
	}
}

func TestSchemaAllowsDeferredValues(t *testing.T) {
	for _, source := range []string{
		`run "true" { device "example.com/gpu=all" required="${required}" }`,
		`run "true" { mount "$kind" target="/tmp" }`,
		`run "true" { mount "${kind:-cache}" target="/tmp" }`,
		`run "true" { mount "bind" target="/tmp" U=#true Z=#true }`,
		`healthcheck "true" interval="${interval}" retries="${retries}"`,
	} {
		if _, err := Parse(strings.NewReader(source)); err != nil {
			t.Errorf("Parse(%q): %v", source, err)
		}
	}
}

func TestResolvedSchemaRejectsUnresolvedTypedValues(t *testing.T) {
	for _, inst := range []Instruction{
		{Name: "healthcheck", Arguments: []string{"CMD-SHELL", "true"}, Properties: map[string]string{"interval": "$invalid"}},
		{Name: "healthcheck", Arguments: []string{"CMD-SHELL", "true"}, Properties: map[string]string{"retries": "$invalid"}},
		{Name: "run", Arguments: []string{"true"}, Children: []Instruction{{Name: "device", Arguments: []string{"example.com/gpu"}, Properties: map[string]string{"required": "$invalid"}}}},
		{Name: "run", Arguments: []string{"true"}, Children: []Instruction{{Name: "mount", Arguments: []string{"$invalid"}}}},
	} {
		if err := ValidateResolved(inst); err == nil {
			t.Errorf("ValidateResolved(%#v) unexpectedly succeeded", inst)
		}
	}
}

func TestSchemaPropertyErrorsAreDeterministic(t *testing.T) {
	for range 30 {
		_, err := Parse(strings.NewReader(`run "true" zzz=#true aaa=#true`))
		if err == nil || !strings.Contains(err.Error(), `unsupported property "aaa"`) {
			t.Fatalf("error = %v", err)
		}
	}
}

func TestPublishedSchemaRejectsStaticTypos(t *testing.T) {
	for _, inst := range []Instruction{
		{Name: "frobnicate", Arguments: []string{"x"}},
		{Name: "run", Arguments: []string{"true"}, Properties: map[string]string{"netwrok": "none"}},
		{Name: "layer", Children: []Instruction{{Name: "copy", Arguments: []string{"a", "/a"}, Properties: map[string]string{"parnets": "true"}}}},
	} {
		if err := Validate(&Definition{Instructions: []Instruction{inst}}); err == nil {
			t.Errorf("Validate(%#v) succeeded", inst)
		}
	}
}

func TestSchemaLiteralEnumsAndDeferredEnums(t *testing.T) {
	for _, source := range []string{
		`run "true" security="typo"`,
		`run "true" { mount "cache" sharing="typo" target="/cache" }`,
		`run "true" { mount "bind" relabel="typo" target="/src" }`,
		`run "true" { mount "bind" bind-propagation="typo" target="/src" }`,
	} {
		if _, err := Parse(strings.NewReader(source)); err == nil {
			t.Errorf("Parse(%q) succeeded", source)
		}
	}
	for _, source := range []string{
		`run "true" security="${security}"`,
		`run "true" { mount "cache" sharing="${sharing}" target="/cache" }`,
		`run "true" { mount "bind" relabel="${relabel}" target="/src" }`,
	} {
		if _, err := Parse(strings.NewReader(source)); err != nil {
			t.Errorf("Parse(%q): %v", source, err)
		}
	}
}
