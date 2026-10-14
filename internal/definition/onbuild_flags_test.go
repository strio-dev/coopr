package definition_test

import (
	"strings"
	"testing"

	"coopr/internal/onbuildparse"
)

func TestOnBuildFlagsRequireLiteralsWhenDockerfileCannotExpandThem(t *testing.T) {
	for _, property := range []string{"interval", "timeout", "start-period", "start-interval", "retries"} {
		t.Run(property, func(t *testing.T) {
			_, err := parseFragment(strings.NewReader(`onbuild { healthcheck "true" ` + property + `="${value}" }`))
			if err == nil || !strings.Contains(err.Error(), property) || !strings.Contains(err.Error(), "literal") {
				t.Fatalf("error = %v, want named literal-required ONBUILD flag", err)
			}
		})
	}
	_, err := parseFragment(strings.NewReader(`onbuild { run "true" { device "vendor.example/gpu=one" required="${required}" } }`))
	if err == nil || !strings.Contains(err.Error(), "required") || !strings.Contains(err.Error(), "literal") {
		t.Fatalf("error = %v, want literal-required device flag", err)
	}
}

func TestOnBuildMetadataRejectsFlagsThatFailDockerfileParsing(t *testing.T) {
	for _, child := range []string{
		`run "true" security="${security}"`,
		`healthcheck "true" interval="0s"`,
		`healthcheck "true" retries="bad"`,
	} {
		if _, err := parseFragment(strings.NewReader(`onbuild { ` + child + ` }`)); err == nil {
			t.Errorf("accepted unusable ONBUILD metadata for %s", child)
		}
	}
}

func TestOnBuildMetadataPreservesSupportedChildScopeFlags(t *testing.T) {
	for _, child := range []string{
		`copy "src" "/dst" chmod="${permissions}"`,
		`run "true" { mount "cache" target="${cache}" }`,
	} {
		parsed, err := parseFragment(strings.NewReader(`onbuild { ` + child + ` }`))
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
	parsed, err := parseFragment(strings.NewReader(`onbuild { run "true" { mount "secret" id="token" target="${target:-/run/token}" required="${required:-true}" mode="${mode:-0400}" uid="${uid:-1000}" } }`))
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
	parsed, err := parseFragment(strings.NewReader(`onbuild { run "true" { mount "${kind:-cache}" target="/cache" sharing="${sharing:-locked}" } }`))
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

func TestOnBuildMetadataPreservesNativeMountOptions(t *testing.T) {
	for _, child := range []string{
		`mount "bind" target="/source" bind-nonrecursive=#true`,
		`mount "cache" target="${target}" U=#true`,
		`mount "bind" target="/source" Z=#true`,
		`mount "secret" target="/token" source="token"`,
	} {
		_, err := parseFragment(strings.NewReader(`onbuild { run "true" { ` + child + ` } }`))
		if err != nil {
			t.Errorf("%s: error = %v", child, err)
		}
	}
}

func TestOnBuildNativeMountEncodingRoundTrip(t *testing.T) {
	for _, child := range []string{
		`mount "tmpfs" target="/tmp" mode="0755" size="1m"`,
		`mount "cache" target="/cache" relabel="private"`,
	} {
		parsed, err := parseFragment(strings.NewReader(`onbuild {run "true" {` + child + `}}`))
		if err != nil {
			t.Fatal(err)
		}
		raw := parsed.Instructions[0].Arguments[0]
		if strings.Contains(child, `"tmpfs"`) && (!strings.Contains(raw, "tmpfs-mode=0755") || !strings.Contains(raw, "tmpfs-size=1m")) {
			t.Errorf("native tmpfs metadata: %s", raw)
		}
		if strings.Contains(child, `"cache"`) && !strings.Contains(raw, ",Z") {
			t.Errorf("native cache relabel metadata: %s", raw)
		}
		if _, err := onbuildparse.ParseExpanded(raw, func(word string) (string, error) { return word, nil }); err != nil {
			t.Fatalf("consume %s: %v", raw, err)
		}
	}
}

func TestOnBuildUnresolvedMountTypeRejectsAmbiguousNativeFields(t *testing.T) {
	for _, option := range []string{`relabel="private"`} {
		_, err := parseFragment(strings.NewReader(`onbuild {run "true" {mount "${kind:-cache}" target="/target" ` + option + `}}`))
		if err == nil || !strings.Contains(err.Error(), "literal mount type") {
			t.Errorf("%s: %v", option, err)
		}
	}
}

func TestOnBuildDynamicMountTypeKeepsRepresentableNativeFields(t *testing.T) {
	for _, test := range []struct{ kind, field, native string }{
		{"bind", `source="payload"`, "source=payload"},
		{"cache", `source="payload"`, "source=payload"},
		{"bind", `readonly=#true`, ",ro"},
		{"cache", `readonly=#true`, ",ro"},
		{"tmpfs", `readonly=#true`, ",ro"},
		{"cache", `mode="0700"`, "mode=0700"},
		{"secret", `mode="0400"`, "mode=0400"},
		{"ssh", `mode="0600"`, "mode=0600"},
	} {
		parsed, err := parseFragment(strings.NewReader(`onbuild {run "true" {mount "${kind}" target="/target" ` + test.field + `}}`))
		if err != nil {
			t.Errorf("%s %s: %v", test.kind, test.field, err)
			continue
		}
		raw := parsed.Instructions[0].Arguments[0]
		if !strings.Contains(raw, test.native) {
			t.Errorf("native spelling lost: %s", raw)
		}
		if _, err := onbuildparse.ParseDeferred(raw); err != nil {
			t.Fatal(err)
		}
		got, err := onbuildparse.ParseExpanded(raw, func(word string) (string, error) { return strings.ReplaceAll(word, "${kind}", test.kind), nil })
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		if got[0].Children[0].Arguments[0] != test.kind {
			t.Fatalf("child type: %#v", got)
		}
	}
}

func TestOnBuildMaintainerCannotBeAuthored(t *testing.T) {
	if _, err := parseFragment(strings.NewReader(`onbuild {maintainer "person"}`)); err == nil || !strings.Contains(err.Error(), "MAINTAINER") {
		t.Fatalf("authoring: %v", err)
	}
}

func TestOnBuildDynamicMountModeDoesNotGuessTmpfsConversion(t *testing.T) {
	parsed, err := parseFragment(strings.NewReader(`onbuild {run "true" {mount "${kind}" target="/target" mode="0700"}}`))
	if err != nil {
		t.Fatal(err)
	}
	raw := parsed.Instructions[0].Arguments[0]
	if strings.Contains(raw, "tmpfs-mode=") {
		t.Fatalf("guessed dynamic tmpfs conversion: %s", raw)
	}
	_, err = onbuildparse.ParseExpanded(raw, func(word string) (string, error) { return strings.ReplaceAll(word, "${kind}", "tmpfs"), nil })
	if err == nil {
		t.Fatal("accepted mode as native tmpfs-mode after dynamic resolution")
	}
}
