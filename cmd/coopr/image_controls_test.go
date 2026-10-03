package main

import (
	"os"
	"slices"
	"testing"

	"github.com/spf13/cobra"
)

func TestImageControlFlagsEnvironmentLookup(t *testing.T) {
	t.Setenv("COOPR_LOOKUP_ONE", "first")
	t.Setenv("COOPR_LOOKUP_TWO", "")
	t.Setenv("COOPR_LOOKUP_MISSING", "temporary")
	if err := os.Unsetenv("COOPR_LOOKUP_MISSING"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		env  []string
		want []string
	}{
		{"absent name", []string{"COOPR_LOOKUP_MISSING"}, nil},
		{"prefix", []string{"COOPR_LOOKUP_*"}, []string{"COOPR_LOOKUP_ONE=first", "COOPR_LOOKUP_TWO="}},
		{"explicit value", []string{"COOPR_LOOKUP_*=literal"}, []string{"COOPR_LOOKUP_*=literal"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			controls, err := (imageControlFlags{env: test.env}).controls()
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(controls.Env)
			if !slices.Equal(controls.Env, test.want) {
				t.Fatalf("environment = %q, want %q", controls.Env, test.want)
			}
		})
	}
	controls, err := (imageControlFlags{env: []string{"*"}}).controls()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(controls.Env, "COOPR_LOOKUP_ONE=first") || !slices.Contains(controls.Env, "COOPR_LOOKUP_TWO=") {
		t.Fatal("full environment expansion omitted test variables")
	}
}

func TestImageControlFlags(t *testing.T) {
	t.Setenv("COOPR_TEST_ENV", "host")
	flags := imageControlFlags{
		env: []string{"COOPR_TEST_ENV", "DIRECT=value"}, labels: []string{"empty", "set=value"},
		annotations: []string{"org.example.flag=value"}, unsetEnv: []string{"OLD"}, unsetLabels: []string{"old"},
		inheritLabels: false, inheritAnnotations: false, omitHistory: true,
	}
	controls, err := flags.controls()
	if err != nil {
		t.Fatal(err)
	}
	if controls.Env[0] != "COOPR_TEST_ENV=host" || controls.Labels[0] != "empty=" || !controls.DropInheritedLabels || !controls.DropInheritedAnnotations || !controls.OmitHistory {
		t.Fatalf("controls = %#v", controls)
	}
}

func TestImageControlFlagsExposePodmanNames(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var flags imageControlFlags
	flags.addTo(cmd)
	for _, name := range []string{"env", "label", "unsetenv", "unsetlabel", "annotation", "unsetannotation", "inherit-labels", "inherit-annotations", "omit-history", "identity-label", "layer-label", "created-annotation", "os-feature", "os-version"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("missing --%s", name)
		}
	}
}

func TestImageControlFlagsPreserveExplicitBooleanState(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var flags imageControlFlags
	flags.addTo(cmd)
	if err := cmd.ParseFlags([]string{"--identity-label=false", "--created-annotation=false", "--layer-label=cache=yes", "--os-feature=win32k", "--os-version=10"}); err != nil {
		t.Fatal(err)
	}
	controls, err := flags.controls(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if controls.IdentityLabel == nil || *controls.IdentityLabel || controls.CreatedAnnotation == nil || *controls.CreatedAnnotation || controls.LayerLabels[0] != "cache=yes" || controls.OSFeatures[0] != "win32k" || controls.OSVersion != "10" {
		t.Fatalf("controls = %#v", controls)
	}
}
