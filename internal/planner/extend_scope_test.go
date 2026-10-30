package planner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/definition"
)

func TestExtendScopeIndependentOutputAndAlias(t *testing.T) {
	def := parse(t, `package as="assets" { copy "hello" "/hello"; }
extend as="runtime" {
 from "scratch" as="image" { copy "/hello" "/hello" from="assets"; }
}
`)
	raw, err := ProjectPublicationRaw(def, "")
	if err != nil {
		t.Fatal(err)
	}
	if raw.Output != "2" || raw.Aliases["runtime"] != "2" || raw.Aliases["image"] != "2" || raw.Stages[2].Owner != "1" {
		t.Fatalf("projection: %+v", raw)
	}
	publication, err := Create(def, Options{Mode: Publish})
	if err != nil {
		t.Fatal(err)
	}
	if publication.Target != "runtime" || len(publication.Stages) != 1 || publication.Stages[0].Kind != "package" {
		t.Fatalf("publication: %+v", publication)
	}
	encoded, err := json.Marshal(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	var component PublishedComponent
	if err := json.Unmarshal(encoded, &component); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidatePublished(&component); err != nil {
		t.Fatal(err)
	}
	invoked, err := Instantiate(&component, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if invoked.Target != "runtime" || len(invoked.Outputs) != 1 || len(invoked.Stages) != 2 || invoked.Stages[1].Kind != "from" {
		t.Fatalf("invocation: %+v", invoked)
	}
	if len(component.Definition.Instructions) != 2 || component.Definition.Instructions[1].Name != "extend" || component.Definition.Instructions[1].Children[0].Name != "from" {
		t.Fatalf("nested publication: %+v", component.Definition)
	}
}

func TestExtendScopeLastMeaningfulChild(t *testing.T) {
	for _, tc := range []struct{ body, output string }{
		{`from "scratch" as="helper" {}; arg "unused";`, "1"},
		{`from "scratch" as="helper" {}; env result="caller";`, "0"},
		{`env result="caller"; from "scratch" as="helper" {};`, "1"},
		{`arg "unused";`, "0"},
	} {
		raw, err := ProjectPublicationRaw(parse(t, `extend as="runtime" { `+tc.body+` }`), "")
		if err != nil {
			t.Fatal(err)
		}
		if raw.Output != tc.output {
			t.Fatalf("%s: output %s", tc.body, raw.Output)
		}
	}
}

func TestExtendScopeTrailingProducerDoesNotChangeDefault(t *testing.T) {
	def := parse(t, `from "scratch" as="producer" { copy "x" "/x"; }
package as="assets" { copy "/x" "/x" from="producer"; }
extend as="runtime" { copy "/x" "/x" from="assets"; }
from "scratch" as="unused" { copy "missing" "/missing"; }
package as="unused_assets" { copy "missing" "/missing"; }
`)
	for _, target := range []string{"", "runtime"} {
		publication, err := Create(def, Options{Mode: Publish, Target: target})
		if err != nil {
			t.Fatal(err)
		}
		if publication.Target != "runtime" || len(publication.Stages) != 2 || publication.Stages[0].Name != "producer" || publication.Stages[1].Name != "assets" {
			t.Fatalf("producer phases: %+v", publication)
		}
		if _, err := ValidatePublished(publication.Component); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExtendScopeIndependentOutputPrunesUnusedCallerInputs(t *testing.T) {
	def := parse(t, `package as="unused" { copy "private" "/private"; }
extend as="runtime" {
 copy "/private" "/private" from="unused"
 arg "required"
 copy "${required}" "/unused"
 from "scratch" { env output="independent"; }
}
`)
	publication, err := CreateDemandDriven(def, Options{Mode: Publish}, func(FromSource) (StageBind, error) { t.Fatal("unused inputs resolved"); return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(publication.Stages) != 0 || len(publication.Component.Definition.Instructions) != 1 || len(publication.Component.Definition.Instructions[0].Children) != 1 {
		t.Fatalf("unused caller input survived: %+v", publication)
	}
	if _, err := ValidatePublished(publication.Component); err != nil {
		t.Fatal(err)
	}
	invocation, err := Instantiate(publication.Component, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(invocation.Stages) != 1 || invocation.Stages[0].Name != "runtime" {
		t.Fatalf("invocation: %+v", invocation)
	}
}

func TestExtendScopeAliasesReferToSelectedResult(t *testing.T) {
	def := parse(t, `extend as="base" { from "scratch" as="image" { env base="yes"; } }
extend as="runtime" { from "base" { copy "/x" "/x" from="base"; } }
`)
	publication, err := Create(def, Options{Mode: Publish})
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := Instantiate(publication.Component, Options{})
	if err != nil {
		t.Fatal(err)
	}
	last := invocation.Stages[len(invocation.Stages)-1]
	if last.Source != "1" || last.Operations[0].Properties["from"] != "1" {
		t.Fatalf("alias did not select independent result: %+v", last)
	}
	raw, err := ProjectPublished(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Aliases["base"] != "1" || raw.Aliases["image"] != "1" {
		t.Fatalf("aliases: %+v", raw.Aliases)
	}
}

func TestExtendScopeExternalOnBuildCanConsumeTopLevelProducer(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `from "scratch" as="producer" { copy "private" "/private"; }
extend as="runtime" { from "registry.example/base:latest" {} }
`), Options{Mode: Publish}, func(FromSource) (StageBind, error) {
		t.Fatal("publication resolved invocation base")
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(source FromSource) (StageBind, error) {
		calls++
		if source.Source != "registry.example/base:latest" {
			t.Fatalf("producer treated as image: %+v", source)
		}
		return StageBind{Inherited: []definition.Instruction{{Name: "copy", Arguments: []string{"/private", "/private"}, Properties: map[string]string{"from": "producer"}}}}, nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("hidden producer edge: calls=%d err=%v", calls, err)
	}
	if invocation.Stages[0].Name != "producer" || invocation.Stages[0].Operations[0].InputContext != "caller" {
		t.Fatalf("ONBUILD helper did not execute in caller context: %+v", invocation.Stages)
	}
}

func TestExtendScopePackageOnBuildCanConsumeTopLevelProducer(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `from "scratch" as="producer" { copy "private" "/private"; }
package as="assets" { onbuild { copy "/private" "/private" from="producer"; } }
extend as="runtime" { from "assets" {} }
`), Options{Mode: Publish}, func(FromSource) (StageBind, error) { t.Fatal("unexpected resolver"); return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(publication.Stages) != 1 || publication.Stages[0].Kind != "package" {
		t.Fatalf("ONBUILD helper executed at publication: %+v", publication.Stages)
	}
	bind := StageBind{Inherited: []definition.Instruction{{Name: "copy", Arguments: []string{"/private", "/private"}, Properties: map[string]string{"from": "producer"}}}}
	if _, err := InstantiateWithPackageBinds(publication.Component, Options{}, map[string]StageBind{publication.Component.Output: bind}); err != nil {
		t.Fatal(err)
	}
}

func TestExtendScopeExternalOnBuildPreservesOriginalNumericIdentity(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `from "scratch" as="producer" {}
extend as="runtime" {
 from "scratch" as="helper" { copy "asset" "/asset"; }
 from "registry.example/base:latest" as="output" {}
 copy "/selected" "/selected" from="output"
}
`), Options{Mode: Publish}, func(FromSource) (StageBind, error) {
		t.Fatal("publication resolved invocation image")
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Dormant root helpers retain their original numeric identity.
	if publication.Component.SourceStageIDs["2"] != "2" {
		t.Fatalf("original identities: %+v", publication.Component.SourceStageIDs)
	}
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(source FromSource) (StageBind, error) {
		if source.Source != "registry.example/base:latest" {
			t.Fatalf("unexpected source %+v", source)
		}
		return StageBind{Inherited: []definition.Instruction{{Name: "copy", Arguments: []string{"/asset", "/selected"}, Properties: map[string]string{"from": "2"}}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, stage := range invocation.Stages {
		if stage.Name == "output" {
			found = true
			if stage.Operations[0].Properties["from"] != "2" {
				t.Fatalf("original numeric identity changed: %+v", stage)
			}
		}
	}
	if !found {
		t.Fatal("selected hidden dependency was lost")
	}
	for _, selector := range []string{"99"} {
		_, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(FromSource) (StageBind, error) {
			return StageBind{Inherited: []definition.Instruction{{Name: "copy", Arguments: []string{"/asset", "/selected"}, Properties: map[string]string{"from": selector}}}}, nil
		})
		if err == nil || !strings.Contains(err.Error(), "unavailable original stage") {
			t.Fatalf("unavailable original selector %s accepted: %v", selector, err)
		}
	}
}

func TestExtendScopePrunedOwnerAliasDoesNotRetargetToHelper(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `extend as="branch" {
 from "scratch" as="helper" { copy "asset" "/asset"; }
 from "scratch" as="original_result" {}
}
extend as="runtime" { arg "source" "helper"; copy "/asset" "/asset" from="${source}"; }
`), Options{Mode: Publish}, func(FromSource) (StageBind, error) { t.Fatal("unexpected resolver"); return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	raw, err := ProjectPublished(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := raw.Aliases["branch"]; ok {
		t.Fatalf("pruned output alias retargeted: %+v", raw.Aliases)
	}
	if _, err := Instantiate(publication.Component, Options{Arguments: map[string]string{"source": "branch"}}); err == nil {
		t.Fatal("pruned output alias changed dependency")
	}
}

func TestExtendScopeExternalOnBuildCanSelectEarlierCallerBody(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `extend as="caller" { copy "payload" "/payload"; }
extend as="runtime" {
 from "registry.example/base:latest" {}
}
`), Options{Mode: Publish}, func(FromSource) (StageBind, error) {
		t.Fatal("publication resolved invocation base")
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(FromSource) (StageBind, error) {
		return StageBind{Inherited: []definition.Instruction{{Name: "copy", Arguments: []string{"/payload", "/selected"}, Properties: map[string]string{"from": "0"}}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(invocation.Stages) != 2 || invocation.Stages[0].Kind != "extend" || len(invocation.Stages[0].Operations) != 1 || invocation.Stages[0].Operations[0].Arguments[0] != "payload" {
		t.Fatalf("original caller body lost: %+v", invocation.Stages)
	}
}

func TestExtendScopePackageOnBuildCanUseSelectedExtendAlias(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `package as="assets" { onbuild { copy "/x" "/x" from="base"; } }
extend as="base" { from "scratch" { copy "asset" "/x"; } }
extend as="runtime" { from "assets" {} }
`), Options{Mode: Publish}, func(FromSource) (StageBind, error) { t.Fatal("unexpected resolver"); return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidatePublished(publication.Component); err != nil {
		t.Fatal(err)
	}
	raw, err := ProjectPublished(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	output := raw.Output
	bind := StageBind{Inherited: []definition.Instruction{{Name: "copy", Arguments: []string{"/x", "/x"}, Properties: map[string]string{"from": "base"}}}}
	invocation, err := InstantiateWithPackageBinds(publication.Component, Options{}, map[string]StageBind{output: bind})
	if err != nil {
		t.Fatal(err)
	}
	last := invocation.Stages[len(invocation.Stages)-1]
	if last.Operations[0].Properties["from"] != raw.Aliases["base"] {
		t.Fatalf("package alias reference: %+v", last)
	}
}
