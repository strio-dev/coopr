package planner

import (
	"encoding/json"
	"testing"

	"github.com/strio-dev/coopr/internal/buildcontext"
	"github.com/strio-dev/coopr/internal/definition"
)

func TestSharedPhaseStageDoesNotPublishContextOverride(t *testing.T) {
	def := parse(t, `from "scratch" as="helper" { copy "payload" "/payload"; }
package as="assets" { copy "/payload" "/payload" from="0"; }
extend {
    copy "/payload" "/packaged" from="assets"
    copy "/payload" "/current" from="helper"
}
`)
	publication, err := CreateDemandDriven(def, Options{Mode: Publish, BuildContexts: []buildcontext.Spec{
		{Name: "helper", Kind: buildcontext.DockerImage, Reference: "registry.example/package-helper:latest"},
	}}, func(FromSource) (StageBind, error) { return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidatePublished(publication.Component); err != nil {
		t.Fatal(err)
	}
	invocation, err := Instantiate(publication.Component, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range invocation.Stages {
		if stage.Name == "helper" {
			if stage.Source != "scratch" || stage.SourceContext != "" || len(stage.Operations) != 1 || stage.Operations[0].InputContext != "caller" {
				t.Fatalf("publication context override leaked into invocation: %+v", stage)
			}
			return
		}
	}
	t.Fatal("shared helper missing at invocation")
}

func TestSharedPhaseStageDoesNotPublishBaseBindingsOrTransientMounts(t *testing.T) {
	def := parse(t, `from "registry.example/helper:latest" as="helper" {
    env selected="${input:-default}"
    run "true"
}
package as="assets" { copy "/payload" "/payload" from="helper"; }
extend {
    copy "/payload" "/packaged" from="assets"
    copy "/payload" "/current" from="helper"
}
`)
	publication, err := CreateDemandDriven(def, Options{Mode: Publish, TransientRunMounts: []definition.Instruction{
		{Name: "mount", Arguments: []string{"tmpfs"}, Properties: map[string]string{"target": "/publisher"}},
	}}, func(FromSource) (StageBind, error) {
		return StageBind{BaseEnvironment: map[string]string{"input": "publisher"}, Inherited: []definition.Instruction{
			{Name: "copy", Arguments: []string{"private", "/payload"}},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(FromSource) (StageBind, error) {
		return StageBind{BaseEnvironment: map[string]string{"input": "caller"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range invocation.Stages {
		if stage.Name == "helper" {
			if len(stage.Operations) != 2 || stage.Operations[0].Properties["selected"] != "caller" || len(stage.Operations[1].Children) != 0 {
				t.Fatalf("publication base state or transient mounts leaked into invocation: %+v", stage)
			}
			return
		}
	}
	t.Fatal("shared helper missing at invocation")
}

func TestInvocationViewDiscoversPackagesHiddenByPublicationOverride(t *testing.T) {
	def := parse(t, `arg "content" "published"
package as="secondary" { copy "${content}" "/payload"; }
from "scratch" as="helper" { copy "/payload" "/payload" from="secondary"; }
package as="assets" { copy "/payload" "/payload" from="1"; }
extend {
    copy "/payload" "/packaged" from="assets"
    copy "/payload" "/current" from="helper"
}
`)
	publication, err := CreateDemandDriven(def, Options{Mode: Publish, BuildContexts: []buildcontext.Spec{
		{Name: "helper", Kind: buildcontext.DockerImage, Reference: "registry.example/package-helper:latest"},
	}}, func(FromSource) (StageBind, error) { return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, stage := range publication.Stages {
		if stage.Name == "secondary" {
			found = true
			if stage.Operations[0].InputContext != "publisher" || stage.Operations[0].Arguments[0] != "published" {
				t.Fatalf("newly discovered package lost publisher inputs: %+v", stage)
			}
		}
	}
	if !found || publication.PackageArguments["content"] != "published" {
		t.Fatalf("newly discovered package missing or arguments unfrozen: %+v", publication)
	}
	if _, err := Instantiate(publication.Component, Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestLocalOnBuildRetainsNumericReferenceAfterPruning(t *testing.T) {
	for _, trigger := range []string{
		`copy "/payload" "/payload" from="1"`,
		`run "true" { mount "bind" from="1" target="/assets"; }`,
	} {
		t.Run(trigger, func(t *testing.T) {
			publication, err := CreateDemandDriven(parse(t, `package as="unused" {}
package as="assets" { copy "payload" "/payload"; }
from "scratch" as="helper" { onbuild { `+trigger+`; }; }
extend { from "helper" {}; }
`), Options{Mode: Publish}, func(FromSource) (StageBind, error) { t.Fatal("unexpected resolver"); return StageBind{}, nil })
			if err != nil {
				t.Fatal(err)
			}
			if publication.Component.SourceStageIDs["1"] != "0" {
				t.Fatalf("package was not reindexed: %+v", publication.Component.SourceStageIDs)
			}
			if _, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(FromSource) (StageBind, error) {
				t.Fatal("unexpected invocation resolver")
				return StageBind{}, nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, mutation := range []string{"missing", "target", "ordinal", "numeric"} {
				data, err := json.Marshal(publication.Component)
				if err != nil {
					t.Fatal(err)
				}
				var corrupt PublishedComponent
				if err := json.Unmarshal(data, &corrupt); err != nil {
					t.Fatal(err)
				}
				index := -1
				for i, ref := range corrupt.StageReferences {
					if ref.Origin == "local-onbuild" {
						index = i
						break
					}
				}
				if index < 0 {
					t.Fatal("local ONBUILD reference missing")
				}
				switch mutation {
				case "missing":
					corrupt.StageReferences = append(corrupt.StageReferences[:index], corrupt.StageReferences[index+1:]...)
				case "target":
					corrupt.StageReferences[index].Target = corrupt.Output
				case "ordinal":
					corrupt.StageReferences[index].Operation = 99
				case "numeric":
					corrupt.StageReferences[index].SourceIndex = "0"
				}
				if _, err := ValidatePublished(&corrupt); err == nil {
					t.Fatalf("forged local ONBUILD %s accepted", mutation)
				}
			}
		})
	}
}

func TestDynamicLocalBaseDoesNotFreezeAuthoredOnBuild(t *testing.T) {
	for _, body := range []string{
		`onbuild { copy "/payload" "/payload" from="assets"; }; component "./replacement.coopr"`,
		`component "./replacement.coopr"; onbuild { copy "/payload" "/payload" from="assets"; }`,
	} {
		t.Run(body, func(t *testing.T) {
			publication, err := CreateDemandDriven(parse(t, `package as="assets" { copy "payload" "/payload"; }
from "scratch" as="helper" { `+body+`; }
extend { from "helper" {}; }
`), Options{Mode: Publish}, func(FromSource) (StageBind, error) { t.Fatal("unexpected resolver"); return StageBind{}, nil })
			if err != nil {
				t.Fatal(err)
			}
			if len(publication.Stages) != 1 || publication.Stages[0].Name != "assets" {
				t.Fatalf("potential dynamic ONBUILD input missing: %+v", publication.Stages)
			}
			if _, err := ValidatePublished(publication.Component); err != nil {
				t.Fatal(err)
			}
			invocation, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(FromSource) (StageBind, error) {
				t.Fatal("unexpected invocation resolver")
				return StageBind{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, stage := range invocation.Stages {
				if stage.DynamicBaseStage != "" {
					found = true
				}
			}
			if !found {
				t.Fatalf("dynamic helper output was not deferred: %+v", invocation.Stages)
			}
		})
	}
}

func TestDynamicScratchBaseRetainsPackagesForDiscoveredOnBuild(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `package as="assets" { copy "payload" "/payload"; }
from "scratch" as="helper" { component "./replacement.coopr"; }
extend { from "helper" {}; }
`), Options{Mode: Publish}, func(FromSource) (StageBind, error) { t.Fatal("unexpected resolver"); return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(publication.Stages) != 1 || publication.Stages[0].Name != "assets" {
		t.Fatalf("potential runtime ONBUILD input was pruned: %+v", publication.Stages)
	}
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(FromSource) (StageBind, error) {
		t.Fatal("unexpected invocation resolver")
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range invocation.Stages {
		if stage.DynamicBaseStage == "" {
			continue
		}
		replanned, err := invocation.ReplanDynamicStage(stage.ID, StageBind{Inherited: []definition.Instruction{
			{Name: "copy", Arguments: []string{"/payload", "/payload"}, Properties: map[string]string{"from": "assets"}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		for _, result := range replanned.Stages {
			if result.Kind == "package-input" && result.Name == "assets" {
				return
			}
		}
		t.Fatalf("discovered ONBUILD input did not enter invocation graph: %+v", replanned.Stages)
	}
	t.Fatal("dynamic helper was not deferred")
}

func TestPackageDerivedLocalOnBuildKeepsExistingProvenance(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `package as="assets" {}
from "assets" as="helper" { onbuild { copy "/payload" "/payload" from="assets"; }; }
extend { from "helper" {}; }
`), Options{Mode: Publish}, func(FromSource) (StageBind, error) { t.Fatal("unexpected resolver"); return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range publication.Component.StageReferences {
		if ref.Origin != "onbuild" {
			t.Fatalf("package-derived ONBUILD changed existing provenance: %+v", ref)
		}
	}
	if _, err := InstantiateWithPackageBinds(publication.Component, Options{}, map[string]StageBind{"1": {}}); err != nil {
		t.Fatal(err)
	}
}

func TestComponentOnlyOutputDoesNotRetainUnusedPackages(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `package as="unused" { copy "missing" "/missing"; }
extend { component "./settings.coopr"; }
`), Options{Mode: Publish}, func(FromSource) (StageBind, error) { t.Fatal("unexpected resolver"); return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(publication.Stages) != 0 || len(publication.Outputs) != 0 {
		t.Fatalf("unconsumed ONBUILD retained unused package: %+v", publication)
	}
	if _, err := Instantiate(publication.Component, Options{}); err != nil {
		t.Fatal(err)
	}
}
