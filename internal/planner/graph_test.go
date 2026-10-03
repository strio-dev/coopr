package planner

import (
	"reflect"
	"strings"
	"testing"
)

func TestProjectStandaloneRetainsRawArgumentsAndOrder(t *testing.T) {
	def := parse(t, `
arg "base" "alpine:3.20"
from "${base}" as="first"
arg "value" ""
run "echo ${value}" {
  mount "cache" target="/cache" id="${value}"
}
from "first" as="second"
arg "value" "later"
copy "/a" "/b" from="first"
`)
	graph, err := ProjectStandalone(def, "second")
	if err != nil {
		t.Fatal(err)
	}
	if graph.Output != "1" || len(graph.Stages) != 2 || graph.Stages[0].ID != "0" || graph.Stages[1].ID != "1" {
		t.Fatalf("stage projection = %+v", graph)
	}
	if got := graph.Globals[0].Arguments; !reflect.DeepEqual(got, []string{"base", "alpine:3.20"}) {
		t.Fatalf("global ARG = %v", got)
	}
	if got := graph.Stages[0].Head.Arguments[0]; got != "${base}" {
		t.Fatalf("FROM expression was expanded: %q", got)
	}
	if got := graph.Stages[0].Body[0].Arguments; !reflect.DeepEqual(got, []string{"value", ""}) {
		t.Fatalf("stage ARG = %v", got)
	}
	if got := graph.Stages[0].Body[1].Children[0].Properties["id"]; got != "${value}" {
		t.Fatalf("mount expression was expanded: %q", got)
	}
	if len(graph.Stages[1].Dependencies) != 0 {
		t.Fatalf("standalone dependencies were bound before frontend probe: %v", graph.Stages[1].Dependencies)
	}
	graph.Globals[0].Arguments[1] = "changed"
	graph.Stages[0].Body[1].Children[0].Properties["id"] = "changed"
	if def.Instructions[0].Arguments[1] != "alpine:3.20" || def.Instructions[3].Children[0].Properties["id"] != "${value}" {
		t.Fatal("projection mutated source definition")
	}
}

