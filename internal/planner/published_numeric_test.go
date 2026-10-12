package planner

import (
	"coopr/internal/definition"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

func TestPublishedPrunedNumericSelectorsRoundTrip(t *testing.T) {
	for _, operation := range []string{"from", "copy", "add", "bind", "cache"} {
		for _, parameterized := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/parameterized=%v", operation, parameterized), func(t *testing.T) {
				selector := "5"
				globals := ""
				if parameterized {
					selector = "$SELECTOR"
					globals = "arg \"SELECTOR\" \"5\"\n"
				}
				source := globals + `extend {
from "scratch" as="unused" {}
from "scratch" as="unused2" {}
from "scratch" as="unused3" {}
from "scratch" as="unused4" {}
from "scratch" {
    env MARKER="source"
}
`
				switch operation {
				case "from":
					source += "from \"" + selector + `" as="selected" {}
    copy "/src" "/dst" from="selected"
}
`
				case "copy", "add":
					source += operation + " \"/src\" \"/dst\" from=\"" + selector + `"
}
`
				default:
					source += `run "true" { mount "` + operation + "\" from=\"" + selector + `" source="/src" target="/dst"; }
}
`
				}
				authored := parse(t, source)
				authoredBefore, _ := json.Marshal(authored)
				publication, err := Create(authored, Options{Mode: Publish})
				if err != nil {
					t.Fatal(err)
				}
				original, _ := json.Marshal(publication.Component)
				var component PublishedComponent
				if err := json.Unmarshal(original, &component); err != nil {
					t.Fatal(err)
				}
				if _, err := ValidatePublished(&component); err != nil {
					t.Fatal(err)
				}
				for count := 0; count < 2; count++ {
					invocation, err := Instantiate(&component, Options{})
					if err != nil {
						t.Fatal(err)
					}
					if len(invocation.Stages) != 2 && operation != "from" {
						t.Fatalf("unpruned graph: %+v", invocation.Stages)
					}
					if operation == "from" {
						if invocation.Stages[1].Source != "1" || !slices.Equal(invocation.Stages[1].Dependencies, []string{"1"}) {
							t.Fatalf("wrong FROM: %+v", invocation.Stages[1])
						}
					} else {
						stage := invocation.Stages[len(invocation.Stages)-1]
						op := stage.Operations[0]
						got := op.Properties["from"]
						if operation == "bind" || operation == "cache" {
							got = op.Children[0].Properties["from"]
						}
						if got != "1" || !slices.Equal(stage.Dependencies, []string{"1"}) {
							t.Fatalf("wrong reference: %+v", stage)
						}
					}
				}
				after, _ := json.Marshal(component)
				if string(after) != string(original) {
					t.Fatal("publication mutated")
				}
				authoredAfter, _ := json.Marshal(authored)
				if string(authoredBefore) != string(authoredAfter) {
					t.Fatal("authored definition mutated")
				}
				if parameterized {
					for _, changed := range []string{"0", "1"} {
						if _, err := Instantiate(&component, Options{Arguments: map[string]string{"SELECTOR": changed}}); err == nil {
							t.Fatal("changed numeric selector accepted")
						}
					}
				}
			})
		}
	}
}

func TestPublishedNumericProvenanceRejectsMalformedAndMissingBindings(t *testing.T) {
	for _, operation := range []string{"from", "copy"} {
		source := `extend {
from "scratch" as="unused" {}
from "scratch" as="unused2" {}
from "scratch" {}
`
		if operation == "from" {
			source += `from "3" as="selected" {}
    copy "/src" "/dst" from="selected"
}
`
		} else {
			source += `copy "/src" "/dst" from="3"
}
`
		}
		publication, err := Create(parse(t, source), Options{Mode: Publish})
		if err != nil {
			t.Fatal(err)
		}
		for _, invalid := range []string{"-1", "+2", "02", "x", ""} {
			data, _ := json.Marshal(publication.Component)
			var component PublishedComponent
			if err := json.Unmarshal(data, &component); err != nil {
				t.Fatal(err)
			}
			if operation == "from" {
				binding := component.FromBindings["2"]
				binding.SourceIndex = invalid
				component.FromBindings["2"] = binding
			} else {
				component.StageReferences[0].SourceIndex = invalid
			}
			if _, err := ValidatePublished(&component); err == nil {
				t.Fatalf("%s provenance %q accepted", operation, invalid)
			}
		}
		data, _ := json.Marshal(publication.Component)
		var component PublishedComponent
		_ = json.Unmarshal(data, &component)
		if operation == "from" {
			delete(component.FromBindings, "2")
		} else {
			component.StageReferences = nil
		}
		if _, err := Instantiate(&component, Options{}); err == nil {
			t.Fatalf("missing %s binding accepted", operation)
		}
	}
	for _, source := range []string{"scratch", "example.invalid/base:latest"} {
		publication, err := Create(parse(t, "extend {\nfrom \""+source+`" as="base" {}
    copy "/src" "/dst" from="base"
}
`), Options{Mode: Publish})
		if err != nil {
			t.Fatal(err)
		}
		binding := publication.Component.FromBindings["1"]
		binding.SourceIndex = "0"
		publication.Component.FromBindings["1"] = binding
		if _, err := ValidatePublished(publication.Component); err == nil {
			t.Fatalf("%s numeric provenance accepted", source)
		}
	}

}

