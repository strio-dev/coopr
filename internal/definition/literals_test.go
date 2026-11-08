package definition

import (
	"reflect"
	"strings"
	"testing"
)

func TestHealthcheckNONEKeyword(t *testing.T) {
	def, err := parseFragment(strings.NewReader(`
healthcheck NONE
healthcheck "NONE"
healthcheck { exec "NONE" }
onbuild { healthcheck NONE }
healthcheck "\u{4e}ONE"
healthcheck #"NONE"#
healthcheck """
    NONE
    """
healthcheck #"""
    NONE
    """#
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Instruction{
		{Name: "healthcheck", Arguments: []string{"NONE"}},
		{Name: "healthcheck", Arguments: []string{"CMD-SHELL", "NONE"}},
		{Name: "healthcheck", Arguments: []string{"CMD", "NONE"}},
		{Name: "onbuild", Arguments: []string{"HEALTHCHECK NONE"}},
		{Name: "healthcheck", Arguments: []string{"CMD-SHELL", "NONE"}},
		{Name: "healthcheck", Arguments: []string{"CMD-SHELL", "NONE"}},
		{Name: "healthcheck", Arguments: []string{"CMD-SHELL", "NONE"}},
		{Name: "healthcheck", Arguments: []string{"CMD-SHELL", "NONE"}},
	}
	if !reflect.DeepEqual(def.Instructions, want) {
		t.Fatalf("healthcheck forms = %#v, want %#v", def.Instructions, want)
	}
	if err := validateFragment(def); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{
		`healthcheck NONE "extra"`,
		`healthcheck NONE { exec "/check" }`,
		`healthcheck #null`,
		`healthcheck { exec #null }`,
	} {
		if _, err := parseFragment(strings.NewReader(source)); err == nil {
			t.Errorf("accepted invalid healthcheck %s", source)
		}
	}
}

func TestQuotedBooleanOptionsMatchNativeValues(t *testing.T) {
	for _, source := range []string{
		`copy "src" "/dst" link="true" parents="false"`,
		`add "archive.tar" "/dst" unpack="false" keep-git-dir="true"`,
		`run "true" { mount "bind" target="/input" readonly="true" }`,
		`run "true" { mount "secret" id="token" required="false" }`,
		`run "true" { device "vendor.example/gpu=one" required="true" }`,
		`run "true" { device "vendor.example/gpu=one" required="false" }`,
	} {
		quoted, err := parseFragment(strings.NewReader(source))
		if err != nil {
			t.Fatal(err)
		}
		nativeSource := strings.NewReplacer(`="true"`, `=#true`, `="false"`, `=#false`).Replace(source)
		native, err := parseFragment(strings.NewReader(nativeSource))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(quoted, native) {
			t.Fatalf("quoted options differ from native values for %s", source)
		}
		if err := validateFragment(quoted); err != nil {
			t.Fatal(err)
		}
	}
	for _, source := range []string{
		`copy "src" "/dst" link=true`,
		`copy "src" "/dst" link=false`,
		`run "true" { device "vendor.example/gpu=one" required="yes" }`,
		`run "true" { device "vendor.example/gpu=one" required=1 }`,
		`run "true" { device "vendor.example/gpu=one" required=(flag)"true" }`,
	} {
		if _, err := parseFragment(strings.NewReader(source)); err == nil {
			t.Errorf("accepted invalid boolean option %s", source)
		}
	}
}

func TestPermissionLiterals(t *testing.T) {
	for _, tc := range []struct{ source, property, want string }{
		{`copy "app" "/app" chmod=0o755`, "chmod", "0755"},
		{`add "app" "/app" chmod=0o755`, "chmod", "0755"},
		{`run "true" { mount "secret" target="/run/secret" mode=0o400 }`, "mode", "0400"},
		{`copy "app" "/app" chmod=0o0`, "chmod", "0"},
		{`copy "app" "/app" chmod="0755"`, "chmod", "0755"},
		{`add "app" "/app" chmod="0755"`, "chmod", "0755"},
	} {
		t.Run(tc.source, func(t *testing.T) {
			def, err := parseFragment(strings.NewReader(tc.source))
			if err != nil {
				t.Fatal(err)
			}
			inst := def.Instructions[0]
			if inst.Name == "run" {
				inst = inst.Children[0]
			}
			if got := inst.Properties[tc.property]; got != tc.want {
				t.Fatalf("%s = %q, want %q", tc.property, got, tc.want)
			}
		})
	}
}

func TestRunShebangStringForms(t *testing.T) {
	for _, tc := range []struct {
		name, source, script string
		inline               bool
	}{
		{
			name: "multiline",
			source: `run """
    #!/bin/sh
    printf hello >/message
    """`,
			script: "#!/bin/sh\nprintf hello >/message",
			inline: true,
		},
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
			def, err := parseFragment(strings.NewReader(tc.source))
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
