package planner

import (
	"github.com/strio-dev/coopr/internal/buildcontext"
	"github.com/strio-dev/coopr/internal/definition"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestPublishedPrunesStaticBodiesWithoutChangingNamespace(t *testing.T) {
	p := residualPublish(t, `from "scratch" as="selected" { env "VALUE" "selected"; }
from "scratch" as="unused" { env "PRIVATE_DEFAULT" "not published"; }
extend { copy "/x" "/x" from="0"; }
from "scratch" as="trailing" { run "not published either"; }`)
	g, err := ProjectPublished(p.Component)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Stages) != 4 || g.Output != "2" || g.Aliases["unused"] != "1" || g.Aliases["trailing"] != "3" {
		t.Fatal(g)
	}
	for _, id := range []int{1, 3} {
		if !g.Stages[id].Head.PrunedFrom || len(g.Stages[id].Body) != 0 {
			t.Fatalf("unselected body retained: %+v", g.Stages[id])
		}
	}
	plan := residualInvoke(t, p.Component, Options{}, nil, nil)
	if !reflect.DeepEqual(stageIDs(plan), []string{"0", "2"}) {
		t.Fatal(stageIDs(plan))
	}
	p.Component.Definition.Instructions[1].Children = []definition.Instruction{{Name: "run", Arguments: []string{"silently rebuild"}}}
	if _, err := ValidatePublished(p.Component); err == nil {
		t.Fatal("pruned executable body admitted")
	}
	p = residualPublish(t, `extend { from "scratch" {}; }`)
	p.Component.Definition.Instructions[0].Children[0].PrunedFrom = true
	if _, err := ValidatePublished(p.Component); err == nil {
		t.Fatal("pruned selected output admitted")
	}
}

func TestPublishedPruningRetainsDynamicChoices(t *testing.T) {
	for name, body := range map[string]string{
		"from":         `from "${choice}" {}`,
		"after":        `from "scratch" after="${choice}" {}`,
		"copy":         `copy "/x" "/x" from="${choice}"`,
		"mount-source": `run "true" { mount "bind" from="${choice}" target="/input"; }`,
		"mount-type":   `run "true" { mount "${kind}" from="first" target="/input"; }`,
		"onbuild":      `onbuild { copy "/x" "/x" from="${choice}"; }`,
		"component":    `component "registry.example/returns-config:stable"`,
	} {
		t.Run(name, func(t *testing.T) {
			p := residualPublish(t, `arg "choice" "first"; arg "kind" "bind"; from "scratch" as="first" { env "VALUE" "first"; }; from "scratch" as="alternate" { env "VALUE" "alternate"; }; extend { `+body+`; }`)
			if p.Component.Definition.Instructions[3].PrunedFrom || len(p.Component.Definition.Instructions[3].Children) == 0 {
				t.Fatal("dynamic alternate was pruned")
			}
			if name == "from" || name == "copy" || name == "mount-source" || name == "after" {
				plan := residualInvoke(t, p.Component, Options{Arguments: map[string]string{"choice": "alternate"}}, nil, nil)
				if !slices.Contains(stageIDs(plan), "1") {
					t.Fatal("alternate not selected", stageIDs(plan))
				}
			}
		})
	}
}

func TestPublishedPruningPreservesContextOnBuildPrefix(t *testing.T) {
	p := residualPublish(t, `from "scratch" as="hidden" { env "VALUE" "needed by context"; }
extend {
 from "scratch" as="replaceable" {}
 from "scratch" as="later" { env "VALUE" "not needed"; }
 copy "/x" "/x" from="2"
}`)
	if p.Component.Definition.Instructions[0].PrunedFrom || !p.Component.Definition.Instructions[1].Children[1].PrunedFrom {
		t.Fatal("wrong lexical prefix")
	}
	spec := buildcontext.Spec{Name: "replaceable", Kind: buildcontext.DockerImage, Reference: "sha256:" + strings.Repeat("a", 64)}
	for _, source := range []string{"hidden", "0"} {
		plan := residualInvoke(t, p.Component, Options{BuildContexts: []buildcontext.Spec{spec}}, nil, func(FromSource) (StageBind, error) {
			return residualBind(t, "COPY --from="+source+" /x /inherited"), nil
		})
		if !slices.Contains(stageIDs(plan), "0") {
			t.Fatal("context trigger lost earlier hidden source", stageIDs(plan))
		}
	}
	_, err := InstantiateDemandDriven(p.Component, Options{BuildContexts: []buildcontext.Spec{spec}}, nil, func(FromSource) (StageBind, error) { return residualBind(t, "COPY --from=3 /x /invalid"), nil })
	if err == nil || !strings.Contains(err.Error(), "earlier stage") {
		t.Fatal("late trigger gained forward access", err)
	}
}

func TestPublishedPruningPreservesSourceDateEpochRecipe(t *testing.T) {
	p := residualPublish(t, `arg "url" "https://example.invalid/source.tar.gz"; arg "SOURCE_DATE_EPOCH" "";
from "scratch" as="epoch" { add "${url}" "/source"; }
extend { env "VALUE" "static"; }`)
	if p.Component.Definition.Instructions[2].PrunedFrom {
		t.Fatal("source-only timestamp recipe pruned")
	}
	called := false
	epoch := int64(1700000000)
	plan := residualInvoke(t, p.Component, Options{Arguments: map[string]string{"SOURCE_DATE_EPOCH": "epoch"}, SourceDateEpochResolver: func(source SourceDateEpochSource) (*int64, error) {
		called = true
		if source.Kind != SourceDateEpochHTTP || source.Reference != "https://example.invalid/source.tar.gz" {
			t.Fatal(source)
		}
		return &epoch, nil
	}}, nil, nil)
	if !called || plan.SourceDateEpoch == nil || *plan.SourceDateEpoch != epoch || !reflect.DeepEqual(stageIDs(plan), []string{"1"}) {
		t.Fatal(called, plan.SourceDateEpoch, stageIDs(plan))
	}
}
