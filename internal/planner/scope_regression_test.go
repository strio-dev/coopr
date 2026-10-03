package planner

import (
	"strings"
	"testing"

	"coopr/internal/definition"
)

func TestGlobalARGRequiresStageRedeclarationForRuntimeScope(t *testing.T) {
	plan := makePlan(t, `
arg "flavor" "global"
from "scratch"
env before="${flavor}"
run "true"
arg "flavor"
env after="${flavor}"
run "true"
`, Options{Mode: Build})
	stage := plan.Stages[0]
	if got := stage.Operations[0].Properties["before"]; got != "" {
		t.Fatalf("global ARG leaked into stage ENV: %q", got)
	}
	if _, present := stage.Operations[1].ArgumentsInScope["flavor"]; present {
		t.Fatalf("global ARG leaked into stage RUN/cache scope: %+v", stage.Operations[1].ArgumentsInScope)
	}
	if got := stage.Operations[2].Properties["after"]; got != "global" {
		t.Fatalf("redeclared global ARG = %q, want global", got)
	}
	if got := stage.Operations[3].ArgumentsInScope["flavor"]; got != "global" {
		t.Fatalf("redeclared RUN ARG = %q, want global", got)
	}
}

func TestRepeatedGlobalARGUsesLatestDefaultAndPreservesOverride(t *testing.T) {
	def := parse(t, `
arg "base" "alpine:3.19"
arg "base" "alpine:3.20"
from "$base"
`)
	for _, test := range []struct {
		name string
		opts Options
		want string
	}{
		{name: "latest default", opts: Options{Mode: Build}, want: "alpine:3.20"},
		{name: "CLI override", opts: Options{Mode: Build, Arguments: map[string]string{"base": "busybox:1.36"}}, want: "busybox:1.36"},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, err := Create(def, test.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got := plan.Stages[0].Source; got != test.want {
				t.Fatalf("planned FROM = %q, want %q", got, test.want)
			}
			sources, err := ResolveFromSources(def, test.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got := sources[0].Source; got != test.want {
				t.Fatalf("resolved FROM = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAuthoredPlatformARGDefaultOverridesAutomaticValue(t *testing.T) {
	def := parse(t, `
arg "TARGETOS" "custom"
from "example.invalid/base:$TARGETOS"
arg "TARGETOS"
env target_os="$TARGETOS"
`)
	plan, err := Create(def, Options{Mode: Build})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Stages[0].Source; got != "example.invalid/base:custom" {
		t.Fatalf("planned FROM = %q, want custom ARG default", got)
	}
	if got := plan.Stages[0].Operations[0].Properties["target_os"]; got != "custom" {
		t.Fatalf("redeclared automatic ARG = %q, want custom global default", got)
	}
	resolved, err := ResolveFromSources(def, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved[0].Source; got != "example.invalid/base:custom" {
		t.Fatalf("resolved FROM = %q, want custom ARG default", got)
	}
}

func TestLocalStageRetainsInheritedARGScope(t *testing.T) {
	plan := makePlan(t, `
arg "flavor" "global"
from "scratch" as="base"
arg "flavor"
run "true"
from "base"
run "true"
`, Options{Mode: Build})
	child := plan.Stages[len(plan.Stages)-1]
	if got := child.Operations[0].ArgumentsInScope["flavor"]; got != "global" {
		t.Fatalf("local child inherited ARG = %q, want global", got)
	}
}

func TestPackageArgumentsIgnoreLaterAndCrossStageDeclarations(t *testing.T) {
	plan := makePlan(t, `
arg "future" "global"
from "scratch" as="unrelated"
arg "future"
env unrelated="${future}"
from "scratch" as="producer"
env before="${future}"
arg "future"
package as="bundle"
copy "/asset" "/asset" from="producer"
extend
copy "/asset" "/asset" from="bundle"
`, Options{Mode: Publish})
	if _, fixed := plan.PackageArguments["future"]; fixed {
		t.Fatalf("later or cross-stage ARG was falsely frozen: %+v", plan.PackageArguments)
	}
}

func TestPackageArgumentsInspectInheritedOnBuildInOrder(t *testing.T) {
	def := parse(t, `
arg "flavor" "stable"
from "registry.example/parent:latest" as="producer"
package as="bundle"
copy "/asset" "/asset" from="producer"
extend
copy "/asset" "/asset" from="bundle"
`)
	plan, err := CreateDemandDriven(def, Options{Mode: Publish}, func(source FromSource) (StageBind, error) {
		return StageBind{Inherited: []definition.Instruction{
			{Name: "arg", Arguments: []string{"flavor"}},
			{Name: "env", Properties: map[string]string{"inherited": "${flavor}"}},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.PackageArguments["flavor"]; got != "stable" {
		t.Fatalf("inherited ONBUILD ARG = %q, want stable", got)
	}
}

func TestPackageArgumentsExcludeDeferredAuthoredOnBuild(t *testing.T) {
	plan := makePlan(t, `
arg "deferred" "invocation"
from "scratch" as="producer"
onbuild { env deferred="$deferred" }
package as="bundle"
copy "/asset" "/asset" from="producer"
extend
copy "/asset" "/asset" from="bundle"
`, Options{Mode: Publish})
	if _, fixed := plan.PackageArguments["deferred"]; fixed {
		t.Fatalf("deferred authored ONBUILD argument was frozen: %+v", plan.PackageArguments)
	}
}

func TestResolveFromSourcesNormalizesSourceDateEpochLikeCreate(t *testing.T) {
	def := parseFromSourcesDefinition(t, `
arg "SOURCE_DATE_EPOCH" "00042"
from "registry.example/image:${SOURCE_DATE_EPOCH}"
`)
	sources, err := ResolveFromSources(def, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Source != "registry.example/image:42" {
		t.Fatalf("normalized sources = %+v", sources)
	}
	for _, test := range []struct {
		name string
		def  *definition.Definition
		opts Options
	}{
		{name: "invalid override", def: parseFromSourcesDefinition(t, `from "scratch"`), opts: Options{Arguments: map[string]string{"SOURCE_DATE_EPOCH": "tomorrow"}}},
		{name: "invalid default", def: parseFromSourcesDefinition(t, `arg "SOURCE_DATE_EPOCH" "tomorrow"
from "scratch"`), opts: Options{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ResolveFromSources(test.def, test.opts)
			if err == nil || !strings.Contains(err.Error(), "SOURCE_DATE_EPOCH") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
