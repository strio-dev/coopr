package planner

import (
	"reflect"
	"strings"
	"testing"
)

func TestProjectStandaloneRetainsRawArgumentsAndOrder(t *testing.T) {
	def := parse(t, `
arg "base" "alpine:3.20"
from "${base}" as="first" {
    arg "value" ""
    run "echo ${value}" {
      mount "cache" target="/cache" id="${value}"
    }
}
from "first" as="second" {
    arg "value" "later"
    copy "/a" "/b" from="first"
}
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
	if def.Instructions[0].Arguments[1] != "alpine:3.20" || def.Instructions[1].Children[1].Children[0].Properties["id"] != "${value}" {
		t.Fatal("projection mutated source definition")
	}
}

func TestProjectPublishedPreservesOriginalNamespaceAndPackageLeaves(t *testing.T) {
	def := parse(t, `
arg "image" "alpine:3.20"
from "scratch" as="unused" {}
package as="pkg" {
    arg "fixed" "value"
    run "touch /package"
}
extend as="base" {
    arg "caller" "default"
    copy "/package" "/package" from="pkg"
}
extend as="final" {
    from "base" {
        arg "caller" "again"
        run "test -f /package" {
          mount "bind" from="pkg" source="/package" target="/input"
        }
    }
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
	if graph.Output != "4" || graph.Platform != "linux/amd64" || !reflect.DeepEqual(graph.PackageLeaves, []string{"1"}) {
		t.Fatalf("published output/package projection = %+v", graph)
	}
	if len(graph.Stages) != 5 || graph.Stages[0].Name != "unused" || graph.Stages[1].Kind != "package" || graph.Stages[4].Kind != "from" {
		t.Fatalf("original stages = %+v", graph.Stages)
	}
	for _, stage := range graph.Stages {
		if len(stage.Dependencies) != 0 {
			t.Fatalf("projection bound dependencies before invocation: %+v", stage)
		}
	}
	if graph.Stages[2].Body[0].Arguments[1] != "default" || graph.Stages[4].Body[0].Arguments[1] != "again" {
		t.Fatalf("stage ARG defaults lost: %+v", graph.Stages)
	}
	invocation, err := Instantiate(publication.Component, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stageIDs(invocation), []string{"1", "2", "4"}) || !reflect.DeepEqual(invocation.Stages[2].Dependencies, []string{"2", "1"}) {
		t.Fatalf("invocation dependencies = %+v", invocation.Stages)
	}
	graph.PackageArguments["fixed"] = "changed"
	graph.Stages[4].Head.Arguments[0] = "changed"
	graph.Stages[4].Body[1].Children[0].Properties["target"] = "/changed"
	if publication.Component.PackageArguments["fixed"] != "value" || publication.Component.Definition.Instructions[4].Children[0].Arguments[0] != "base" || publication.Component.Definition.Instructions[4].Children[0].Children[1].Children[0].Properties["target"] != "/input" {
		t.Fatal("projection mutated publication metadata")
	}
}

func TestProjectRawRejectsWrongDefinitionKindsAndInvalidPlatform(t *testing.T) {
	if _, err := ProjectStandalone(parse(t, `extend as="base" {}
`), ""); err == nil || !strings.Contains(err.Error(), "cannot contain extend") {
		t.Fatalf("standalone accepted extend: %v", err)
	}
	if _, err := ProjectStandalone(parse(t, `from "scratch" as="base" {}
`), "unknown"); err == nil || !strings.Contains(err.Error(), "unknown target") {
		t.Fatalf("standalone accepted unknown target: %v", err)
	}
	publication, err := Create(parse(t, `extend as="base" {}
`), Options{Mode: Publish})
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
from "scratch" as="Build" {}
from "Build" as="Final" {}
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
from "scratch" as="Build" {}
from "scratch" as="build" {}
`), "")
	if err == nil || !strings.Contains(err.Error(), "duplicate stage name") {
		t.Fatalf("case-variant duplicate aliases accepted: %v", err)
	}
}

func TestProjectPublicationRawRetainsAuthoredStagesUntilFrontendBinding(t *testing.T) {
	def := parse(t, `
arg "base" "scratch"
from "scratch" as="unused" {}
package as="pkg" {}
extend as="caller" {}
extend as="selected" {
    from "caller" {}
}
extend as="later" {
    from "scratch" {}
}
`)
	graph, err := ProjectPublicationRaw(def, "selected")
	if err != nil {
		t.Fatal(err)
	}
	if graph.Output != "4" || len(graph.Stages) != 7 || graph.Stages[0].Name != "unused" || graph.Stages[6].Name != "later" {
		t.Fatalf("raw publication graph = %+v", graph)
	}
	if graph.Stages[4].Head.Arguments[0] != "caller" || len(graph.Stages[4].Dependencies) != 0 {
		t.Fatalf("FROM was bound before frontend probe: %+v", graph.Stages[3])
	}
	graph.Stages[4].Head.Arguments[0] = "changed"
	if def.Instructions[4].Children[0].Arguments[0] != "caller" {
		t.Fatal("publication projection mutated source definition")
	}
	if _, err := ProjectPublicationRaw(parse(t, `from "scratch" {}
`), ""); err == nil || !strings.Contains(err.Error(), "requires extend") {
		t.Fatalf("container projected as component: %v", err)
	}
}

func TestProjectionOwnsNestedInstructionTrees(t *testing.T) {
	def := parse(t, `arg "value" "original"
extend {
    from "scratch" as="nested" {
        layer {
            run "true" { mount "cache" id="original" target="/cache"; }
            copy "/inline/" { file "text" "original"; }
        }
    }
    copy "/inline/" "/selected/" from="nested"
}`)
	projected, err := ProjectPublicationRaw(def, "")
	if err != nil {
		t.Fatal(err)
	}
	before, err := ProjectPublicationRaw(def, "")
	if err != nil {
		t.Fatal(err)
	}
	def.Instructions[0].Arguments[1] = "changed"
	nested := &def.Instructions[1].Children[0]
	nested.Arguments[0] = "changed"
	nested.Properties["as"] = "changed"
	nested.Children[0].Children[0].Children[0].Properties["id"] = "changed"
	nested.Children[0].Children[1].InlineFiles[0].Data = "changed"
	if !reflect.DeepEqual(projected, before) {
		t.Fatal("source mutation reached the projection's instruction trees")
	}
	if projected.Stages[1].endPosition >= projected.Stages[0].operationPositions[0] {
		t.Fatal("nested LAYER instructions must precede the enclosing EXTEND copy")
	}
}
