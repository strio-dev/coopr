package planner

import (
	"strconv"
	"strings"
	"testing"

	"coopr/internal/definition"
)

func TestStageOrderRejectsUnavailableReferences(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		mode         Mode
	}{
		{"from", `from "later" as="output" {}; from "scratch" as="later" {}`, Build},
		{"numeric from", `from "1" as="output" {}; from "scratch" as="later" {}`, Build},
		{"self from", `from "output" as="output" {}`, Build},
		{"copy", `from "scratch" as="output" { copy "/x" "/x" from="later"; }; from "scratch" as="later" {}`, Build},
		{"numeric copy", `from "scratch" as="output" { copy "/x" "/x" from="1"; }; from "scratch" as="later" {}`, Build},
		{"self copy", `from "scratch" as="output" { copy "/x" "/x" from="output"; }`, Build},
		{"add", `from "scratch" as="output" { add "/x" "/x" from="later"; }; from "scratch" as="later" {}`, Build},
		{"mount", `from "scratch" as="output" { run "true" { mount "bind" from="later" target="/x"; }; }; from "scratch" as="later" {}`, Build},
		{"parameter", `arg "input" "later"; from "scratch" as="output" { arg "input"; copy "/x" "/x" from="${input}"; }; from "scratch" as="later" {}`, Build},
		{"nested helper", `extend as="output" { copy "/x" "/x" from="helper"; from "scratch" as="helper" {}; env selected="caller"; }`, Publish},
		{"nested sibling", `extend as="output" { from "later" as="first" {}; from "scratch" as="later" {}; copy "/x" "/x" from="first"; }`, Publish},
		{"enclosing alias", `extend as="output" { from "scratch" as="helper" {}; copy "/x" "/x" from="output"; }`, Publish},
		{"incomplete caller", `extend as="output" { from "0" {}; }`, Publish},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateDemandDriven(parse(t, tc.source), Options{Mode: tc.mode, Target: "output"}, unexpectedBindResolver(t))
			if err == nil || !strings.Contains(err.Error(), "earlier") {
				t.Fatalf("expected earlier-stage diagnostic, got %v", err)
			}
		})
	}
}

