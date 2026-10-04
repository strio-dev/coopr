package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func definitionFile(t *testing.T, source string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "build.coopr")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNamedValuesRejectInvalidUTF8(t *testing.T) {
	v := namedValues{values: map[string]string{}}
	if err := v.Set("x=" + string([]byte{0xff})); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid value accepted: %v", err)
	}
	if err := v.Set(string([]byte{0xff}) + "=x"); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid name accepted: %v", err)
	}
	if err := v.Set("x=�"); err != nil {
		t.Fatalf("valid replacement character rejected: %v", err)
	}
}

func TestNamedValuesInheritEnvironment(t *testing.T) {
	t.Setenv("COOPR_TEST_BUILD_ARG", "from-environment")
	v := namedValues{values: map[string]string{}}
	if err := v.Set("COOPR_TEST_BUILD_ARG"); err != nil {
		t.Fatalf("inherit environment: %v", err)
	}
	if got := v.values["COOPR_TEST_BUILD_ARG"]; got != "from-environment" {
		t.Fatalf("inherited value = %q", got)
	}
	if err := v.Set("COOPR_TEST_BUILD_ARG=explicit"); err != nil {
		t.Fatalf("override inherited argument: %v", err)
	}
	if got := v.values["COOPR_TEST_BUILD_ARG"]; got != "explicit" {
		t.Fatalf("overridden value = %q", got)
	}
}

func TestNamedValuesLeaveUnsetEnvironmentUnspecified(t *testing.T) {
	const name = "COOPR_TEST_UNSET_BUILD_ARG"
	original, existed := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(name, original)
		}
	})
	v := namedValues{values: map[string]string{}}
	if err := v.Set(name); err != nil {
		t.Fatalf("unset environment variable rejected: %v", err)
	}
	if _, exists := v.values[name]; exists {
		t.Fatal("unset environment variable created an override")
	}
	if err := v.Set(name + "=explicit"); err != nil {
		t.Fatalf("set argument after absent environment lookup: %v", err)
	}
	if got := v.values[name]; got != "explicit" {
		t.Fatalf("overridden value = %q", got)
	}
	if err := v.Set(name); err != nil {
		t.Fatalf("clear argument with absent environment lookup: %v", err)
	}
	if _, exists := v.values[name]; exists {
		t.Fatal("last absent environment lookup did not clear earlier override")
	}
}

func TestNamedValuesLastExplicitValueWins(t *testing.T) {
	v := namedValues{values: map[string]string{}}
	for _, value := range []string{"mode=first", "mode=second", "mode=final"} {
		if err := v.Set(value); err != nil {
			t.Fatalf("set %q: %v", value, err)
		}
	}
	if got := v.values["mode"]; got != "final" {
		t.Fatalf("last build argument = %q", got)
	}
}

func TestNamedValuesRejectInvalidNames(t *testing.T) {
	for _, value := range []string{"=value", " leading=value", "trailing =value", "two words=value", "tab\tname=value"} {
		v := namedValues{values: map[string]string{}}
		if err := v.Set(value); err == nil {
			t.Errorf("invalid build argument %q accepted", value)
		}
	}
}

func TestPlanCommandIsAbsent(t *testing.T) {
	var out, errOut bytes.Buffer
	if status := run([]string{"plan"}, &out, &errOut); status == 0 || !strings.Contains(errOut.String(), "unknown command") {
		t.Fatalf("status %d, stderr %q", status, errOut.String())
	}
}

func TestHelp(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}} {
		var out, errOut bytes.Buffer
		if status := run(args, &out, &errOut); status != 0 {
			t.Fatalf("help status %d: %s", status, errOut.String())
		}
		if strings.Contains(out.String(), "plan") {
			t.Fatalf("help advertises removed plan command: %s", out.String())
		}
	}
}

func TestCompletion(t *testing.T) {
	var out, errOut bytes.Buffer
	if status := run([]string{"completion", "bash"}, &out, &errOut); status != 0 || !strings.Contains(out.String(), "coopr") {
		t.Fatalf("status %d, stdout %q, stderr %q", status, out.String(), errOut.String())
	}
}

func TestVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	if status := run([]string{"--version"}, &out, &errOut); status != 0 {
		t.Fatalf("version status %d: %s", status, errOut.String())
	}
	if got, want := out.String(), "coopr version dev\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}
