package planner

import (
	"testing"

	"github.com/containerd/platforms"
)

func TestDockerfileVariableExpansion(t *testing.T) {
	values := scope{
		"set":   {value: "value", present: true},
		"empty": {value: "", present: true},
		"unset": {},
	}
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "unbraced", input: "$set", want: "value"},
		{name: "braced", input: "${set}", want: "value"},
		{name: "escaped", input: `\$set`, want: "$set"},
		{name: "unset default", input: "${unset-default}", want: "default"},
		{name: "unset colon default", input: "${unset:-default}", want: "default"},
		{name: "empty default", input: "${empty-default}", want: ""},
		{name: "empty colon default", input: "${empty:-default}", want: "default"},
		{name: "unset alternate", input: "${unset+alternate}", want: ""},
		{name: "set alternate", input: "${set+alternate}", want: "alternate"},
		{name: "empty alternate", input: "${empty+alternate}", want: "alternate"},
		{name: "empty colon alternate", input: "${empty:+alternate}", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := expand(test.input, values)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("expand(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestExpansionUsesDockerfileUnsetArgumentSemantics(t *testing.T) {
	for _, test := range []struct {
		input  string
		values scope
		want   string
	}{
		{input: "$missing", want: ""},
		{input: "prefix-${required}", values: scope{"required": {}}, want: "prefix-"},
		{input: "${required:-fallback}", values: scope{"required": {}}, want: "fallback"},
	} {
		got, err := expand(test.input, test.values)
		if err != nil || got != test.want {
			t.Fatalf("expand(%q) = %q, %v; want %q", test.input, got, err, test.want)
		}
	}
}

func TestPlannerExpandsStructuralAndPlatformVariables(t *testing.T) {
	plan := makePlan(t, `arg "base_image" "alpine"
arg "source_stage" "base"
from "$base_image" as="base" platform="$TARGETPLATFORM"
arg "TARGETOS"
arg "TARGETARCH"
arg "TARGETVARIANT"
arg "BUILDPLATFORM"
env TARGET="${TARGETOS}/${TARGETARCH}/${TARGETVARIANT}" BUILD="$BUILDPLATFORM"
run "printf '%s' $TARGETARCH ${base_image}"
from "scratch"
copy "/payload" "/payload" from="$source_stage"
`, Options{Mode: Build, Platform: "linux/arm64/v8"})

	if got := plan.Stages[0].Source; got != "alpine" {
		t.Fatalf("FROM source = %q", got)
	}
	if got := plan.Stages[0].Platform; got != "linux/arm64/v8" {
		t.Fatalf("FROM platform = %q", got)
	}
	env := plan.Stages[0].Operations[0]
	if env.Properties["TARGET"] != "linux/arm64/v8" {
		t.Fatalf("target platform expansion = %q", env.Properties["TARGET"])
	}
	if env.Properties["BUILD"] != platforms.Format(platforms.DefaultSpec()) {
		t.Fatalf("build platform expansion = %q", env.Properties["BUILD"])
	}
	run := plan.Stages[0].Operations[1]
	if run.Arguments[0] != "printf '%s' $TARGETARCH ${base_image}" {
		t.Fatalf("RUN payload was expanded: %q", run.Arguments[0])
	}
	copy := plan.Stages[1].Operations[0]
	if copy.Properties["from"] != "base" || len(plan.Stages[1].Dependencies) != 1 || plan.Stages[1].Dependencies[0] != "0" {
		t.Fatalf("COPY --from binding = %+v, dependencies = %v", copy.Properties, plan.Stages[1].Dependencies)
	}
	if _, ok := run.ArgumentsInScope["TARGETARCH"]; !ok {
		t.Fatal("automatic platform variables missing from executor scope")
	}
	if len(plan.Arguments) != 6 {
		t.Fatalf("declared arguments = %+v", plan.Arguments)
	}
}

func TestAutomaticPlatformArgumentsIncludeTargetStageAndOSVersions(t *testing.T) {
	definition := parse(t, `from "scratch" as="release"
env BEFORE="$TARGETSTAGE:$TARGETOSVERSION"
arg "TARGETSTAGE"
arg "TARGETOSVERSION"
arg "BUILDOSVERSION"
env AFTER="$TARGETSTAGE:$TARGETOSVERSION:$BUILDOSVERSION"
`)
	for _, test := range []struct {
		name      string
		target    string
		arguments map[string]string
		want      string
	}{
		{name: "default target", want: "default::"},
		{name: "selected target", target: "release", want: "release::"},
		{name: "build argument overrides", target: "release", arguments: map[string]string{
			"TARGETSTAGE": "custom", "TARGETOSVERSION": "target-v1", "BUILDOSVERSION": "build-v2",
		}, want: "custom:target-v1:build-v2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, err := Create(definition, Options{Mode: Build, Platform: "linux/amd64", Target: test.target, Arguments: test.arguments})
			if err != nil {
				t.Fatal(err)
			}
			operations := plan.Stages[0].Operations
			if got := operations[0].Properties["BEFORE"]; got != ":" {
				t.Fatalf("automatic argument leaked before ARG declaration: %q", got)
			}
			if got := operations[1].Properties["AFTER"]; got != test.want {
				t.Fatalf("automatic argument expansion = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAutomaticPlatformBuildArgumentOverrideAppliesToFromResolution(t *testing.T) {
	definition := parse(t, `from "scratch" platform="$TARGETPLATFORM"
arg "TARGETPLATFORM"
env RESOLVED="$TARGETPLATFORM"
`)
	opts := Options{Mode: Build, Platform: "linux/amd64", Arguments: map[string]string{"TARGETPLATFORM": "linux/arm64"}}
	sources, err := ResolveFromSources(definition, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Platform != "linux/arm64" {
		t.Fatalf("FROM source platform override = %+v", sources)
	}
	plan, err := Create(definition, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Stages[0].Operations[0].Properties["RESOLVED"]; got != "linux/arm64" {
		t.Fatalf("stage ARG platform override = %q", got)
	}
}

func TestAutomaticPlatformVariablesDoNotLeakIntoComponentMetadata(t *testing.T) {
	publication := makePlan(t, `extend
arg "TARGETARCH"
env TARGET="$TARGETARCH"
`, Options{Mode: Publish, Platform: "linux/arm64"})
	if len(publication.PackageArguments) != 0 || len(publication.Component.PackageArguments) != 0 {
		t.Fatalf("automatic variables leaked into package metadata: plan=%v component=%v", publication.PackageArguments, publication.Component.PackageArguments)
	}
	invocation := invokePublished(t, publication, Options{})
	if got := invocation.Stages[0].Operations[0].Properties["TARGET"]; got != "arm64" {
		t.Fatalf("invocation target expansion = %q", got)
	}
}
