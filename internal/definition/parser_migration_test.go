package definition

import (
	"reflect"
	"strings"
	"testing"
)

func TestParserMigrationNativeOctalPermissions(t *testing.T) {
	for _, tc := range []struct {
		name, source, property, want string
	}{
		{"copy newline", "copy \"app\" \"/app\" chmod=0o755\n", "chmod", "0755"},
		{"add semicolon", `add "app" "/app" chmod=0o755;`, "chmod", "0755"},
		{"copy line comment", "copy \"app\" \"/app\" chmod=0o755// mode\n", "chmod", "0755"},
		{"copy block comment", `copy "app" "/app" chmod=0o755/* mode */;`, "chmod", "0755"},
		{"layer closing brace", `layer { copy "app" "/app" chmod=0o755}`, "chmod", "0755"},
		{"mount closing brace", `run "true" { mount "secret" target="/run/secret" mode=0o400}`, "mode", "0400"},
		{"mount semicolon", `run "true" { mount "secret" target="/run/secret" mode=0o400; }`, "mode", "0400"},
		{"mount block comment", `run "true" { mount "secret" target="/run/secret" mode=0o400/* mode */; }`, "mode", "0400"},
		{"zero permission", `copy "app" "/app" chmod=0o0;`, "chmod", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, err := Parse(strings.NewReader(tc.source))
			if err != nil {
				t.Fatal(err)
			}
			inst := def.Instructions[0]
			if len(inst.Children) != 0 {
				inst = inst.Children[0]
			}
			if got := inst.Properties[tc.property]; got != tc.want {
				t.Fatalf("%s = %q, want %q", tc.property, got, tc.want)
			}
		})
	}
}

func TestParserMigrationNumericArgumentsAndContinuations(t *testing.T) {
	for _, tc := range []struct {
		name, source, want string
	}{
		{"octal default", "arg \"count\" 0o755\n", "493"},
		{"octal semicolon", `arg "count" 0o755;`, "493"},
		{"hex semicolon", `arg "count" 0x10;`, "16"},
		{"binary comment", "arg \"count\" 0b10// count\n", "2"},
		{"decimal continuation", "arg \"count\" 3\\\n", "3"},
		{"octal continuation", "arg \"count\" 0o755\\\n", "493"},
		{"continuation with comment", "arg \"count\" 3\\ // count\n", "3"},
		// KDL v2 permits EOF as the terminator of an escaped line.
		{"continuation at eof", "arg \"count\" 3\\ ", "3"},
		{"quoted permission default", `arg "count" "0755"`, "0755"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, err := Parse(strings.NewReader(tc.source))
			if err != nil {
				t.Fatal(err)
			}
			if got := def.Instructions[0].Arguments; !reflect.DeepEqual(got, []string{"count", tc.want}) {
				t.Fatalf("arguments = %#v, want count and %q", got, tc.want)
			}
		})
	}
}

func TestParserMigrationRejectsMalformedContinuations(t *testing.T) {
	for _, source := range []string{
		"arg \"count\" 3\\x\n",
		"arg \"count\" 3\\;\n",
	} {
		t.Run(source, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(source)); err == nil {
				t.Fatal("Parse accepted a malformed line continuation")
			}
		})
	}
}

func TestParserMigrationPreservesQuotedHealthcheckNONE(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"escaped quoted", `healthcheck "\u{4e}ONE"`},
		{"raw quoted", `healthcheck #"NONE"#`},
		{"multiline quoted", "healthcheck \"\"\"\n    NONE\n    \"\"\"\n"},
		{"raw multiline quoted", "healthcheck #\"\"\"\n    NONE\n    \"\"\"#\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, err := Parse(strings.NewReader(tc.source))
			if err != nil {
				t.Fatal(err)
			}
			if got := def.Instructions[0].Arguments; !reflect.DeepEqual(got, []string{"CMD-SHELL", "NONE"}) {
				t.Fatalf("quoted NONE = %#v, want shell command", got)
			}
		})
	}
}

func TestParserMigrationPreservesShebangStringForms(t *testing.T) {
	for _, tc := range []struct {
		name, source, script string
		inline               bool
	}{
		{
			name: "raw multiline",
			source: `run #"""
    #!/bin/sh
    printf '%s\n' "hello"
    """#`,
			script: "#!/bin/sh\nprintf '%s\\n' \"hello\"",
			inline: true,
		},
		{
			name:   "escaped single line",
			source: `run "\u{23}!/bin/sh\nprintf hello"`,
			script: "#!/bin/sh\nprintf hello",
		},
		{
			name:   "raw single line",
			source: `run #"#!/bin/sh\nprintf hello"#`,
			script: `#!/bin/sh\nprintf hello`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, err := Parse(strings.NewReader(tc.source))
			if err != nil {
				t.Fatal(err)
			}
			want := Instruction{Name: "run", Form: "shell", Arguments: []string{tc.script}}
			if tc.inline {
				want.Form = "exec"
				want.Arguments = []string{"/run/coopr-heredoc/script"}
				want.InlineFiles = []InlineFile{{Path: "script", Data: tc.script}}
			}
			if got := def.Instructions[0]; !reflect.DeepEqual(got, want) {
				t.Fatalf("RUN = %#v, want %#v", got, want)
			}
		})
	}
}

func TestParserMigrationV2BooleansAndParameterizedOptions(t *testing.T) {
	def, err := Parse(strings.NewReader(`
component "example.com/test:latest" enabled=#true disabled=#false count=0o755
run "true" { mount "secret" target="/run/secret" required=#true mode="${mode:-0400}" }
copy "app" "/app" chmod="${mode:-0755}" link="${link:-false}"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := def.Instructions[0].Properties; !reflect.DeepEqual(got, map[string]string{"enabled": "true", "disabled": "false", "count": "493"}) {
		t.Fatalf("component parameters = %#v", got)
	}
	if got := def.Instructions[1].Children[0].Properties; got["required"] != "true" || got["mode"] != "${mode:-0400}" {
		t.Fatalf("mount properties = %#v", got)
	}
	if got := def.Instructions[2].Properties; got["chmod"] != "${mode:-0755}" || got["link"] != "${link:-false}" {
		t.Fatalf("COPY properties = %#v", got)
	}
	for _, source := range []string{
		`component "example.com/test:latest" enabled=true`,
		`component "example.com/test:latest" enabled=false`,
		`run "true" { mount "secret" target="/run/secret" required=true }`,
	} {
		if _, err := Parse(strings.NewReader(source)); err == nil {
			t.Errorf("Parse(%q) accepted a KDL v1 boolean", source)
		}
	}
}
