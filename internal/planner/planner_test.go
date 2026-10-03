package planner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"coopr/internal/definition"
)

func parse(t *testing.T, source string) *definition.Definition {
	t.Helper()
	d, err := definition.Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func makePlan(t *testing.T, source string, options Options) *Plan {
	t.Helper()
	var p *Plan
	var err error
	if options.Mode == Invoke {
		publication, publishErr := Create(parse(t, source), Options{Mode: Publish, Target: options.Target, Platform: options.Platform})
		if publishErr != nil {
			t.Fatal(publishErr)
		}
		options.Target = ""
		p, err = Instantiate(publication.Component, options)
	} else {
		p, err = Create(parse(t, source), options)
	}
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func invokePublished(t *testing.T, publication *Plan, options Options) *Plan {
	t.Helper()
	data, err := json.Marshal(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	var decoded PublishedComponent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	p, err := Instantiate(&decoded, options)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func stageIDs(p *Plan) []string {
	var ids []string
	for _, s := range p.Stages {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestNormalizePreservesExecRunForm(t *testing.T) {
	plan := makePlan(t, "from \"alpine\"\nrun { exec \"/usr/local/bin/gofmt\" \"-h\" }\n", Options{})
	if got := plan.Stages[0].Operations[0].Instruction; got.Form != "exec" || len(got.Arguments) != 2 {
		t.Fatalf("exec run was changed by planning: %+v", got)
	}
}

const packaged = `
from "compiler:1" as="builder"
run "compile"
package as="tools"
copy "/out" "/tools" from="builder"
from "tools" as="prepared"
run "prepare"
extend as="base"
run "configure"
from "base"
copy "/tools" "/usr/local/bin" from="prepared"
`

func TestPackageBoundary(t *testing.T) {
	publication := makePlan(t, packaged, Options{Mode: Publish})
	if !reflect.DeepEqual(stageIDs(publication), []string{"0", "1"}) {
		t.Fatalf("publication = %+v", publication)
	}
	invocation := invokePublished(t, publication, Options{})
	if !reflect.DeepEqual(stageIDs(invocation), []string{"2", "0", "1", "3"}) {
		t.Fatalf("invocation order = %v", stageIDs(invocation))
	}
	for _, s := range invocation.Stages {
		if s.Name == "builder" {
			t.Fatal("producer scheduled in invocation")
		}
		if s.Name == "tools" && (s.Kind != "package-input" || len(s.Operations) != 0 || len(s.Dependencies) != 0) {
			t.Fatalf("package is executable: %+v", s)
		}
	}
	if !reflect.DeepEqual(invocation.Outputs, []string{"3"}) {
		t.Fatalf("outputs = %v", invocation.Outputs)
	}
	if !reflect.DeepEqual(invocation.Inputs, []Input{{Kind: "package", Reference: "tools"}}) {
		t.Fatalf("inputs = %v", invocation.Inputs)
	}
}

func TestSharedProducerRunsInBothPhases(t *testing.T) {
	source := packaged
	source += `copy "/other" "/other" from="builder"` + "\n"
	p := invokePublished(t, makePlan(t, source, Options{Mode: Publish}), Options{})
	found := false
	for _, s := range p.Stages {
		if s.Name == "builder" {
			found = true
			if len(s.Operations) != 1 {
				t.Fatal("shared producer lost operation")
			}
		}
	}
	if !found {
		t.Fatal("directly referenced producer must execute")
	}
}

func TestMultiplePackagesAndPackageBasedProducer(t *testing.T) {
	source := `from "compiler:1" as="builder"
run "compile"
package as="tools"
copy "/out" "/" from="builder"
from "tools" as="combined"
run "assemble"
package as="bundle"
copy "/" "/" from="combined"
package as="unused"
copy "./static" "/static"
extend
copy "/" "/opt" from="bundle"
`
	publish := makePlan(t, source, Options{Mode: Publish})
	if !reflect.DeepEqual(publish.Outputs, []string{"1", "3"}) {
		t.Fatalf("outputs = %v", publish.Outputs)
	}
	if len(publish.Stages) != 4 {
		t.Fatalf("publication stage count = %d", len(publish.Stages))
	}
	invoke := invokePublished(t, publish, Options{})
	if !reflect.DeepEqual(stageIDs(invoke), []string{"0", "1"}) {
		t.Fatalf("invocation = %v", stageIDs(invoke))
	}
}

func TestBindMountDependency(t *testing.T) {
	source := `from "compiler:1" as="builder"
package as="tools"
copy "/out" "/" from="builder"
extend
run "install" {
 mount "bind" from="tools" source="/" target="/tools"
}
`
	p := makePlan(t, source, Options{Mode: Invoke})
	if !reflect.DeepEqual(stageIDs(p), []string{"0", "1"}) {
		t.Fatalf("stages = %v", stageIDs(p))
	}
}

func TestRejectedGraphs(t *testing.T) {
	cases := []struct {
		name, source string
		mode         Mode
		message      string
	}{
		{"component as image", "extend\n", Build, "does not match"},
		{"image as component", "from \"debian\"\n", Publish, "does not match"},
		{"duplicate names", "from \"debian\" as=\"base\"\nextend as=\"base\"\n", Publish, "duplicate stage"},
		{"case-variant duplicate names", "from \"debian\" as=\"Base\"\nextend as=\"base\"\n", Publish, "duplicate stage"},
		{"dynamic alias", "from \"debian\" as=\"${name}\"\n", Build, "invalid stage name"},
		{"leading underscore alias", "from \"debian\" as=\"_base\"\n", Build, "invalid stage name"},
		{"unknown numeric copy stage", "extend\ncopy \"/a\" \"/b\" from=\"12\"\n", Publish, "unknown numeric stage"},
		{"copy cycle", "from \"debian\" as=\"a\"\ncopy \"/a\" \"/a\" from=\"b\"\nfrom \"a\" as=\"b\"\n", Build, "cycle"},
		{"from cycle", "from \"b\" as=\"a\"\nfrom \"a\" as=\"b\"\n", Build, "cycle"},
		{"unused cycle", "from \"b\" as=\"a\"\nfrom \"a\" as=\"b\"\nextend\n", Publish, "cycle"},
		{"package depends on extend", "extend as=\"base\"\npackage as=\"p\"\ncopy \"/a\" \"/a\" from=\"base\"\nfrom \"base\"\ncopy \"/a\" \"/a\" from=\"p\"\n", Publish, "depends on extend"},
		{"indirect extend dependency", "extend as=\"base\"\nfrom \"base\" as=\"builder\"\npackage as=\"p\"\ncopy \"/a\" \"/a\" from=\"builder\"\nfrom \"base\"\ncopy \"/a\" \"/a\" from=\"p\"\n", Publish, "depends on extend"},
		{"output replaces consuming image", "extend\nfrom \"debian\"\n", Publish, "must descend"},
		{"copy ancestry is insufficient", "extend as=\"base\"\nfrom \"scratch\"\ncopy \"/\" \"/\" from=\"base\"\n", Publish, "must descend"},
		{"package in image", "from \"debian\"\npackage as=\"p\"\n", Build, "require a component"},
		{"operation before base", "run \"echo hi\"\nfrom \"debian\"\n", Build, "requires a stage"},
		{"missing structural arg", "arg \"base\"\nfrom \"${base}\"\n", Build, "invalid from source"},
		{"undeclared structural arg", "from \"${base}\"\n", Build, "invalid from source"},
		{"bad argument name", "arg \"bad-name\" \"x\"\nfrom \"debian\"\n", Build, "invalid argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Create(parse(t, tc.source), Options{Mode: tc.mode})
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("want %q, got %v", tc.message, err)
			}
		})
	}
}

const parameterized = `arg "builder_image" "compiler:1"
from "${builder_image}" as="builder"
run "compile"
package as="tools"
copy "/out" "/" from="builder"
extend
arg "channel"
env CHANNEL="${channel}"
run "echo $channel; echo ${channel}"
copy "/tool" "/tool" from="tools"
`

func TestArgumentPhasesAndShellBoundary(t *testing.T) {
	publish := makePlan(t, parameterized, Options{Mode: Publish, Arguments: map[string]string{"builder_image": "compiler:2"}})
	if publish.PackageArguments["builder_image"] != "compiler:2" {
		t.Fatalf("metadata = %v", publish.PackageArguments)
	}
	p := invokePublished(t, publish, Options{Arguments: map[string]string{"channel": "preview; touch /bad"}})
	last := p.Stages[len(p.Stages)-1]
	if last.Operations[0].Properties["CHANNEL"] != "preview; touch /bad" {
		t.Fatal("env argument not expanded")
	}
	if last.Operations[1].Arguments[0] != "echo $channel; echo ${channel}" {
		t.Fatal("shell code was interpolated")
	}
	if last.Operations[1].ArgumentsInScope["channel"] != "preview; touch /bad" {
		t.Fatal("argument environment lost")
	}
	for _, a := range p.Arguments {
		if a.Name == "builder_image" && a.Phase != "package" {
			t.Fatal("builder_image not fixed")
		}
		if a.Name == "channel" && a.Phase != "invocation" {
			t.Fatal("channel not local")
		}
	}
}

func TestArgumentErrors(t *testing.T) {
	publication := makePlan(t, parameterized, Options{Mode: Publish})
	for _, tc := range []struct {
		name    string
		opts    Options
		message string
	}{
		{"override fixed argument", Options{Mode: Invoke, Arguments: map[string]string{"builder_image": "compiler:2", "channel": "stable"}}, "fixed at publication"},
		{"metadata override", Options{Mode: Invoke, PublishedArguments: map[string]string{"builder_image": "compiler:1"}}, "publication metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Instantiate(publication.Component, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("want %s, got %v", tc.message, err)
			}
		})
	}
	missingMetadata := *publication.Component
	missingMetadata.PackageArguments = nil
	if _, err := Instantiate(&missingMetadata, Options{Arguments: map[string]string{"channel": "stable"}}); err == nil || !strings.Contains(err.Error(), "published value") {
		t.Fatalf("missing fixed metadata accepted: %v", err)
	}
	invocation := invokePublished(t, publication, Options{})
	last := invocation.Stages[len(invocation.Stages)-1]
	if got := last.Operations[0].Properties["CHANNEL"]; got != "" {
		t.Fatalf("unset invocation argument expanded to %q", got)
	}
	for _, tc := range []struct {
		name    string
		opts    Options
		message string
	}{
		{"metadata on publication", Options{Mode: Publish, PublishedArguments: map[string]string{"builder_image": "compiler:1"}}, "only accepted in invoke"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Create(parse(t, parameterized), tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("want %s, got %v", tc.message, err)
			}
		})
	}
}

func TestForwardStageReferencesAreResolved(t *testing.T) {
	source := `from "later" as="first"
run "first"
from "debian" as="later"
run "later"
from "first"
`
	p := makePlan(t, source, Options{})
	if !reflect.DeepEqual(stageIDs(p), []string{"1", "0", "2"}) {
		t.Fatalf("order = %v", stageIDs(p))
	}
}

func TestNoPackagesAndAutoMode(t *testing.T) {
	p := makePlan(t, "extend\narg \"required\"\nrun \"echo $required\"\n", Options{})
	if p.Mode != Publish || len(p.Stages) != 0 || len(p.Outputs) != 0 {
		t.Fatalf("plan = %+v", p)
	}
	p = makePlan(t, "from \"debian\"\nrun \"hello\"\n", Options{})
	if p.Mode != Build || p.Inputs[0].Reference != "debian" {
		t.Fatalf("plan = %+v", p)
	}
}

func TestPlanIsDeterministicAndDoesNotMutateDefinition(t *testing.T) {
	d := parse(t, parameterized)
	before, _ := json.Marshal(d)
	options := Options{Mode: Publish}
	a, err := Create(d, options)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Create(d, options)
	if err != nil {
		t.Fatal(err)
	}
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	after, _ := json.Marshal(d)
	if string(aa) != string(bb) || string(before) != string(after) {
		t.Fatal("nondeterministic or mutating plan")
	}
}

func TestEmptyInvalidAndPlatformOptions(t *testing.T) {
	if _, err := Create(nil, Options{}); err == nil {
		t.Fatal("nil definition accepted")
	}
	d := parse(t, "from \"debian\"\n")
	for _, opts := range []Options{{Mode: "bad"}, {Platform: "linux"}, {Platform: "linux/"}, {Platform: "windows/amd64"}} {
		if _, err := Create(d, opts); err == nil {
			t.Fatalf("accepted %+v", opts)
		}
	}
	p, err := Create(d, Options{Platform: "linux/arm64"})
	if err != nil || p.Stages[0].Platform != "linux/arm64" {
		t.Fatalf("plan = %v err=%v", p, err)
	}
	p, err = Create(d, Options{Platform: "linux/arm/v7"})
	if err != nil || p.Stages[0].Platform != "linux/arm/v7" {
		t.Fatalf("variant plan = %v err=%v", p, err)
	}
	for _, platform := range []string{"windows/amd64", "darwin/arm64"} {
		_, err = Create(parse(t, `from "debian" platform="`+platform+`"`), Options{})
		if err == nil || !strings.Contains(err.Error(), "requires Linux") {
			t.Fatalf("non-Linux stage platform %q accepted: %v", platform, err)
		}
	}
}

func TestRunNetworkDefaultsExplicitly(t *testing.T) {
	implicit := makePlan(t, "from \"debian\"\nrun \"echo hi\"\n", Options{})
	explicit := makePlan(t, "from \"debian\"\nrun \"echo hi\" network=\"default\"\n", Options{})
	a := implicit.Stages[0].Operations[0]
	b := explicit.Stages[0].Operations[0]
	if a.Properties["network"] != "default" || b.Properties["network"] != "default" || a.NetworkExplicit || !b.NetworkExplicit {
		t.Fatalf("network mismatch: implicit=%+v explicit=%+v", a, b)
	}
}

func TestPlanPlatformFollowsSelectedOutput(t *testing.T) {
	p := makePlan(t, `from "debian" platform="linux/arm64" as="arm"
run "compile"
from "alpine" platform="linux/amd64" as="other"
`, Options{Mode: Build, Target: "arm", Platform: "linux/amd64"})
	if p.Platform != "linux/arm64" || p.Stages[0].Platform != "linux/arm64" {
		t.Fatalf("selected output platform = %q, stage = %+v", p.Platform, p.Stages)
	}
}

func TestExplicitArgumentReplacesUnevaluableDefault(t *testing.T) {
	source := `arg "base" "${unspecified}"
from "${base}"
`
	p := makePlan(t, source, Options{Arguments: map[string]string{"base": "debian"}})
	if p.Stages[0].Source != "debian" {
		t.Fatalf("source = %q", p.Stages[0].Source)
	}
}

func TestConflictingPublishedArgumentValues(t *testing.T) {
	source := `arg "x" "before"
from "compiler" as="builder"
run "echo $x"
arg "x" "after"
run "echo $x"
package as="assets"
copy "/out" "/" from="builder"
extend
copy "/out" "/out" from="assets"
`
	_, err := Create(parse(t, source), Options{Mode: Publish})
	if err == nil || !strings.Contains(err.Error(), "changes value") {
		t.Fatalf("expected incompatible argument metadata rejection, got %v", err)
	}
}

func TestArgumentDeclarationDoesNotFillEarlierRunRetroactively(t *testing.T) {
	source := `arg "x"
from "debian"
run "echo $x"
arg "x" "later"
run "echo $x"
`
	plan := makePlan(t, source, Options{})
	operations := plan.Stages[0].Operations
	if _, present := operations[0].ArgumentsInScope["x"]; present {
		t.Fatalf("first RUN saw later ARG value: %#v", operations[0].ArgumentsInScope)
	}
	if got := operations[1].ArgumentsInScope["x"]; got != "later" {
		t.Fatalf("second RUN ARG value = %q", got)
	}
}

func TestRepositoryExamples(t *testing.T) {
	count := 0
	err := filepath.WalkDir("../../examples", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".coopr" {
			return nil
		}
		count++
		t.Run(path, func(t *testing.T) {
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			d := parse(t, string(source))
			p, err := Create(d, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if p.Mode == Publish {
				invocation, err := Instantiate(p.Component, Options{})
				if err != nil {
					t.Fatal(err)
				}
				for _, stage := range invocation.Stages {
					if stage.Kind == "package" {
						t.Fatal("invocation contains executable package")
					}
				}
			}
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("no example definitions found")
	}
}

const targets = `extend as="runtime"
arg "flavor" "runtime"
run "configure-runtime"
extend as="debug"
arg "flavor" "debug"
run "configure-debug"
from "runtime" as="release"
run "finish-runtime"
`

func TestMultipleExtendTargets(t *testing.T) {
	for _, tc := range []struct {
		target string
		ids    []string
		flavor string
	}{
		{"runtime", []string{"0"}, "runtime"},
		{"debug", []string{"0"}, "debug"},
		{"release", []string{"0", "1"}, "runtime"},
		{"", []string{"0", "1"}, "runtime"},
	} {
		t.Run("target="+tc.target, func(t *testing.T) {
			publication := makePlan(t, targets, Options{Mode: Publish, Target: tc.target})
			p := invokePublished(t, publication, Options{})
			if !reflect.DeepEqual(stageIDs(p), tc.ids) {
				t.Fatalf("stages = %v", stageIDs(p))
			}
			if p.Target != publication.Target {
				t.Fatalf("invoked target = %q, published target = %q", p.Target, publication.Target)
			}
			if p.Stages[0].Operations[0].ArgumentsInScope["flavor"] != tc.flavor {
				t.Fatal("extend inherited another target's arguments")
			}
			if len(p.Stages[0].Dependencies) != 0 {
				t.Fatal("extend must start independently from consuming snapshot")
			}
		})
	}
	publish := makePlan(t, targets, Options{Mode: Publish})
	if len(publish.Stages) != 0 {
		t.Fatal("publication executes extend targets")
	}
}

func TestSelectContainerBuildTarget(t *testing.T) {
	p := makePlan(t, "from \"debian\" as=\"base\"\nrun \"base\"\nfrom \"alpine\"\nrun \"other\"\n", Options{Target: "base"})
	if p.Mode != Build || !reflect.DeepEqual(stageIDs(p), []string{"0"}) {
		t.Fatalf("plan = %+v", p)
	}
}

func TestStageAliasesAndTargetAreCaseInsensitive(t *testing.T) {
	p := makePlan(t, `from "scratch" as="Source"
run "prepare"
from "SOURCE" as="Final"
copy "/artifact" "/copied" from="sOuRcE"
run "consume" {
  mount "bind" from="SoUrCe" source="/" target="/source"
}
`, Options{Mode: Build, Target: "fInAl"})
	if p.Target != "Final" || !reflect.DeepEqual(stageIDs(p), []string{"0", "1"}) {
		t.Fatalf("mixed-case stage plan = %+v", p)
	}
	if !reflect.DeepEqual(p.Stages[1].Dependencies, []string{"0"}) {
		t.Fatalf("mixed-case stage dependencies = %v", p.Stages[1].Dependencies)
	}
	if p.Stages[1].Source != "SOURCE" || p.Stages[1].Operations[0].Properties["from"] != "sOuRcE" {
		t.Fatalf("authored spelling was not preserved: %+v", p.Stages[1])
	}
}

func TestComponentMetadataCanonicalizesStageReservations(t *testing.T) {
	publication := makePlan(t, `package as="Pkg"
extend as="Base"
copy "/artifact" "/artifact" from="pKg"
from "BASE" as="Final"
`, Options{Mode: Publish, Target: "fInAl"})
	if !reflect.DeepEqual(publication.Component.ReservedStageNames, []string{"base", "final", "pkg"}) {
		t.Fatalf("reserved stage names = %v", publication.Component.ReservedStageNames)
	}
	if _, err := Instantiate(publication.Component, Options{}); err != nil {
		t.Fatalf("case-insensitive component metadata rejected: %v", err)
	}
}

func TestUnselectedTargetDoesNotConsumeArguments(t *testing.T) {
	source := `extend as="runtime"
run "runtime"
extend as="debug"
arg "debug_option"
run "debug"
`
	p := invokePublished(t, makePlan(t, source, Options{Mode: Publish, Target: "runtime"}), Options{})
	if !reflect.DeepEqual(stageIDs(p), []string{"0"}) {
		t.Fatal("unselected target scheduled")
	}
	publication := makePlan(t, source, Options{Mode: Publish, Target: "debug"})
	debug := invokePublished(t, publication, Options{})
	if _, present := debug.Stages[0].Operations[0].ArgumentsInScope["debug_option"]; present {
		t.Fatal("unset ARG entered the selected target's RUN environment")
	}
}

func TestTargetSelectionErrors(t *testing.T) {
	source := `from "compiler" as="builder"
package as="tools"
copy "/out" "/" from="builder"
extend as="runtime"
extend as="debug"
`
	for _, tc := range []struct {
		mode            Mode
		target, message string
	}{
		{Publish, "missing", "unknown target"},
		{Publish, "builder", "must descend"},
		{Publish, "tools", "must descend"},
	} {
		t.Run(string(tc.mode)+tc.target, func(t *testing.T) {
			_, err := Create(parse(t, source), Options{Mode: tc.mode, Target: tc.target})
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("want %s; got %v", tc.message, err)
			}
		})
	}
}

func TestPackageCannotReachAnyExtend(t *testing.T) {
	for _, alias := range []string{"first", "second"} {
		source := `extend as="first"
extend as="second"
package as="bad"
copy "/data" "/data" from="` + alias + `"
from "first"
copy "/data" "/data" from="bad"
`
		_, err := Create(parse(t, source), Options{Mode: Publish})
		if err == nil || !strings.Contains(err.Error(), "depends on extend") {
			t.Fatalf("dependency on %s accepted: %v", alias, err)
		}
	}
}

func TestSelectedTargetKeepsPackageBoundary(t *testing.T) {
	source := `from "compiler" as="builder"
run "compile"
package as="tools"
copy "/out" "/" from="builder"
extend as="runtime"
copy "/binary" "/binary" from="tools"
extend as="debug"
run "debug-only"
`
	p := invokePublished(t, makePlan(t, source, Options{Mode: Publish, Target: "runtime"}), Options{})
	if !reflect.DeepEqual(stageIDs(p), []string{"0", "1"}) || p.Stages[0].Kind != "package-input" {
		t.Fatalf("plan = %+v", p)
	}
	p = invokePublished(t, makePlan(t, source, Options{Mode: Publish, Target: "debug"}), Options{})
	if !reflect.DeepEqual(stageIDs(p), []string{"0"}) {
		t.Fatalf("unused packages included: %v", stageIDs(p))
	}
}

func TestNamedTargetCanPrecedePrivateFinalStage(t *testing.T) {
	source := `extend as="runtime"
from "compiler" as="builder"
`
	makePlan(t, source, Options{Mode: Publish, Target: "runtime"})
	if _, err := Create(parse(t, source), Options{Mode: Publish}); err == nil {
		t.Fatal("default private output accepted")
	}
}

func TestComponentTargetIsAnArgumentProperty(t *testing.T) {
	source := `from "debian"
arg "selected" "debug"
component "registry.example.com/setup:v1" target="${selected}"
`
	p := makePlan(t, source, Options{})
	if len(p.Inputs) != 2 || p.Inputs[1].Reference != "registry.example.com/setup:v1" {
		t.Fatalf("component input = %+v", p.Inputs)
	}
	op := p.Stages[0].Operations[0]
	if len(op.Arguments) != 1 || op.Properties["target"] != "debug" {
		t.Fatalf("component argument = %+v", op)
	}
}

func TestPublishedTargetAndPlatformCannotBeChangedAtInvocation(t *testing.T) {
	publication := makePlan(t, targets, Options{Mode: Publish, Target: "debug", Platform: "linux/arm64"})
	if publication.Component.Platform != "linux/arm64" || publication.Component.Output != "0" {
		t.Fatalf("publication metadata = %+v", publication.Component)
	}
	if len(publication.Component.Definition.Instructions) != 3 {
		t.Fatalf("unrelated stages retained: %+v", publication.Component.Definition.Instructions)
	}
	p := invokePublished(t, publication, Options{})
	if p.Target != "debug" || p.Platform != "linux/arm64" {
		t.Fatalf("invocation = %+v", p)
	}
	for _, tc := range []struct {
		name    string
		opts    Options
		message string
	}{
		{"target", Options{Target: "runtime"}, "target selection is not supported"},
		{"platform", Options{Platform: "linux/amd64"}, "must match published platform"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Instantiate(publication.Component, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("want %q, got %v", tc.message, err)
			}
		})
	}
	if _, err := Create(parse(t, targets), Options{Mode: Invoke}); err == nil || !strings.Contains(err.Error(), "publication plan") {
		t.Fatalf("raw invocation accepted: %v", err)
	}
}

func TestMalformedPublishedDefinitionIsRejected(t *testing.T) {
	component := &PublishedComponent{
		Definition: &definition.Definition{Instructions: []definition.Instruction{{Name: "from"}, {Name: "extend"}}},
		Output:     "1",
		Platform:   "linux/amd64",
	}
	if _, err := Instantiate(component, Options{}); err == nil {
		t.Fatal("malformed decoded definition accepted")
	}
}

func TestPublishedFromBindingsRejectTopologyChanges(t *testing.T) {
	for _, tc := range []struct {
		name, source, override, message string
	}{
		{"image to retained stage", `arg "source" "debian"
from "${source}" as="builder"
extend as="base"
copy "/file" "/file" from="builder"
`, "base", "FROM binding changed"},
		{"retained stage to image", `arg "source" "base"
from "debian" as="base"
from "${source}" as="builder"
extend
copy "/file" "/file" from="builder"
`, "ubuntu", "FROM binding changed"},
		{"pruned stage alias", `arg "source" "debian"
from "${source}" as="builder"
extend as="used"
copy "/file" "/file" from="builder"
extend as="discarded"
`, "discarded", "was a stage name at publication"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{Mode: Publish}
			if tc.name == "pruned stage alias" {
				opts.Target = "used"
			}
			publish := makePlan(t, tc.source, opts)
			encoded, err := json.Marshal(publish.Component)
			if err != nil {
				t.Fatal(err)
			}
			var decoded PublishedComponent
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			_, err = Instantiate(&decoded, Options{Arguments: map[string]string{"source": tc.override}})
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("want %q, got %v", tc.message, err)
			}
		})
	}
}