func TestStageOrderNestedHelpersRemainVisibleAfterDeclaration(t *testing.T) {
	for _, source := range []string{"helper", "1"} {
		def := parse(t, `extend as="output" {
            from "scratch" as="helper" { env helper="yes"; }
            layer { arg "unused"; copy "/x" "/x" from="`+source+`"; }
        }`)
		publication, err := CreateDemandDriven(def, Options{Mode: Publish}, unexpectedBindResolver(t))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Instantiate(publication.Component, Options{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStageOrderOnBuildUsesConsumingHead(t *testing.T) {
	for _, earlier := range []bool{false, true} {
		asset := `from "scratch" as="asset" {}`
		consumer := `from "registry.example/base:latest" as="output" {}`
		source := consumer + "\n" + asset
		if earlier {
			source = asset + "\n" + consumer
		}
		_, err := CreateDemandDriven(parse(t, source), Options{Target: "output"}, func(input FromSource) (StageBind, error) {
			return StageBind{Inherited: []definition.Instruction{{Name: "copy", Arguments: []string{"/x", "/x"}, Properties: map[string]string{"from": "asset"}}}}, nil
		})
		if earlier && err != nil {
			t.Fatal(err)
		}
		if !earlier && (err == nil || !strings.Contains(err.Error(), "earlier")) {
			t.Fatalf("forward ONBUILD accepted: %v", err)
		}
	}
	def := parse(t, `from "scratch" as="base" { onbuild { copy "/x" "/x" from="asset"; }; }
        from "scratch" as="asset" {}
        from "base" as="output" {}`)
	if _, err := CreateDemandDriven(def, Options{}, unexpectedBindResolver(t)); err != nil {
		t.Fatal(err)
	}
}

func TestStageOrderSourceDiscoveryRejectsForwardFrom(t *testing.T) {
	_, err := ResolveFromSources(parse(t, `from "later" {}; from "scratch" as="later" {}`), Options{})
	if err == nil || !strings.Contains(err.Error(), "earlier") {
		t.Fatalf("forward FROM discovery: %v", err)
	}
}

func TestStageOrderPublishedMetadataRejectsMovedForwardReferences(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `extend as="output" {
        from "scratch" as="helper" {}
        copy "/x" "/x" from="helper"
        env selected="caller"
    }`), Options{Mode: Publish}, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	children := publication.Component.Definition.Instructions[0].Children
	children[0], children[1] = children[1], children[0]
	if _, err := ValidatePublished(publication.Component); err == nil || !strings.Contains(err.Error(), "earlier") {
		t.Fatalf("forged forward binding: %v", err)
	}
}

func TestStageOrderPublicationIgnoresTrailingOnBuildCandidates(t *testing.T) {
	def := parse(t, `from "scratch" as="earlier" {}
        extend as="output" { from "registry.example/base:latest" {}; }
        from "registry.example/unused:latest" as="unused" { arg "required"; copy "${required:?missing}" "/x"; }
        package as="unused_assets" { arg "required"; copy "${required:?missing}" "/x"; }`)
	publication, err := CreateDemandDriven(def, Options{Mode: Publish}, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(publication.Stages) != 0 {
		t.Fatalf("trailing packages executed: %+v", publication.Stages)
	}
	raw, err := ProjectPublished(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := raw.Aliases["unused"]; ok {
		t.Fatal("trailing FROM retained")
	}
	if _, ok := raw.Aliases["unused_assets"]; ok {
		t.Fatal("trailing package retained")
	}
	_, err = InstantiateDemandDriven(publication.Component, Options{}, nil, func(input FromSource) (StageBind, error) {
		if input.Source != "registry.example/base:latest" {
			t.Fatalf("unused image resolved: %+v", input)
		}
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStageOrderPublishedMetadataRejectsTrailingDormantStages(t *testing.T) {
	for _, kind := range []string{"package", "extend"} {
		t.Run(kind, func(t *testing.T) {
			publication, err := CreateDemandDriven(parse(t, `extend as="output" { from "registry.example/base:latest" {}; }`), Options{Mode: Publish}, unexpectedBindResolver(t))
			if err != nil {
				t.Fatal(err)
			}
			component := publication.Component
			component.Definition.Instructions = append(component.Definition.Instructions, definition.Instruction{
				Name: kind, Properties: map[string]string{"as": "trailing"}, Children: []definition.Instruction{},
			})
			id := component.SourceStageCount
			component.SourceStageCount++
			component.SourceStageIDs[strconv.Itoa(id)] = strconv.Itoa(id)
			component.ReservedStageNames = append(component.ReservedStageNames, "trailing")
			component.DormantRoots = append(component.DormantRoots, strconv.Itoa(id))
			if _, err := ValidatePublished(component); err == nil || !strings.Contains(err.Error(), "no selected external FROM") {
				t.Fatalf("forged trailing dormant %s: %v", kind, err)
			}
		})
	}
}

func TestStageOrderInheritedInstructionsCannotSeeLaterNestedHelpers(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `extend as="output" {
        from "registry.example/base:latest" as="consumer" {}
        from "scratch" as="later" {}
        copy "/x" "/x" from="consumer"
    }`), Options{Mode: Publish}, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = InstantiateDemandDriven(publication.Component, Options{}, nil, func(input FromSource) (StageBind, error) {
		return StageBind{Inherited: []definition.Instruction{{Name: "copy", Arguments: []string{"/x", "/x"}, Properties: map[string]string{"from": "later"}}}}, nil
	})
	if err == nil {
		t.Fatal("inherited trigger consumed later nested helper")
	}
}

func TestStageOrderInheritedArgsDoNotShiftAuthoredReferences(t *testing.T) {
	_, err := CreateDemandDriven(parse(t, `from "scratch" as="asset" {}
        from "registry.example/base:latest" as="output" {
            arg "unused"
            layer { copy "/x" "/x" from="asset"; }
        }`), Options{}, func(input FromSource) (StageBind, error) {
		return StageBind{Inherited: []definition.Instruction{
			{Name: "arg", Arguments: []string{"inherited", "yes"}},
			{Name: "env", Properties: map[string]string{"inherited": "${inherited}"}},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
