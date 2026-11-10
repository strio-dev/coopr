package planner

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/definition"
)

func TestPublishedOriginalNumericSelectorsRoundTrip(t *testing.T) {
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
					wantStages := 2
					if operation == "from" {
						wantStages = 3
					}
					if len(invocation.Stages) != wantStages || invocation.Stages[0].ID != "5" || invocation.Stages[0].Operations[0].Properties["MARKER"] != "source" {
						t.Fatalf("original selected source or closure changed: %+v", invocation.Stages)
					}
					if operation == "from" {
						if invocation.Stages[1].Source != "5" || !slices.Equal(invocation.Stages[1].Dependencies, []string{"5"}) {
							t.Fatalf("wrong FROM: %+v", invocation.Stages[1])
						}
					} else {
						stage := invocation.Stages[len(invocation.Stages)-1]
						op := stage.Operations[0]
						got := op.Properties["from"]
						if operation == "bind" || operation == "cache" {
							got = op.Children[0].Properties["from"]
						}
						if got != "5" || !slices.Equal(stage.Dependencies, []string{"5"}) {
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
					changed, err := Instantiate(&component, Options{Arguments: map[string]string{"SELECTOR": "1"}})
					if err != nil {
						t.Fatalf("earlier numeric source rejected: %v", err)
					}
					if changed.Stages[0].ID != "1" || changed.Stages[0].Name != "unused" {
						t.Fatalf("changed numeric source selected wrong original stage: %+v", changed.Stages)
					}
					if _, err := Instantiate(&component, Options{Arguments: map[string]string{"SELECTOR": "0"}}); err == nil {
						t.Fatal("self/enclosing numeric reference accepted")
					}
				}
			})
		}
	}
}

func TestPublishedNumericSelectorsUseCurrentCooprCanonicalRules(t *testing.T) {
	for _, operation := range []string{
		`copy "/asset" "/asset" from="${source}"`,
		`add "/asset" "/asset" from="${source}"`,
		`run "true" { mount "bind" from="${source}" target="/asset"; }`,
		`run "true" { mount "cache" from="${source}" target="/asset"; }`,
	} {
		t.Run(operation, func(t *testing.T) {
			publication := makePlan(t, `arg "source" "0"
from "scratch" as="producer" {}
extend { `+operation+`; }
`, Options{Mode: Publish})
			for _, selector := range []string{"01", "+0", "-1", "99"} {
				_, err := Instantiate(publication.Component, Options{Arguments: map[string]string{"source": selector}})
				if err == nil || !strings.Contains(err.Error(), "unknown numeric stage") {
					t.Fatalf("noncanonical/unknown selector %q: %v", selector, err)
				}
			}
			invocation, err := Instantiate(publication.Component, Options{Arguments: map[string]string{"source": "registry.example/asset:latest"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(invocation.Stages) != 1 || len(invocation.Stages[0].Dependencies) != 0 {
				t.Fatalf("image selector kept stale stage dependency: %+v", invocation.Stages)
			}
			if _, err := Instantiate(publication.Component, Options{Arguments: map[string]string{"source": "1"}}); err == nil {
				t.Fatal("self numeric source accepted")
			}
		})
	}
}

func TestPublishedNumericSelectorsKeepInheritedAndAuthoredARGOffsets(t *testing.T) {
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
	publication, err := CreateDemandDriven(parse(t, source), Options{Mode: Publish}, unexpectedBindResolver(t))
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
		invocation, err := InstantiateWithPackageBinds(&component, Options{}, map[string]StageBind{"2": {}, "3": {Inherited: inherited}})
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
		if !slices.Equal(selected.Dependencies, []string{"3", "2"}) || selected.Operations[3].ArgumentsInScope["AUTHORED"] != "value" || selected.Operations[3].ArgumentsInScope["INHERITED"] != "2" {
			t.Fatalf("inherited/authored ARG scope or dependencies shifted: %+v", selected)
		}
		if selected.Operations[0].Arguments[1] != "/inherited" || selected.Operations[1].Arguments[1] != "/authored" || selected.Operations[2].Name != "add" || len(selected.Operations[3].Children) != 2 {
			t.Fatalf("inherited/authored lexical operation order changed: %+v", selected.Operations)
		}
		for _, op := range selected.Operations[:3] {
			if op.Properties["from"] != "2" {
				t.Fatalf("original numeric copy/add source changed: %+v", op)
			}
		}
		for _, mount := range selected.Operations[3].Children {
			if mount.Properties["from"] != "2" {
				t.Fatalf("original numeric mount source changed: %+v", mount)
			}
		}
	}
	after, _ := json.Marshal(component)
	if string(data) != string(after) {
		t.Fatal("invocation mutated published metadata")
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

	invocation, err := Instantiate(publication.Component, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if invocation.Stages[0].Source != "01" || len(invocation.Stages[0].Dependencies) != 0 {
		t.Fatalf("accepted image source changed: %+v", invocation.Stages[0])
	}
}
