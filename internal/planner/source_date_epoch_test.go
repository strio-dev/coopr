package planner

import (
	"reflect"
	"strings"
	"testing"

	"coopr/internal/buildcontext"
	"coopr/internal/definition"
)

func TestSourceDateEpochSpecialArgument(t *testing.T) {
	tests := []struct {
		name      string
		source    string
		arguments map[string]string
		want      *int64
		wantRun   string
		exposeRun bool
		context   *int64
	}{
		{name: "numeric override wins over context", source: "arg \"SOURCE_DATE_EPOCH\" \"context\"\nfrom \"scratch\"\nrun \"true\"", arguments: map[string]string{"SOURCE_DATE_EPOCH": "00123"}, context: epochPointer(946684800), want: epochPointer(123)},
		{name: "global default", source: "arg \"SOURCE_DATE_EPOCH\" \"456\"\nfrom \"scratch\"\nrun \"true\"", want: epochPointer(456)},
		{name: "override and redeclare", source: "arg \"SOURCE_DATE_EPOCH\" \"456\"\nfrom \"scratch\"\narg \"SOURCE_DATE_EPOCH\"\nrun \"printf %s $SOURCE_DATE_EPOCH\"", arguments: map[string]string{"SOURCE_DATE_EPOCH": "00789"}, want: epochPointer(789), wantRun: "789", exposeRun: true},
		{name: "empty reset", source: "arg \"SOURCE_DATE_EPOCH\" \"456\"\nfrom \"scratch\"\narg \"SOURCE_DATE_EPOCH\"\nrun \"true\"", arguments: map[string]string{"SOURCE_DATE_EPOCH": ""}, exposeRun: true},
		{name: "local context resolves unset", source: "arg \"SOURCE_DATE_EPOCH\" \"456\"\nfrom \"scratch\"\narg \"SOURCE_DATE_EPOCH\"\nrun \"true\"", arguments: map[string]string{"SOURCE_DATE_EPOCH": "context"}, exposeRun: true},
		{name: "remote context override", source: "from \"scratch\"\nrun \"true\"", arguments: map[string]string{"SOURCE_DATE_EPOCH": "context"}, context: epochPointer(946684800), want: epochPointer(946684800)},
		{name: "remote context default", source: "arg \"SOURCE_DATE_EPOCH\" \"context\"\nfrom \"scratch\"\narg \"SOURCE_DATE_EPOCH\"\nrun \"true\"", context: epochPointer(946684801), want: epochPointer(946684801), wantRun: "946684801", exposeRun: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			def, err := definition.Parse(strings.NewReader(test.source))
			if err != nil {
				t.Fatal(err)
			}
			plan, err := Create(def, Options{Mode: Build, Platform: "linux/amd64", Arguments: test.arguments, ContextSourceDateEpoch: test.context})
			if err != nil {
				t.Fatal(err)
			}
			if !equalEpoch(plan.SourceDateEpoch, test.want) {
				t.Fatalf("epoch = %v, want %v", plan.SourceDateEpoch, test.want)
			}
			if test.exposeRun {
				run := plan.Stages[0].Operations[len(plan.Stages[0].Operations)-1]
				if got := run.ArgumentsInScope["SOURCE_DATE_EPOCH"]; got != test.wantRun {
					t.Fatalf("RUN epoch = %q, want %q", got, test.wantRun)
				}
			} else if run := plan.Stages[0].Operations[len(plan.Stages[0].Operations)-1]; run.Name == "run" {
				if _, exposed := run.ArgumentsInScope["SOURCE_DATE_EPOCH"]; exposed {
					t.Fatalf("global SOURCE_DATE_EPOCH exposed to RUN without stage ARG: %#v", run.ArgumentsInScope)
				}
			}
		})
	}
}

func TestSourceDateEpochRejectsInvalidValue(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`from "scratch"`))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"tomorrow", "-1", "1.5", "253402300800", "named-remote-context"} {
		if _, err := Create(def, Options{Mode: Build, Arguments: map[string]string{"SOURCE_DATE_EPOCH": value}}); err == nil {
			t.Fatalf("accepted SOURCE_DATE_EPOCH=%q", value)
		}
	}
}

