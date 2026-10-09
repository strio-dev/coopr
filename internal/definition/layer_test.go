package definition

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestLayerOrderedNestedDefinitionRoundTrip(t *testing.T) {
	def, err := Parse(strings.NewReader(`from "scratch"
layer {
 arg "value" "first"
 run { exec "echo" "$value"; mount "cache" target="/cache" }
 layer { env "value" "second"; copy "input" "/input" }
 component "./shared.coopr" value="${value}"
}
layer
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Instructions) != 3 || len(def.Instructions[1].Children) != 4 || def.Instructions[1].Children[2].Name != "layer" {
		t.Fatalf("ordered layer body lost: %#v", def)
	}
	encoded, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Definition
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := Validate(&decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(def, &decoded) {
		t.Fatal("JSON roundtrip changed layer instructions")
	}
}

func TestLayerRejectsStageHeadsAndInvalidChildren(t *testing.T) {
	for _, source := range []string{`layer { from "scratch" }`, `layer { extend }`, `layer { package as="payload" }`, `layer { layer { extend } }`, `layer "name"`, `layer compact=true`, `layer { mount "cache" target="/cache" }`, `layer { run "true" { env "x" "y" } }`} {
		t.Run(source, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(source)); err == nil {
				t.Fatalf("invalid layer accepted: %s", source)
			}
		})
	}
	for _, name := range []string{"from", "extend", "package"} {
		child := Instruction{Name: name}
		if name == "from" {
			child.Arguments = []string{"scratch"}
		}
		if name == "package" {
			child.Properties = map[string]string{"as": "payload"}
		}
		if err := Validate(&Definition{Instructions: []Instruction{{Name: "layer", Children: []Instruction{child}}}}); err == nil {
			t.Fatalf("decoded stage head %s accepted in layer", name)
		}
	}
}

func TestLayerBoundariesAreInternalAndValidated(t *testing.T) {
	for _, inst := range []Instruction{{Name: "layer", LayerBoundary: "begin"}, {Name: "layer", Children: []Instruction{{Name: "layer", LayerBoundary: "end"}}}, {Name: "run", Form: "shell", Arguments: []string{"true"}, LayerBoundary: "begin"}} {
		if err := Validate(&Definition{Instructions: []Instruction{inst}}); err == nil {
			t.Fatalf("internal marker accepted as public definition: %+v", inst)
		}
	}
	for _, inst := range []Instruction{{Name: "layer", LayerBoundary: "wrong"}, {Name: "layer", LayerBoundary: "begin", Children: []Instruction{{Name: "layer"}}}, {Name: "run", Form: "shell", Arguments: []string{"true"}, LayerBoundary: "begin"}} {
		if err := ValidateResolved(inst); err == nil {
			t.Fatalf("invalid resolved marker accepted: %+v", inst)
		}
	}
	for _, boundary := range []string{"begin", "end"} {
		if err := ValidateResolved(Instruction{Name: "layer", LayerBoundary: boundary}); err != nil {
			t.Fatal(err)
		}
	}
}