func TestPublishedExternalImageArgumentMayChange(t *testing.T) {
	source := `arg "source" "debian"
from "${source}" as="builder"
extend
copy "/file" "/file" from="builder"
`
	publish := makePlan(t, source, Options{Mode: Publish})
	invoke := invokePublished(t, publish, Options{Arguments: map[string]string{"source": "ubuntu"}})
	if invoke.Stages[0].Source != "ubuntu" || invoke.Stages[0].Kind != "from" {
		t.Fatalf("external image argument did not change: %+v", invoke.Stages)
	}
}

func TestPublishedStageReferencesCannotChange(t *testing.T) {
	for _, operation := range []string{
		`copy "/file" "/file" from="${source}"`,
		`run "cat /file" {
 mount "bind" from="${source}" source="/file" target="/file"
}`,
	} {
		source := `arg "source" "a"
from "debian" as="a"
from "alpine" as="b"
extend
` + operation + `
copy "/other" "/other" from="b"
`
		publish := makePlan(t, source, Options{Mode: Publish})
		_, err := Instantiate(publish.Component, Options{Arguments: map[string]string{"source": "b"}})
		if err == nil || !strings.Contains(err.Error(), "stage copy or mount references changed") {
			t.Fatalf("changed stage dependency accepted: %v", err)
		}
	}
}