func TestSourceDateEpochResolvesNamedContextBeforeStage(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`
arg "SOURCE_DATE_EPOCH" "source"
from "scratch" as="source"
add "https://example.invalid/source.tar" "/source"
from "scratch"
arg "SOURCE_DATE_EPOCH"
run "printf %s $SOURCE_DATE_EPOCH"
`))
	if err != nil {
		t.Fatal(err)
	}
	var got SourceDateEpochSource
	epoch := int64(946684800)
	plan, err := Create(def, Options{
		Mode:          Build,
		BuildContexts: []buildcontext.Spec{{Name: "source", Kind: buildcontext.Git, Reference: "https://example.invalid/context.git"}},
		SourceDateEpochResolver: func(source SourceDateEpochSource) (*int64, error) {
			got = source
			return &epoch, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != SourceDateEpochNamedContext || got.Context == nil || got.Context.Reference != "https://example.invalid/context.git" {
		t.Fatalf("resolved source = %#v", got)
	}
	if plan.SourceDateEpoch == nil || *plan.SourceDateEpoch != epoch {
		t.Fatalf("epoch = %v", plan.SourceDateEpoch)
	}
	if value := plan.Stages[len(plan.Stages)-1].Operations[0].ArgumentsInScope["SOURCE_DATE_EPOCH"]; value != "946684800" {
		t.Fatalf("stage epoch = %q", value)
	}
}

func TestSourceDateEpochSourceOnlyStage(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`
arg "SOURCE_DATE_EPOCH" "metadata"
arg "repository" "https://example.invalid/source"
from "scratch" as="metadata"
arg "suffix" ".git#main"
arg "SOURCE_DATE_EPOCH" "must-not-recurse"
add "${repository}${suffix}" "/ignored" checksum="sha256:abc"
from "scratch"
`))
	if err != nil {
		t.Fatal(err)
	}
	epoch := int64(42)
	var got SourceDateEpochSource
	plan, err := Create(def, Options{Mode: Build, SourceDateEpochResolver: func(source SourceDateEpochSource) (*int64, error) {
		got = source
		return &epoch, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := SourceDateEpochSource{Kind: SourceDateEpochGit, Name: "https://example.invalid/source.git#main", Reference: "https://example.invalid/source.git#main", Checksum: "sha256:abc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved source = %#v, want %#v", got, want)
	}
	if plan.SourceDateEpoch == nil || *plan.SourceDateEpoch != epoch {
		t.Fatalf("epoch = %v", plan.SourceDateEpoch)
	}
}

func TestSourceDateEpochSourceOnlyStageValidation(t *testing.T) {
	tests := []struct {
		name, source, want string
	}{
		{"base", `arg "SOURCE_DATE_EPOCH" "metadata"
from "alpine" as="metadata"
add "https://example.invalid/source.tar" "/source"
from "scratch"`, "must use FROM scratch"},
		{"missing add", `arg "SOURCE_DATE_EPOCH" "metadata"
from "scratch" as="metadata"
arg "value" "one"
from "scratch"`, "exactly one remote ADD"},
		{"multiple sources", `arg "SOURCE_DATE_EPOCH" "metadata"
from "scratch" as="metadata"
add "https://example.invalid/one" "https://example.invalid/two" "/source"
from "scratch"`, "exactly one remote ADD source"},
		{"local add", `arg "SOURCE_DATE_EPOCH" "metadata"
from "scratch" as="metadata"
add "source.tar" "/source"
from "scratch"`, "single HTTP(S) or Git ADD"},
		{"unsupported instruction", `arg "SOURCE_DATE_EPOCH" "metadata"
from "scratch" as="metadata"
run "true"
add "https://example.invalid/source.tar" "/source"
from "scratch"`, "unsupported run instruction"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			def, err := definition.Parse(strings.NewReader(test.source))
			if err != nil {
				t.Fatal(err)
			}
			_, err = Create(def, Options{Mode: Build, SourceDateEpochResolver: func(SourceDateEpochSource) (*int64, error) { return nil, nil }})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func epochPointer(value int64) *int64 { return &value }
func equalEpoch(left, right *int64) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}
