package definition_test

import (
	"strings"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/onbuildparse"
)

func TestOnBuildFlagsRequireLiteralsWhenDockerfileCannotExpandThem(t *testing.T) {
	for _, property := range []string{"interval", "timeout", "start-period", "start-interval", "retries"} {
		t.Run(property, func(t *testing.T) {
			_, err := definition.Parse(strings.NewReader(`onbuild { healthcheck "true" ` + property + `="${value}" }`))
			if err == nil || !strings.Contains(err.Error(), property) || !strings.Contains(err.Error(), "literal") {
				t.Fatalf("error = %v, want named literal-required ONBUILD flag", err)
			}
		})
	}
	_, err := definition.Parse(strings.NewReader(`onbuild { run "true" { device "vendor.example/gpu=one" required="${required}" } }`))
	if err == nil || !strings.Contains(err.Error(), "required") || !strings.Contains(err.Error(), "literal") {
		t.Fatalf("error = %v, want literal-required device flag", err)
	}
}

func TestOnBuildMetadataRejectsFlagsThatFailDockerfileParsing(t *testing.T) {
	for _, child := range []string{
		`copy "src" "/dst" link="${linked}"`,
		`copy "src" "/dst" parents="${parents}"`,
		`add "src" "/dst" keep-git-dir="${keep}"`,
		`add "src" "/dst" unpack="${unpack}"`,
		`run "true" network="${network}"`,
		`run "true" security="${security}"`,
		`healthcheck "true" interval="1ns"`,
		`healthcheck "true" retries="bad"`,
	} {
		if _, err := definition.Parse(strings.NewReader(`onbuild { ` + child + ` }`)); err == nil {
			t.Errorf("accepted unusable ONBUILD metadata for %s", child)
		}
	}
}

func TestOnBuildMetadataPreservesSupportedChildScopeFlags(t *testing.T) {
	for _, child := range []string{
		`copy "src" "/dst" chmod="${permissions}"`,
		`run "true" { mount "cache" target="${cache}" }`,
	} {
		parsed, err := definition.Parse(strings.NewReader(`onbuild { ` + child + ` }`))
		if err != nil {
			t.Fatal(err)
		}
		trigger := parsed.Instructions[0].Arguments[0]
		if _, err := onbuildparse.ParseDeferred(trigger); err != nil {
			t.Fatalf("deferred trigger %q: %v", trigger, err)
		}
		got, err := onbuildparse.ParseExpanded(trigger, func(word string) (string, error) {
			return strings.NewReplacer("${permissions}", "0644", "${cache}", "/cache").Replace(word), nil
		})
		if err != nil {
			t.Fatalf("expanded trigger %q: %v", trigger, err)
		}
		if got[0].Name == "copy" && got[0].Properties["chmod"] != "0644" {
			t.Fatalf("copy properties = %#v", got[0].Properties)
		}
		if got[0].Name == "run" && got[0].Children[0].Properties["target"] != "/cache" {
			t.Fatalf("run children = %#v", got[0].Children)
		}
	}
}

func TestOnBuildMetadataDefersTypedMountFlags(t *testing.T) {
	parsed, err := definition.Parse(strings.NewReader(`onbuild { run "true" { mount "secret" id="token" target="${target:-/run/token}" required="${required:-true}" mode="${mode:-0400}" uid="${uid:-1000}" } }`))
	if err != nil {
		t.Fatal(err)
	}
	trigger := parsed.Instructions[0].Arguments[0]
	got, err := onbuildparse.ParseDeferred(trigger)
	if err != nil {
		t.Fatal(err)
	}
	mount := got[0].Children[0]
	for name, want := range map[string]string{"required": "${required:-true}", "mode": "${mode:-0400}", "uid": "${uid:-1000}", "target": "${target:-/run/token}"} {
		if mount.Properties[name] != want {
			t.Errorf("%s = %q, want %q", name, mount.Properties[name], want)
		}
	}
}

func TestOnBuildMetadataDefersMountTypeAndSharing(t *testing.T) {
	parsed, err := definition.Parse(strings.NewReader(`onbuild { run "true" { mount "${kind:-cache}" target="/cache" sharing="${sharing:-locked}" } }`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := onbuildparse.ParseDeferred(parsed.Instructions[0].Arguments[0])
	if err != nil {
		t.Fatal(err)
	}
	mount := got[0].Children[0]
	if mount.Arguments[0] != "${kind:-cache}" || mount.Properties["sharing"] != "${sharing:-locked}" {
		t.Fatalf("deferred mount = %#v", mount)
	}
}

func TestOnBuildMetadataRejectsNativeOnlyMountOptions(t *testing.T) {
	for _, child := range []string{
		`mount "bind" target="/source" bind-nonrecursive=#true`,
		`mount "cache" target="${target}" U=#true`,
		`mount "bind" target="/source" Z=#true`,
		`mount "secret" target="/token" source="/host/token" id="token"`,
	} {
		_, err := definition.Parse(strings.NewReader(`onbuild { run "true" { ` + child + ` } }`))
		if err == nil || !strings.Contains(err.Error(), "Dockerfile metadata") {
			t.Errorf("%s: error = %v, want Dockerfile metadata boundary", child, err)
		}
	}
}
