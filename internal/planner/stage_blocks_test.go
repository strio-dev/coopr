package planner

import (
	"encoding/json"
	"testing"
)

func TestStageBlocksKeepGlobalsOutsideStageScope(t *testing.T) {
	def := parse(t, `from "scratch" as="first" { arg "shared"; copy "${shared}" "/first"; }
arg "shared" "global"
from "scratch" as="last" { arg "shared" "stage"; copy "${shared}" "/last"; }
`)
	plan, err := Create(def, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Target != "last" || len(plan.Stages) != 1 || plan.Stages[0].Operations[0].Arguments[0] != "stage" {
		t.Fatalf("global/stage scope: %+v", plan)
	}
	first, err := Create(def, Options{Target: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Stages[0].Operations[0].Arguments[0] != "global" {
		t.Fatalf("stage did not consume global: %+v", first)
	}
	raw, err := ProjectStandalone(def, "first")
	if err != nil {
		t.Fatal(err)
	}
	if raw.Output != "0" || len(raw.Globals) != 1 || len(raw.Stages[0].Head.Children) != 0 || len(raw.Stages[0].Body) != 2 {
		t.Fatalf("nested projection: %+v", raw)
	}
}

func TestStageBlocksPublicationPreservesNestedBodiesAndPackageScope(t *testing.T) {
	publication := makePlan(t, `arg "message" "hello"
package as="assets" { arg "message"; run "echo ${message} > /hello"; }
extend as="output" { layer { copy "/hello" "/hello" from="assets"; }; }
`, Options{Mode: Publish})
	component := publication.Component
	if len(component.Definition.Instructions) != 3 || len(component.Definition.Instructions[1].Children) != 1 || component.Definition.Instructions[1].Children[0].Name != "arg" || component.Definition.Instructions[2].Children[0].Name != "layer" {
		t.Fatalf("published nested definition: %+v", component.Definition)
	}
	encoded, err := json.Marshal(component)
	if err != nil {
		t.Fatal(err)
	}
	var restored PublishedComponent
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if _, err := Instantiate(&restored, Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestStageBlocksComponentDefaultsToLastExtend(t *testing.T) {
	for _, trailing := range []string{
		`package as="assets" { copy "missing" "/missing"; }`,
		`from "registry.example/unused:latest" as="unused" { copy "missing" "/missing"; }`,
		`from "scratch" as="unused" {}; package as="assets" { copy "missing" "/missing"; }`,
	} {
		t.Run(trailing, func(t *testing.T) {
			def := parse(t, `extend as="first" { env selected="first"; }
extend as="output" { env selected="last"; }
`+trailing+"\n")
			for _, target := range []string{"", "output", "first", "assets", "unused", "missing"} {
				projected, err := ProjectPublicationRaw(def, target)
				want := "1"
				if target == "first" {
					want = "0"
				}
				if target == "" || target == "output" || target == "first" {
					if err != nil || projected.Output != want {
						t.Fatalf("target %q: projection=%+v err=%v", target, projected, err)
					}
				} else if err == nil {
					t.Fatalf("invalid projected target %q accepted", target)
				}
			}
			for _, demand := range []bool{false, true} {
				opts := Options{Mode: Publish}
				create := func() (*Plan, error) {
					if demand {
						return CreateDemandDriven(def, opts, func(FromSource) (StageBind, error) { t.Fatal("unused base resolved"); return StageBind{}, nil })
					}
					return Create(def, opts)
				}
				publication, err := create()
				if err != nil || publication.Target != "output" || len(publication.Stages) != 0 {
					t.Fatalf("default target: publication=%+v err=%v", publication, err)
				}
				if _, err := ValidatePublished(publication.Component); err != nil {
					t.Fatal(err)
				}
				invoked, err := Instantiate(publication.Component, Options{})
				if err != nil || invoked.Target != "output" || len(invoked.Stages) != 1 || invoked.Stages[0].Operations[0].Properties["selected"] != "last" {
					t.Fatalf("default invocation: %+v err=%v", invoked, err)
				}
				opts.Target = "first"
				if explicit, err := create(); err != nil || explicit.Target != "first" {
					t.Fatalf("explicit target: %+v err=%v", explicit, err)
				}
				opts.Target = "assets"
				if _, err := create(); err == nil {
					t.Fatal("explicit package accepted")
				}
			}
		})
	}
}

func TestStageBlocksDefaultExtendKeepsSelectedNestedResult(t *testing.T) {
	def := parse(t, `extend as="first" {}
extend {
    from "scratch" { env selected="independent"; }
}
package as="unused" { copy "missing" "/missing"; }
`)
	publication, err := Create(def, Options{Mode: Publish})
	if err != nil {
		t.Fatal(err)
	}
	invoked, err := Instantiate(publication.Component, Options{})
	if err != nil || len(invoked.Stages) != 1 || invoked.Stages[0].Kind != "from" || invoked.Stages[0].Operations[0].Properties["selected"] != "independent" {
		t.Fatalf("anonymous extend output: %+v err=%v", invoked, err)
	}
}