func TestProjectPublishedUsesReindexedBindingsAndPackageLeaves(t *testing.T) {
	def := parse(t, `
arg "image" "alpine:3.20"
from "scratch" as="unused"
package as="pkg"
arg "fixed" "value"
run "touch /package"
extend as="base"
arg "caller" "default"
copy "/package" "/package" from="pkg"
from "base" as="final"
arg "caller" "again"
run "test -f /package" {
  mount "bind" from="pkg" source="/package" target="/input"
}
`)
	publication, err := Create(def, Options{Mode: Publish})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := ProjectPublished(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	if graph.Output != "2" || graph.Platform != "linux/amd64" || !reflect.DeepEqual(graph.PackageLeaves, []string{"0"}) {
		t.Fatalf("published output/package projection = %+v", graph)
	}
	if len(graph.Stages) != 3 || graph.Stages[0].Kind != "package" || graph.Stages[1].Kind != "extend" || graph.Stages[2].Kind != "from" {
		t.Fatalf("pruned stages = %+v", graph.Stages)
	}
	if !reflect.DeepEqual(graph.Stages[1].Dependencies, []string{"0"}) || !reflect.DeepEqual(graph.Stages[2].Dependencies, []string{"1", "0"}) {
		t.Fatalf("published dependencies = %+v", graph.Stages)
	}
	if len(graph.StageReferences) != 2 || graph.StageReferences[0].Stage != "1" || graph.StageReferences[0].Operation != 0 || graph.StageReferences[0].Target != "0" || graph.StageReferences[1].Stage != "2" || graph.StageReferences[1].Operation != 0 || graph.StageReferences[1].Target != "0" {
		t.Fatalf("ARG-aware references = %+v", graph.StageReferences)
	}
	if graph.Stages[1].Body[0].Arguments[1] != "default" || graph.Stages[2].Body[0].Arguments[1] != "again" {
		t.Fatalf("stage ARG defaults lost: %+v", graph.Stages)
	}
	graph.FromBindings["2"] = FromBinding{Kind: "image"}
	graph.PackageArguments["fixed"] = "changed"
	graph.Stages[2].Body[1].Children[0].Properties["target"] = "/changed"
	if publication.Component.FromBindings["2"].Kind != "stage" || publication.Component.PackageArguments["fixed"] != "value" || publication.Component.Definition.Instructions[len(publication.Component.Definition.Instructions)-1].Children[0].Properties["target"] != "/input" {
		t.Fatal("projection mutated publication metadata")
	}
}

func TestProjectRawRejectsWrongDefinitionKindsAndInvalidPlatform(t *testing.T) {
	if _, err := ProjectStandalone(parse(t, `extend as="base"`), ""); err == nil || !strings.Contains(err.Error(), "cannot contain extend") {
		t.Fatalf("standalone accepted extend: %v", err)
	}
	if _, err := ProjectStandalone(parse(t, `from "scratch" as="base"`), "unknown"); err == nil || !strings.Contains(err.Error(), "unknown target") {
		t.Fatalf("standalone accepted unknown target: %v", err)
	}
	publication, err := Create(parse(t, `extend as="base"`), Options{Mode: Publish})
	if err != nil {
		t.Fatal(err)
	}
	publication.Component.Platform = ""
	if _, err := ProjectPublished(publication.Component); err == nil || !strings.Contains(err.Error(), "platform") {
		t.Fatalf("published graph accepted empty platform: %v", err)
	}
}

func TestProjectStandaloneMatchesStageTargetsCaseInsensitively(t *testing.T) {
	graph, err := ProjectStandalone(parse(t, `
from "scratch" as="Build"
from "Build" as="Final"
`), "fInAl")
	if err != nil {
		t.Fatal(err)
	}
	if graph.Output != "1" {
		t.Fatalf("case-insensitive target selected output %q, want 1", graph.Output)
	}
	if graph.Stages[0].Name != "Build" || graph.Stages[0].Head.Properties["as"] != "Build" {
		t.Fatalf("authored stage spelling was not preserved: %+v", graph.Stages[0])
	}
}

func TestProjectRawRejectsStageAliasesDifferingOnlyByCase(t *testing.T) {
	_, err := ProjectStandalone(parse(t, `
from "scratch" as="Build"
from "scratch" as="build"
`), "")
	if err == nil || !strings.Contains(err.Error(), "duplicate stage name") {
		t.Fatalf("case-variant duplicate aliases accepted: %v", err)
	}
}

func TestProjectPublicationRawRetainsAuthoredStagesUntilFrontendBinding(t *testing.T) {
	def := parse(t, `
arg "base" "scratch"
from "scratch" as="unused"
package as="pkg"
extend as="caller"
from "caller" as="selected"
from "scratch" as="later"
`)
	graph, err := ProjectPublicationRaw(def, "selected")
	if err != nil {
		t.Fatal(err)
	}
	if graph.Output != "3" || len(graph.Stages) != 5 || graph.Stages[0].Name != "unused" || graph.Stages[4].Name != "later" {
		t.Fatalf("raw publication graph = %+v", graph)
	}
	if graph.Stages[3].Head.Arguments[0] != "caller" || len(graph.Stages[3].Dependencies) != 0 {
		t.Fatalf("FROM was bound before frontend probe: %+v", graph.Stages[3])
	}
	graph.Stages[3].Head.Arguments[0] = "changed"
	if def.Instructions[4].Arguments[0] != "caller" {
		t.Fatal("publication projection mutated source definition")
	}
	if _, err := ProjectPublicationRaw(parse(t, `from "scratch"`), ""); err == nil || !strings.Contains(err.Error(), "requires extend") {
		t.Fatalf("container projected as component: %v", err)
	}
}
