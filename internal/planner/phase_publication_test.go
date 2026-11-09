package planner

import (
	"slices"
	"strings"
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
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, emptyPackageBinds(t, publication.Component), func(FromSource) (StageBind, error) {
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

func TestLocalOnBuildPreservesOriginalNumericReference(t *testing.T) {
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
			projected, err := ProjectPublished(publication.Component)
			if err != nil {
				t.Fatal(err)
			}
			if projected.Stages[1].Name != "assets" || projected.Stages[1].Head.UnavailablePackage || !projected.Stages[0].Head.UnavailablePackage {
				t.Fatalf("original package identities or capture status changed: %+v", projected.Stages)
			}
			invocation, err := InstantiateDemandDriven(publication.Component, Options{}, map[string]StageBind{"1": {}}, unexpectedBindResolver(t))
			if err != nil {
				t.Fatal(err)
			}
			child := invocation.Stages[len(invocation.Stages)-1]
			if !slices.Contains(child.Dependencies, "1") || len(child.Operations) != 1 {
				t.Fatalf("local ONBUILD did not select original package: %+v", child)
			}
			op := child.Operations[0]
			source := op.Properties["from"]
			if op.Name == "run" {
				source = op.Children[0].Properties["from"]
			}
			if source != "1" {
				t.Fatalf("local ONBUILD numeric source changed: %+v", op)
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
			if len(publication.Stages) != 0 || !publication.Component.Definition.Instructions[0].UnavailablePackage {
				t.Fatalf("unknown dynamic ONBUILD captured an unselected package: %+v", publication.Stages)
			}
			if _, err := ValidatePublished(publication.Component); err != nil {
				t.Fatal(err)
			}
			invocation, err := InstantiateDemandDriven(publication.Component, Options{}, emptyPackageBinds(t, publication.Component), unexpectedBindResolver(t))
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

func TestDynamicScratchBaseRejectsUncapturedPackageDiscoveredByOnBuild(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `package as="assets" { copy "payload" "/payload"; }
from "scratch" as="helper" { component "./replacement.coopr"; }
extend { from "helper" {}; }
`), Options{Mode: Publish}, func(FromSource) (StageBind, error) { t.Fatal("unexpected resolver"); return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(publication.Stages) != 0 || !publication.Component.Definition.Instructions[0].UnavailablePackage {
		t.Fatalf("unselected package was captured speculatively: %+v", publication.Stages)
	}
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, emptyPackageBinds(t, publication.Component), unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range invocation.Stages {
		if stage.DynamicBaseStage == "" {
			continue
		}
		_, err := invocation.ReplanDynamicStage(stage.ID, StageBind{Inherited: []definition.Instruction{
			{Name: "copy", Arguments: []string{"/payload", "/payload"}, Properties: map[string]string{"from": "assets"}},
		}})
		if err == nil || !strings.Contains(err.Error(), `package "assets" (original stage 0) was not captured at publication`) {
			t.Fatalf("late ONBUILD did not reject unavailable package: %v", err)
		}
		return
	}
	t.Fatal("dynamic helper was not deferred")
}

func TestPackageDerivedLocalOnBuildUsesOriginalSnapshot(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `package as="assets" {}
from "assets" as="helper" { onbuild { copy "/payload" "/payload" from="assets"; }; }
extend { from "helper" {}; }
`), Options{Mode: Publish}, func(FromSource) (StageBind, error) { t.Fatal("unexpected resolver"); return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := InstantiateWithPackageBinds(publication.Component, Options{}, map[string]StageBind{"0": {}})
	if err != nil {
		t.Fatal(err)
	}
	child := invocation.Stages[len(invocation.Stages)-1]
	if len(child.Operations) != 1 || child.Operations[0].Properties["from"] != "assets" || !slices.Contains(child.Dependencies, "0") {
		t.Fatalf("package-derived local ONBUILD was not replayed: %+v", child)
	}
}

func TestComponentOnlyOutputDoesNotCaptureUnusedPackages(t *testing.T) {
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
