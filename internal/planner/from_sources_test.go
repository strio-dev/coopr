package planner

import (
	"strings"
	"testing"

	"coopr/internal/definition"
)

func parseFromSourcesDefinition(t *testing.T, source string) *definition.Definition {
	t.Helper()
	def, err := definition.Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func TestResolveFromSourcesExpandsGlobalArguments(t *testing.T) {
	def := parseFromSourcesDefinition(t, `arg "registry" "docker.io"
arg "base" "${registry}/library/alpine"
from "$base" platform="$TARGETPLATFORM"
`)

	defaults, err := ResolveFromSources(def, Options{Platform: "linux/arm64/v8"})
	if err != nil {
		t.Fatal(err)
	}
	if len(defaults) != 1 || defaults[0].Source != "docker.io/library/alpine" || defaults[0].Platform != "linux/arm64/v8" {
		t.Fatalf("default FROM resolution = %+v", defaults)
	}

	overridden, err := ResolveFromSources(def, Options{
		Arguments: map[string]string{"registry": "registry.example", "base": "registry.example/base:v1"},
		Platform:  "linux/amd64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := overridden[0]; got.Source != "registry.example/base:v1" || got.Platform != "linux/amd64" {
		t.Fatalf("overridden FROM resolution = %+v", got)
	}
}

func TestResolveFromSourcesAllowsUnsetArgumentWhenImageRemainsValid(t *testing.T) {
	def := parseFromSourcesDefinition(t, "arg \"variant\"\nfrom \"busybox:stable${variant}\"\n")
	sources, err := ResolveFromSources(def, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Source != "busybox:stable" {
		t.Fatalf("unset FROM argument resolved to %+v", sources)
	}
}

func TestResolveFromSourcesPreservesRawIDsAndClassifiesSources(t *testing.T) {
	def := parseFromSourcesDefinition(t, `from "alpine" as="base" platform="linux/arm64"
run "prepare"
package as="payload"
from "BASE" as="child"
from "scratch"
`)

	sources, err := ResolveFromSources(def, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []FromSource{
		{StageID: "0", Source: "alpine", Platform: "linux/arm64", Kind: FromSourceImage},
		{StageID: "2", Source: "BASE", Platform: "linux/arm64", Kind: FromSourceStage, BaseStageID: "0"},
		{StageID: "3", Source: "scratch", Platform: "linux/amd64", Kind: FromSourceScratch},
	}
	if len(sources) != len(want) {
		t.Fatalf("sources = %+v", sources)
	}
	for i := range want {
		if sources[i] != want[i] {
			t.Fatalf("source %d = %+v, want %+v", i, sources[i], want[i])
		}
	}
}

func TestResolveFromSourcesDefersStageBodyExpansion(t *testing.T) {
	def := parseFromSourcesDefinition(t, `from "alpine"
env value="$inherited_arg"
`)

	plan, err := Create(def, Options{Mode: Build})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Stages[0].Operations[0].Properties["value"]; got != "" {
		t.Fatalf("undefined variable expanded to %q, want empty", got)
	}
	sources, err := ResolveFromSources(def, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Source != "alpine" {
		t.Fatalf("sources = %+v", sources)
	}
}

func TestResolveFromSourcesRejectsInvalidArguments(t *testing.T) {
	tests := []struct {
		name string
		def  string
		opts Options
		want string
	}{
		{name: "undeclared structural", def: "from \"$base\"\n", want: `invalid from source ""`},
		{name: "unset structural", def: "arg \"base\"\nfrom \"$base\"\n", want: `invalid from source ""`},
		{name: "invalid platform", def: "from \"alpine\"\n", opts: Options{Platform: "windows/amd64"}, want: "Coopr requires Linux"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ResolveFromSources(parseFromSourcesDefinition(t, test.def), test.opts)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestResolveFromSourcesRejectsLocalPlatformChange(t *testing.T) {
	def := parseFromSourcesDefinition(t, `from "alpine" as="base" platform="linux/arm64"
from "base" platform="linux/amd64"
`)
	if _, err := ResolveFromSources(def, Options{}); err == nil || !strings.Contains(err.Error(), "cannot change the platform") {
		t.Fatalf("error = %v", err)
	}
}