func TestOperationLocalInputContexts(t *testing.T) {
	for _, tc := range []struct {
		mode    Mode
		source  string
		context string
	}{
		{Build, `from "debian"
copy "./local" "/dest"
add "./archive.tar" "/dest"
run "cat /src" {
 mount "bind" source="./local" target="/src"
}
`, "build"},
		{Publish, `package as="assets"
copy "./local" "/dest"
extend
copy "/dest" "/dest" from="assets"
`, "publisher"},
	} {
		p := makePlan(t, tc.source, Options{Mode: tc.mode})
		ops := p.Stages[0].Operations
		if ops[0].InputContext != tc.context {
			t.Fatalf("%s copy context = %+v", tc.mode, ops[0])
		}
		if tc.mode == Build {
			if ops[1].InputContext != "build" || ops[2].MountContexts[0] != "build" {
				t.Fatalf("build input contexts = %+v", ops)
			}
		}
	}
	source := `extend
copy "./caller-file" "/dest"
run "cat /src" {
 mount "bind" source="./caller-file" target="/src"
}
`
	publish := makePlan(t, source, Options{Mode: Publish})
	p := invokePublished(t, publish, Options{})
	if p.Stages[0].Operations[0].InputContext != "caller" || p.Stages[0].Operations[1].MountContexts[0] != "caller" {
		t.Fatalf("invocation input contexts = %+v", p.Stages[0].Operations)
	}
	if p.Stages[0].Operations[0].Properties["from"] != "" {
		t.Fatal("local context was converted into a stage reference")
	}
}

