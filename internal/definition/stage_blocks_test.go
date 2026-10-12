package definition

import (
	"strings"
	"testing"
)

func TestStageBlocksRetainOwnedInstructions(t *testing.T) {
	def, err := Parse(strings.NewReader(`
arg "base" "scratch"
from "${base}" as="producer" {
    arg "base"
    copy "input" "/payload"
}
package as="assets" {
    copy "/payload" "/payload" from="producer"
}
extend as="configured" {
    layer {
        component "./shared.coopr#selected" { arg "channel" "stable" }
        copy "/payload" "/payload" from="assets"
    }
}
from "configured" as="final" {}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Instructions) != 5 {
		t.Fatalf("top-level instructions = %d, want global plus four stages", len(def.Instructions))
	}
	if got := def.Instructions[1].Children; len(got) != 2 || got[0].Name != "arg" || got[1].Name != "copy" {
		t.Fatalf("producer body = %#v", got)
	}
	if got := def.Instructions[3].Children[0]; got.Name != "layer" || len(got.Children) != 2 || got.Children[0].Children[0].Name != "arg" {
		t.Fatalf("component/layer body = %#v", got)
	}
	if err := Validate(def); err != nil {
		t.Fatal(err)
	}
}

func TestStageBlocksRejectFlatAndNestedStages(t *testing.T) {
	for _, source := range []string{
		`from "scratch"`,
		`extend`,
		`package as="assets"`,
		"from \"scratch\" {}\nrun \"true\"",
		"extend {}\ncopy \"input\" \"/payload\"",
		`from "scratch" { from "scratch" {} }`,
		`extend { package as="assets" {} }`,
		`extend { extend {} }`,
		`extend { from "scratch" { from "scratch" {} } }`,
		`extend { from "scratch" { package as="assets" {} } }`,
		`extend { layer { from "scratch" {} } }`,
		`package as="assets" { extend {} }`,
		`from "scratch" { layer { from "scratch" {} } }`,
		`from "scratch" { arg "value" { run "true" } }`,
	} {
		t.Run(source, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(source)); err == nil {
				t.Fatalf("accepted noncanonical definition %s", source)
			}
		})
	}
}

func TestStageBlocksValidateDecodedStructure(t *testing.T) {
	for _, def := range []*Definition{
		{Instructions: []Instruction{{Name: "from", Arguments: []string{"scratch"}}, {Name: "run", Form: "shell", Arguments: []string{"true"}}}},
		{Instructions: []Instruction{{Name: "from", Arguments: []string{"scratch"}, Children: []Instruction{{Name: "from", Arguments: []string{"scratch"}}}}}},
		{Instructions: []Instruction{{Name: "package", Properties: map[string]string{"as": "assets"}, Children: []Instruction{{Name: "extend"}}}}},
	} {
		if err := Validate(def); err == nil {
			t.Fatalf("accepted noncanonical decoded definition %#v", def)
		}
	}
}

func TestExtendOwnsNestedFromStages(t *testing.T) {
	def, err := Parse(strings.NewReader(`
extend as="runtime" {
    arg "message" "hello"
    from "scratch" as="helper" {
        copy "input" "/hello"
    }
    layer {
        copy "/hello" "/hello" from="helper"
    }
}
`))
	if err != nil {
		t.Fatal(err)
	}
	body := def.Instructions[0].Children
	if len(body) != 3 || body[1].Name != "from" || body[1].Children[0].Name != "copy" || body[2].Name != "layer" {
		t.Fatalf("extend ownership: %#v", body)
	}
	if err := Validate(def); err != nil {
		t.Fatal(err)
	}
}