func TestPublishedNumericBindingsKeepInheritedAndAuthoredARGOffsets(t *testing.T) {
	source := `from "scratch" as="unused" {}
from "scratch" as="unused2" {}
package as="assets" {}
package as="payload" {
    onbuild { arg "INHERITED" "2" }
    onbuild { copy "/asset" "/inherited" from="$INHERITED" }
}
extend {
from "payload" as="selected" {
    arg "AUTHORED" "value"
    copy "/asset" "/authored" from="2"
    add "/asset" "/added" from="2"
    run "true" {
     mount "bind" from="2" source="/asset" target="/bind"
     mount "cache" from="2" source="/asset" target="/cache"
    }
}
    copy "/authored" "/result" from="selected"
}
`
	publication, err := CreateDemandDriven(parse(t, source), Options{Mode: Publish}, func(FromSource) (StageBind, error) {
		t.Fatal("unexpected external resolution")
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(publication.Component)
	var component PublishedComponent
	if err := json.Unmarshal(data, &component); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidatePublished(&component); err != nil {
		t.Fatal(err)
	}
	inherited := []definition.Instruction{
		{Name: "arg", Arguments: []string{"INHERITED", "2"}},
		{Name: "copy", Arguments: []string{"/asset", "/inherited"}, Properties: map[string]string{"from": "$INHERITED"}},
	}
	for i := 0; i < 2; i++ {
		invocation, err := InstantiateWithPackageBinds(&component, Options{}, map[string]StageBind{"3": {Inherited: inherited}})
		if err != nil {
			t.Fatal(err)
		}
		var selected Stage
		for _, stage := range invocation.Stages {
			if stage.Name == "selected" {
				selected = stage
			}
		}
		if len(selected.Operations) != 4 {
			t.Fatalf("ARG counted as operation: %+v", selected)
		}
		for _, op := range selected.Operations[:3] {
			if op.Properties["from"] != "0" {
				t.Fatalf("numeric copy/add rebind failed: %+v", op)
			}
		}
		for _, mount := range selected.Operations[3].Children {
			if mount.Properties["from"] != "0" {
				t.Fatalf("numeric mount rebind failed: %+v", mount)
			}
		}
	}
	after, _ := json.Marshal(component)
	if string(data) != string(after) {
		t.Fatal("invocation mutated published metadata")
	}
	for index, ref := range component.StageReferences {
		if ref.Origin == "onbuild" {
			component.StageReferences = append(component.StageReferences[:index], component.StageReferences[index+1:]...)
			break
		}
	}
	if _, err := InstantiateWithPackageBinds(&component, Options{}, map[string]StageBind{"3": {Inherited: inherited}}); err == nil {
		t.Fatal("missing inherited frozen binding accepted")
	}
}

func TestPublishedNoncanonicalNumericFROMRetainsImageRole(t *testing.T) {
	publication, err := Create(parse(t, `extend {
from "01" as="external" {}
    copy "/src" "/dst" from="external"
}
`), Options{Mode: Publish})
	if err != nil {
		t.Fatal(err)
	}
	if publication.Component.FromBindings["1"].Kind != "image" {
		t.Fatal("noncanonical numeric image source became a local stage")
	}
	invocation, err := Instantiate(publication.Component, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if invocation.Stages[0].Source != "01" || len(invocation.Stages[0].Dependencies) != 0 {
		t.Fatalf("accepted image source changed: %+v", invocation.Stages[0])
	}
}