func TestNormalizeParameters(t *testing.T) {
	a, err := NormalizeParameters(map[string]string{"z": "a", "a": "", "unicode": "世界"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NormalizeParameters(map[string]string{"unicode": "世界", "a": "", "z": "a"})
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) || string(a) != `{"a":"","unicode":"世界","z":"a"}` {
		t.Fatalf("normalization differs: %s / %s", a, b)
	}
	emptyValue, err := NormalizeParameters(map[string]string{"a": ""})
	if err != nil {
		t.Fatal(err)
	}
	absent, err := NormalizeParameters(nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(emptyValue) == string(absent) {
		t.Fatal("missing parameter collapsed into an empty value")
	}
	validReplacement, err := NormalizeParameters(map[string]string{"name": "�"})
	if err != nil || len(validReplacement) == 0 {
		t.Fatalf("valid replacement character rejected: %v", err)
	}
	for _, parameters := range []map[string]string{
		{string([]byte{0xff}): "value"},
		{"name": string([]byte{0xff})},
	} {
		if _, err := NormalizeParameters(parameters); err == nil || !strings.Contains(err.Error(), "UTF-8") {
			t.Fatalf("invalid UTF-8 accepted: %v", err)
		}
	}
}

func TestPlannerRejectsInvalidParameterEncoding(t *testing.T) {
	invalid := string([]byte{0xff})
	for _, params := range []map[string]string{{invalid: "value"}, {"x": invalid}} {
		_, err := Create(parse(t, `arg "x" "default"
from "debian"
`), Options{Arguments: params})
		if err == nil || !strings.Contains(err.Error(), "UTF-8") {
			t.Fatalf("invalid direct argument accepted: %v", err)
		}
	}
	publication := makePlan(t, parameterized, Options{Mode: Publish})
	metadata := *publication.Component
	metadata.PackageArguments = map[string]string{"builder_image": invalid}
	_, err := Instantiate(&metadata, Options{Arguments: map[string]string{"channel": "stable"}})
	if err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid publication metadata accepted: %v", err)
	}
	_, err = Instantiate(publication.Component, Options{Arguments: map[string]string{"channel": "�"}})
	if err != nil {
		t.Fatalf("valid replacement character rejected: %v", err)
	}
}
