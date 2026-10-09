package definition_test

import (
	"reflect"
	"strings"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/onbuildparse"
)

func TestOnBuildMetadataRoundTripsOrdinaryInstructions(t *testing.T) {
	tests := []struct {
		name   string
		child  string
		want   []definition.Instruction
		verify func(*testing.T, string, []definition.Instruction)
	}{
		{name: "arg unset", child: `arg "later"`, want: []definition.Instruction{{Name: "arg", Arguments: []string{"later"}}}},
		{name: "arg empty default", child: `arg "later" ""`, want: []definition.Instruction{{Name: "arg", Arguments: []string{"later", ""}}}},
		{name: "arg quoted deferred default", child: `arg "complex" "hello \"syntax\" \\\"literal\\\" \\$literal C:\\tmp $later ${missing:-\"fallback\"}"`, want: []definition.Instruction{{Name: "arg", Arguments: []string{"complex", `hello "syntax" "literal" $literal C:tmp $later ${missing:-"fallback"}`}}}},
		{name: "env conventional metadata", child: `env "FROM_PARENT" "yes"`, want: []definition.Instruction{{Name: "env", Arguments: []string{"FROM_PARENT", "yes"}}}, verify: func(t *testing.T, raw string, _ []definition.Instruction) {
			if raw != "ENV FROM_PARENT=yes" {
				t.Fatalf("safe ENV metadata = %q, want conventional unquoted form", raw)
			}
		}},
		{name: "env deferred and quoted", child: `env "MESSAGE" "hello \"$later\" C:\\tmp path"`, want: []definition.Instruction{{Name: "env", Arguments: []string{"MESSAGE", `hello "$later" C:tmp path`}}}},
		{name: "label", child: `label z="last value" a="first\\value" message="he said \"ready\""`, want: []definition.Instruction{{Name: "label", Arguments: []string{"a", `firstvalue`}}, {Name: "label", Arguments: []string{"message", `he said "ready"`}}, {Name: "label", Arguments: []string{"z", "last value"}}}},
		{
			name: "run exec flags mounts devices", child: `run network="none" security="insecure" {
      exec "/bin/echo" "$later" "C:\\path with space"
	  mount "secret" id="token" target="/run/a b\\c" required="true" mode="0400"
	  device "vendor.example/gpu=one" "vendor.example/gpu=two" required="true"
	  device "vendor.example/gpu=three,required" required="false"
	  device "vendor.example/gpu=four,required=true" required="false"
	  device "vendor.example/gpu=five,required=false" required="true"
	}`,
			want: []definition.Instruction{{Name: "run", Form: "exec", Arguments: []string{"/bin/echo", "$later", `C:\path with space`}, Properties: map[string]string{"network": "none", "security": "insecure"}, Children: []definition.Instruction{
				{Name: "mount", Arguments: []string{"secret"}, Properties: map[string]string{"id": "token", "target": `/run/a b\c`, "required": "true", "mode": "400"}},
				{Name: "device", Arguments: []string{"vendor.example/gpu=one"}, Properties: map[string]string{"required": "true"}},
				{Name: "device", Arguments: []string{"vendor.example/gpu=two"}, Properties: map[string]string{"required": "true"}},
				{Name: "device", Arguments: []string{"vendor.example/gpu=three"}},
				{Name: "device", Arguments: []string{"vendor.example/gpu=four"}},
				{Name: "device", Arguments: []string{"vendor.example/gpu=five"}, Properties: map[string]string{"required": "true"}},
			}}}},
		{
			name: "copy flags and excludes", child: `copy "a file" "$source" "/dest path/" from="base" chown="1:2" chmod="0755" link="true" parents="true" {
  exclude "*.tmp" "space name"
}`,
			want: []definition.Instruction{{Name: "copy", Arguments: []string{"a file", "$source", "/dest path/"}, Properties: map[string]string{"from": "base", "chown": "1:2", "chmod": "0755", "link": "true", "parents": "true"}, Children: []definition.Instruction{{Name: "exclude", Arguments: []string{"*.tmp", "space name"}}}}},
		},
		{
			name: "add flags and excludes", child: `add "https://example.com/archive.tar" "/src" checksum="sha256:0000000000000000000000000000000000000000000000000000000000000000" keep-git-dir="true" unpack="false" link="true" {
  exclude ".git"
}`,
			want: []definition.Instruction{{Name: "add", Arguments: []string{"https://example.com/archive.tar", "/src"}, Properties: map[string]string{"checksum": "sha256:0000000000000000000000000000000000000000000000000000000000000000", "keep-git-dir": "true", "unpack": "false", "link": "true"}, Children: []definition.Instruction{{Name: "exclude", Arguments: []string{".git"}}}}},
		},
		{name: "cmd exec reset", child: `cmd`, want: []definition.Instruction{{Name: "cmd", Form: "exec", Arguments: []string{}}}},
		{name: "cmd explicit exec reset", child: `cmd { exec }`, want: []definition.Instruction{{Name: "cmd", Form: "exec", Arguments: []string{}}}},
		{name: "entrypoint explicit exec reset", child: `entrypoint { exec }`, want: []definition.Instruction{{Name: "entrypoint", Form: "exec"}}},
		{name: "cmd shell", child: `cmd "echo $later"`, want: []definition.Instruction{{Name: "cmd", Form: "shell", Arguments: []string{"echo $later"}}}},
		{name: "entrypoint exec", child: `entrypoint { exec "/usr/bin/app" "$later" }`, want: []definition.Instruction{{Name: "entrypoint", Form: "exec", Arguments: []string{"/usr/bin/app", "$later"}}}},
		{name: "entrypoint shell", child: `entrypoint "exec /usr/bin/app $later"`, want: []definition.Instruction{{Name: "entrypoint", Form: "shell", Arguments: []string{"exec /usr/bin/app $later"}}}},
		{name: "shell", child: `shell "/bin/bash" "-euxo" "pipefail" "-c"`, want: []definition.Instruction{{Name: "shell", Arguments: []string{"/bin/bash", "-euxo", "pipefail", "-c"}}}},
		{name: "user", child: `user "1000:1000"`, want: []definition.Instruction{{Name: "user", Arguments: []string{"1000:1000"}}}},
		{name: "workdir", child: `workdir "/work dir/$later"`, want: []definition.Instruction{{Name: "workdir", Arguments: []string{"/work dir/$later"}}}},
		{name: "expose", child: `expose "8080/tcp" "8443"`, want: []definition.Instruction{{Name: "expose", Arguments: []string{"8080/tcp"}}, {Name: "expose", Arguments: []string{"8443"}}}},
		{name: "volume", child: `volume "/one path" "/two"`, want: []definition.Instruction{{Name: "volume", Arguments: []string{"/one path"}}, {Name: "volume", Arguments: []string{"/two"}}}},
		{name: "stopsignal", child: `stopsignal "SIGTERM"`, want: []definition.Instruction{{Name: "stopsignal", Arguments: []string{"SIGTERM"}}}},
		{name: "healthcheck exec", child: `healthcheck interval="30s" timeout="3s" start-period="5s" start-interval="1s" retries=4 { exec "/bin/check" "$later" }`, want: []definition.Instruction{{Name: "healthcheck", Arguments: []string{"CMD", "/bin/check", "$later"}, Properties: map[string]string{"interval": "30s", "timeout": "3s", "start-period": "5s", "start-interval": "1s", "retries": "4"}}}},
		{name: "healthcheck shell", child: `healthcheck "test \"$later\" = ready"`, want: []definition.Instruction{{Name: "healthcheck", Arguments: []string{"CMD-SHELL", `test "$later" = ready`}}}},
		{name: "healthcheck none", child: `healthcheck NONE`, want: []definition.Instruction{{Name: "healthcheck", Arguments: []string{"NONE"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, got := parseOnBuildRoundTrip(t, test.child)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("round trip %q = %#v, want %#v (metadata %q)", test.child, got, test.want, raw)
			}
			if test.verify != nil {
				test.verify(t, raw, got)
			}
		})
	}
}

func TestOnBuildMetadataMultilineRunAvoidsDelimiterCollision(t *testing.T) {
	raw, got := parseOnBuildRoundTrip(t, `run """
    printf first
    COOPR_EOF
    printf '$later\\path'
    """`)
	if !strings.Contains(raw, "<<'COOPR_EOF_1'") {
		t.Fatalf("metadata did not choose a collision-free delimiter: %q", raw)
	}
	want := "printf first\nCOOPR_EOF\nprintf '$later\\path'\n"
	if len(got) != 1 || got[0].Name != "run" || got[0].Form != "shell" || !reflect.DeepEqual(got[0].Arguments, []string{want}) {
		t.Fatalf("multiline RUN = %#v, want shell command %q", got, want)
	}
}

func TestOnBuildMetadataKeepsJSONLookingShellRunInShellForm(t *testing.T) {
	_, got := parseOnBuildRoundTrip(t, `run "[\"looks\",\"json\"]"`)
	if len(got) != 1 || got[0].Form != "shell" {
		t.Fatalf("JSON-looking shell RUN = %#v", got)
	}
}

func TestOnBuildMetadataPreservesExecutableMultilineRun(t *testing.T) {
	raw, got := parseOnBuildRoundTrip(t, `run """
    #!/bin/sh
    printf '%s' "$later" >/message
    """`)
	if !strings.Contains(raw, "RUN <<'script'") {
		t.Fatalf("executable RUN metadata = %q", raw)
	}
	if len(got) != 1 || got[0].Form != "exec" || !reflect.DeepEqual(got[0].Arguments, []string{"/run/coopr-heredoc/script"}) || len(got[0].InlineFiles) != 1 || got[0].InlineFiles[0].Path != "script" || got[0].InlineFiles[0].Data != "#!/bin/sh\nprintf '%s' \"$later\" >/message\n" {
		t.Fatalf("executable multiline RUN = %#v", got)
	}
}

func TestOnBuildMetadataRejectsUnknownOrInstructionSpecificProperties(t *testing.T) {
	for _, source := range []string{
		`onbuild { run "true" netwrok="none" }`,
		`onbuild { copy "src" "/dst" cheksum="sha256:bad" }`,
		`onbuild { copy "src" "/dst" checksum="sha256:bad" }`,
		`onbuild { add "src" "/dst" parnets="true" }`,
		`onbuild { add "src" "/dst" from="base" }`,
		`onbuild { shell "/bin/sh" typo="x" }`,
		`onbuild { stopsignal "SIGTERM" typo="x" }`,
		`onbuild { expose 80 typo="x" }`,
		`onbuild { volume "/data" typo="x" }`,
	} {
		_, err := definition.Parse(strings.NewReader(source))
		if err == nil || !strings.Contains(err.Error(), "unsupported property") {
			t.Errorf("Parse(%q) error = %v, want unsupported property", source, err)
		}
	}
}

func parseOnBuildRoundTrip(t *testing.T, child string) (string, []definition.Instruction) {
	t.Helper()
	parsed, err := definition.Parse(strings.NewReader("onbuild {\n" + child + "\n}"))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Instructions) != 1 || len(parsed.Instructions[0].Arguments) != 1 || len(parsed.Instructions[0].Children) != 0 {
		t.Fatalf("compiled ONBUILD = %#v", parsed.Instructions)
	}
	raw := parsed.Instructions[0].Arguments[0]
	got, err := onbuildparse.Parse(raw)
	if err != nil {
		t.Fatalf("parse generated metadata %q: %v", raw, err)
	}
	return raw, got
}
