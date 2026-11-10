package planner

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/buildcontext"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/onbuildparse"
)

func residualPublish(t *testing.T, source string) *Plan {
	t.Helper()
	p, e := CreateDemandDriven(parse(t, source), Options{Mode: Publish}, func(FromSource) (StageBind, error) { return StageBind{}, nil })
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func residualInvoke(t *testing.T, c *PublishedComponent, opts Options, binds map[string]StageBind, resolve StageBindResolver) *Plan {
	t.Helper()
	if resolve == nil {
		resolve = func(FromSource) (StageBind, error) { return StageBind{}, nil }
	}
	p, e := InstantiateDemandDriven(c, opts, binds, resolve)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func residualStage(t *testing.T, p *Plan, id string) Stage {
	t.Helper()
	for _, s := range p.Stages {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("missing stage %s in %+v", id, p.Stages)
	return Stage{}
}
func residualBind(t *testing.T, trigger string) StageBind {
	t.Helper()
	ins, e := onbuildparse.ParseDeferred(trigger)
	if e != nil {
		t.Fatal(e)
	}
	return StageBind{Inherited: ins}
}

func TestResidualFromLateTopology(t *testing.T) {
	p := residualPublish(t, `arg "base" "a"; from "scratch" as="a" { env "VALUE" "a"; }; from "scratch" as="b" { env "VALUE" "b"; }; extend as="result" { from "${base}" as="selected" { env "OBSERVED" "${VALUE:-empty}"; }; }`)
	for _, test := range []struct{ source, dependency, value string }{
		{"a", "0", "a"}, {"b", "1", "b"}, {"0", "0", "a"}, {"1", "1", "b"}, {"scratch", "", "empty"}, {"registry.example/image:stable", "", "external"},
	} {
		t.Run(test.source, func(t *testing.T) {
			plan := residualInvoke(t, p.Component, Options{Arguments: map[string]string{"base": test.source}}, nil, func(src FromSource) (StageBind, error) {
				if src.Kind != FromSourceImage || src.Source != "registry.example/image:stable" {
					t.Fatalf("unexpected image resolution %+v", src)
				}
				return StageBind{BaseEnvironment: map[string]string{"VALUE": "external"}}, nil
			})
			final := residualStage(t, plan, "3")
			want := []string{}
			if test.dependency != "" {
				want = append(want, test.dependency)
			}
			if !slices.Equal(final.Dependencies, want) || final.Source != test.source || !slices.Equal(plan.Outputs, []string{"3"}) {
				t.Fatalf("wrong selected graph: %+v, output=%v", final, plan.Outputs)
			}
			if len(final.Operations) != 1 || final.Operations[0].Arguments[1] != test.value {
				t.Fatalf("wrong inherited environment: %+v", final.Operations)
			}
		})
	}
}

func TestResidualInvocationContextOverridesLocalStage(t *testing.T) {
	p := residualPublish(t, `arg "src" "a"
from "scratch" as="a" {}
extend { copy "/x" "/x" from="${src}"; run "true" { mount "bind" from="${src}" target="/input"; }; }
`)
	// This is the established context boundary, not automatic caller-stage support.
	spec := buildcontext.Spec{Name: "a", Kind: buildcontext.DockerImage, Reference: "sha256:" + strings.Repeat("a", 64)}
	plan := residualInvoke(t, p.Component, Options{BuildContexts: []buildcontext.Spec{spec}}, nil, nil)
	s := residualStage(t, plan, "1")
	if s.Operations[0].InputContext != "a" || s.Operations[1].MountContexts[0] != "a" || !reflect.DeepEqual(stageIDs(plan), []string{"1"}) {
		t.Fatal(s, stageIDs(plan))
	}
}

func TestResidualLocalAndPackageOnbuildLateSelection(t *testing.T) {
	for _, kind := range []string{"local", "package"} {
		t.Run(kind, func(t *testing.T) {
			source := `arg "src" "a"
from "scratch" as="a" {}
from "scratch" as="b" {}
`
			binds := map[string]StageBind{}
			if kind == "local" {
				source += `from "scratch" as="base" { onbuild { copy "/x" "/x" from="${src}"; }; }
`
			} else {
				source += `package as="base" { onbuild { copy "/x" "/x" from="${src}"; }; }
`
				binds["2"] = residualBind(t, "COPY --from=${src} /x /x")
			}
			source += `extend { from "base" as="result" {}; }`
			p := residualPublish(t, source)
			plan := residualInvoke(t, p.Component, Options{Arguments: map[string]string{"src": "b"}}, binds, nil)
			s := residualStage(t, plan, "4")
			if s.Operations[0].Properties["from"] != "b" || !slices.Contains(s.Dependencies, "1") {
				t.Fatal(s)
			}
		})
	}
}

func TestResidualSelectedPackagesRejectUnavailableWithoutProducer(t *testing.T) {
	p := residualPublish(t, `arg "base" "one"
package as="one" { copy "/DO_NOT_REPLAY_ONE" "/x"; }
package as="two" { copy "/DO_NOT_REPLAY_TWO" "/x"; }
extend { from "${base}" as="result" { env "OBSERVED" "${VALUE}"; }; }
`)
	if !reflect.DeepEqual(p.Outputs, []string{"0"}) {
		t.Fatal(p.Outputs)
	}
	data, _ := json.Marshal(p.Component)
	if strings.Contains(string(data), "DO_NOT_REPLAY") {
		t.Fatal("producer body survived")
	}
	if !p.Component.Definition.Instructions[2].UnavailablePackage {
		t.Fatal("missing unavailable leaf")
	}
	binds := map[string]StageBind{"0": {BaseEnvironment: map[string]string{"VALUE": "one"}}}
	plan := residualInvoke(t, p.Component, Options{}, binds, nil)
	if !reflect.DeepEqual(stageIDs(plan), []string{"0", "3"}) {
		t.Fatal(stageIDs(plan))
	}
	s := residualStage(t, plan, "3")
	if s.Operations[0].Arguments[1] != "one" {
		t.Fatal(s)
	}
	_, err := InstantiateDemandDriven(p.Component, Options{Arguments: map[string]string{"base": "two"}}, binds, func(FromSource) (StageBind, error) { return StageBind{}, nil })
	if err == nil || !strings.Contains(err.Error(), "was not captured") {
		t.Fatal(err)
	}
	p.Component.Definition.Instructions[2].Children = []definition.Instruction{{Name: "run", Arguments: []string{"REBUILD"}}}
	if _, err := ValidatePublished(p.Component); err == nil {
		t.Fatal("unavailable producer accepted")
	}
}
func TestResidualSelectedPackagesAvoidExtraFrozenArguments(t *testing.T) {
	p := residualPublish(t, `arg "extra" "frozen"
package as="used" { copy "/x" "/x"; }
package as="unused" { arg "extra"; run "echo ${extra}"; }
extend { copy "/x" "/x" from="used"; }
`)
	if len(p.PackageArguments) != 0 {
		t.Fatal(p.PackageArguments)
	}
	residualInvoke(t, p.Component, Options{Arguments: map[string]string{"extra": "changed"}}, map[string]StageBind{"0": {}}, nil)
}
func TestResidualCallerStageBoundaryNotAutomatic(t *testing.T) {
	p := residualPublish(t, `arg "src" "example.invalid/default:latest"
extend { copy "/x" "/x" from="${src}"; }`)
	var observed []FromSource
	resolve := func(s FromSource) (StageBind, error) { observed = append(observed, s); return StageBind{}, nil }
	residualInvoke(t, p.Component, Options{Arguments: map[string]string{"src": "caller-stage"}}, nil, resolve)
	if len(observed) != 1 || observed[0].Kind != FromSourceCopyImage {
		t.Fatal("bare argument unexpectedly imported caller stage", observed)
	}
	observed = nil
	spec := buildcontext.Spec{Name: "caller-stage", Kind: buildcontext.DockerImage, Reference: "sha256:" + strings.Repeat("b", 64)}
	plan := residualInvoke(t, p.Component, Options{Arguments: map[string]string{"src": "caller-stage"}, BuildContexts: []buildcontext.Spec{spec}}, nil, resolve)
	if len(observed) != 1 || observed[0].Kind != FromSourceContext || plan.Contexts[0].Reference != spec.Reference {
		t.Fatal(observed, plan.Contexts)
	}
	t.Log("Explicit existing context path works. Automatic caller scheduling, source export, lifetime and cache scoping are NOT implemented.")
}

func TestResidualUnusedSourceDoesNotResolveAtInvocation(t *testing.T) {
	p := residualPublish(t, `from "${missing}" as="unused" {}; extend { env "OK" "yes"; }`)
	plan, err := Instantiate(p.Component, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stageIDs(plan), []string{"1"}) {
		t.Fatal(stageIDs(plan))
	}
}

func TestResidualDependencyInventoryUsesSelectedClosure(t *testing.T) {
	for _, dynamic := range []bool{false, true} {
		t.Run(fmt.Sprint(dynamic), func(t *testing.T) {
			selector := "used"
			prefix := ""
			if dynamic {
				selector = "${choice}"
				prefix = `arg "choice" "used"; `
			}
			p := residualPublish(t, prefix+`from "scratch" as="unused" { component "unavailable.invalid/unselected:stable"; }
from "scratch" as="used" { component "registry.example/selected:stable"; }
extend { copy "/x" "/x" from="`+selector+`"; component "registry.example/first:stable"; component "registry.example/later:stable"; }`)
			got, err := InspectPublishedDependencies(p.Component, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := map[Input]bool{{Kind: "component", Reference: "registry.example/selected:stable"}: true, {Kind: "component", Reference: "registry.example/first:stable"}: true, {Kind: "component", Reference: "registry.example/later:stable"}: true}
			if len(got.Inputs) != len(want) {
				t.Fatalf("inputs=%+v", got)
			}
			for _, v := range got.Inputs {
				if !want[v] {
					t.Fatalf("unexpected dependency %+v", v)
				}
			}
			if got.Incomplete != dynamic {
				t.Fatalf("incomplete=%t, dynamic=%t", got.Incomplete, dynamic)
			}
		})
	}
}

func TestResidualDependencyInventoryConsumesOnlyActiveSnapshotTriggers(t *testing.T) {
	for _, mode := range []string{"copy", "from", "dynamic-from"} {
		t.Run(mode, func(t *testing.T) {
			source := `arg "input" "registry.example/trigger:stable"; package as="pkg" { copy "/CONTROLLED_NONEXISTENT_PUBLISHER_FILE" "/x"; }; extend { `
			if mode == "copy" {
				source += `copy "/x" "/x" from="pkg"; }`
			} else {
				source += `from "pkg" { arg "input"; }; }`
			}
			p := residualPublish(t, source)
			trigger := "COPY --from=registry.example/trigger:stable /x /x"
			if mode == "dynamic-from" {
				trigger = "COPY --from=${input} /x /x"
			}
			bind := residualBind(t, trigger)
			bind.BaseEnvironment = map[string]string{"input": "registry.example/trigger:stable"}
			binds := map[string]StageBind{"0": bind}
			before, _ := json.Marshal(binds)
			got, err := InspectPublishedDependencies(p.Component, binds)
			if err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(binds)
			if string(before) != string(after) {
				t.Fatal("inspection mutated verified package metadata")
			}
			if mode == "copy" {
				if len(got.Inputs) != 0 || got.Incomplete {
					t.Fatalf("copy activated snapshot triggers: %+v", got)
				}
			} else {
				if len(got.Inputs) != 1 || got.Inputs[0].Reference != "registry.example/trigger:stable" {
					t.Fatalf("trigger missing: %+v", got)
				}
				if got.Incomplete != (mode == "dynamic-from") {
					t.Fatalf("trigger uncertainty: %+v", got)
				}
			}
		})
	}
}

func TestResidualDependencyInventoryDynamicPackageSelectionIsIncomplete(t *testing.T) {
	p := residualPublish(t, `arg "choice" "used"; package as="used" { copy "/CONTROLLED_MISSING_FILE" "/x"; }; package as="other" { run "must never execute during inspection"; }; extend { copy "/x" "/x" from="${choice}"; }`)
	binds := map[string]StageBind{}
	for _, id := range p.Outputs {
		binds[id] = StageBind{}
	}
	got, err := InspectPublishedDependencies(p.Component, binds)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Incomplete || len(got.Inputs) != 0 {
		t.Fatalf("ARG-selected package inventory=%+v", got)
	}
}

func TestResidualDependencyInventoryDynamicAfterIsIncomplete(t *testing.T) {
	p := residualPublish(t, `arg "pick" "a"; from "scratch" as="a" { component "registry.example/a:stable"; }; from "scratch" as="b" { component "registry.example/b:stable"; }; extend { from "scratch" after="${pick}" {}; }`)
	got, err := InspectPublishedDependencies(p.Component, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Incomplete || len(got.Inputs) != 1 || got.Inputs[0].Reference != "registry.example/a:stable" {
		t.Fatalf("ARG-selected ordering edge inventory=%+v", got)
	}
}

func TestResidualDependencyInventoryDynamicMountTypeIsIncomplete(t *testing.T) {
	p := residualPublish(t, `arg "kind" "bind"; arg "src" "registry.example/default:stable"; extend { run "true" { mount "${kind}" from="${src}" target="/input"; }; }`)
	got, err := InspectPublishedDependencies(p.Component, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Incomplete || len(got.Inputs) != 1 || got.Inputs[0].Reference != "registry.example/default:stable" {
		t.Fatalf("ARG-selected mount inventory=%+v", got)
	}
}

func TestResidualUnusedFixedPackageArguments(t *testing.T) {
	p := residualPublish(t, `arg "base" "pkg"; package as="pkg" { arg "private" "fixed"; copy "${private}" "/x"; }; extend { from "${base}" {}; }`)
	if p.Component.PackageArguments["private"] != "fixed" {
		t.Fatalf("fixed inputs=%v", p.Component.PackageArguments)
	}
	binds := map[string]StageBind{"0": {}}
	for _, base := range []string{"scratch", "registry.example/independent:stable"} {
		plan := residualInvoke(t, p.Component, Options{Arguments: map[string]string{"base": base}}, binds, nil)
		if slices.Contains(stageIDs(plan), "0") {
			t.Fatal("unused package selected", stageIDs(plan))
		}
		_, err := InstantiateDemandDriven(p.Component, Options{Arguments: map[string]string{"base": base, "private": "changed"}}, binds, func(FromSource) (StageBind, error) { return StageBind{}, nil })
		if err == nil || !strings.Contains(err.Error(), "fixed at publication") {
			t.Fatalf("unused package override=%v", err)
		}
	}
}

func TestResidualCapturedPackagesSwitchConfiguration(t *testing.T) {
	p := residualPublish(t, `arg "base" "one"; package as="one" { copy "publisher-one" "/x"; }; package as="two" { copy "publisher-two" "/x"; }; extend { from "${base}" { copy "/x" "/one" from="one"; copy "/x" "/two" from="two"; env "OBSERVED" "${VALUE}"; }; }`)
	if !slices.Equal(p.Outputs, []string{"0", "1"}) {
		t.Fatalf("both selected snapshots not captured: %v", p.Outputs)
	}
	binds := map[string]StageBind{}
	for index, name := range []string{"one", "two"} {
		bind := residualBind(t, "ENV TRIGGER="+name)
		bind.BaseEnvironment = map[string]string{"VALUE": name}
		binds[fmt.Sprint(index)] = bind
	}
	for index, name := range []string{"one", "two"} {
		t.Run(name, func(t *testing.T) {
			plan := residualInvoke(t, p.Component, Options{Arguments: map[string]string{"base": name}}, binds, nil)
			final := residualStage(t, plan, "3")
			if !slices.Contains(final.Dependencies, fmt.Sprint(index)) || len(final.Dependencies) != 2 || final.Source != name || !final.InheritedOnBuildPlanned {
				t.Fatalf("wrong package base: %+v", final)
			}
			if len(final.Operations) != 4 || final.Operations[0].Name != "env" || final.Operations[0].Arguments[1] != name || final.Operations[3].Arguments[1] != name {
				t.Fatalf("wrong verified package ENV/ONBUILD: %+v", final.Operations)
			}
			for _, stage := range plan.Stages {
				if stage.Kind == "package-input" && len(stage.Operations) != 0 {
					t.Fatalf("replayed package producer: %+v", stage)
				}
			}
		})
	}
}

func TestResidualFixedArgumentsNeedCapturedSnapshot(t *testing.T) {
	p := residualPublish(t, `extend { env "VALUE" "ok"; }`)
	p.Component.PackageArguments = map[string]string{"private": "fixed"}
	if _, err := ValidatePublished(p.Component); err == nil {
		t.Fatal("fixed arguments without any captured package accepted")
	}
}

func TestResidualSharedProjectionIsReadOnly(t *testing.T) {
	p := residualPublish(t, `from "scratch" as="helper" { copy "/x" { file "x" "fixture"; }; }; package as="pkg" { copy "publisher" "/x"; }; extend { from "pkg" { arg "input" "helper"; layer { run "true" { mount "bind" from="${input}" target="/input"; }; copy "/inline/" { file "text" "${VALUE}\n"; }; }; component "registry.example/setup:stable"; env "RESULT" "${VALUE}"; }; }`)
	projected, err := ProjectPublished(p.Component)
	if err != nil {
		t.Fatal(err)
	}
	before, err := ProjectPublished(p.Component)
	if err != nil {
		t.Fatal(err)
	}
	inherited := append(residualBind(t, "ARG input=helper").Inherited, residualBind(t, "COPY --from=${input} /x /inherited").Inherited...)
	binds := map[string]StageBind{"1": {Inherited: inherited}}
	for _, await := range []bool{true, false} {
		for _, value := range []string{"one", "two"} {
			_, err := instantiate(p.Component, Options{AwaitComponentReturns: await, CallerEnvironment: map[string]string{"VALUE": value}, packageBinds: binds}, func(FromSource) (StageBind, error) { return StageBind{}, nil }, projected)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(projected, before) {
				t.Fatal("planning mutated the shared projected instructions or order metadata")
			}
		}
	}
}
