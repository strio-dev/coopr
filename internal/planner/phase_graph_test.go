package planner

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestComponentPhasesFollowDependencyEdges(t *testing.T) {
	for _, body := range []string{
		`copy "/payload" "/payload" from="helper"`,
		`add "/payload" "/payload" from="helper"`,
		`run "cat /helper/payload" { mount "bind" from="helper" target="/helper"; }`,
		`from "helper" {}`,
		`from "scratch" after="helper" {}`,
	} {
		t.Run(body, func(t *testing.T) {
			def := parse(t, `arg "message" "default"
from "example.invalid/helper:latest" as="helper" {
    arg "message"
    env message="${message}"
}
extend as="output" { `+body+`; }
`)
			publication, err := CreateDemandDriven(def, Options{Mode: Publish}, func(source FromSource) (StageBind, error) {
				t.Fatalf("invocation-only base resolved during publication: %+v", source)
				return StageBind{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(publication.Stages) != 0 || len(publication.PackageArguments) != 0 {
				t.Fatalf("invocation helper executed or fixed arguments during publication: %+v", publication)
			}
			encoded, err := json.Marshal(publication.Component)
			if err != nil {
				t.Fatal(err)
			}
			var restored PublishedComponent
			if err := json.Unmarshal(encoded, &restored); err != nil {
				t.Fatal(err)
			}
			calls := 0
			invocation, err := InstantiateDemandDriven(&restored, Options{Arguments: map[string]string{"message": "caller"}}, nil, func(source FromSource) (StageBind, error) {
				calls++
				if source.Source != "example.invalid/helper:latest" {
					t.Fatalf("unexpected invocation base: %+v", source)
				}
				return StageBind{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("invocation base resolutions = %d", calls)
			}
			found := false
			for _, stage := range invocation.Stages {
				if stage.Name == "helper" {
					found = true
					if stage.Operations[0].Properties["message"] != "caller" {
						t.Fatalf("invocation argument lost: %+v", stage)
					}
				}
			}
			if !found {
				t.Fatal("top-level invocation helper was pruned")
			}
		})
	}
}

func TestComponentPhasesShareProducerAcrossPackageAndInvocation(t *testing.T) {
	def := parse(t, `arg "fixed" "packaged"
from "example.invalid/helper:latest" as="helper" {
    arg "fixed"
    env fixed="${fixed}"
    copy "payload" "/payload"
}
package as="assets" { copy "/payload" "/payload" from="helper"; }
extend {
    copy "/payload" "/packaged" from="assets"
    copy "/payload" "/current" from="helper"
}
`)
	calls := 0
	publication, err := CreateDemandDriven(def, Options{Mode: Publish}, func(FromSource) (StageBind, error) {
		calls++
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(publication.Stages) != 2 || publication.PackageArguments["fixed"] != "packaged" {
		t.Fatalf("publication = %+v, resolutions = %d", publication, calls)
	}
	invocation, err := Instantiate(publication.Component, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(invocation.Stages) != 3 {
		t.Fatalf("shared helper not retained as executable invocation stage: %+v", invocation.Stages)
	}
	var helper, assets bool
	for _, stage := range invocation.Stages {
		helper = helper || stage.Name == "helper" && stage.Kind == "from"
		assets = assets || stage.Name == "assets" && stage.Kind == "package-input"
	}
	if !helper || !assets {
		t.Fatalf("shared invocation must retain helper and immutable package: %+v", invocation.Stages)
	}
	if _, err := Instantiate(publication.Component, Options{Arguments: map[string]string{"fixed": "changed"}}); err == nil || !strings.Contains(err.Error(), "fixed at publication") {
		t.Fatalf("shared producer's package argument override = %v", err)
	}
}

func TestComponentPhasesPackageCanUseIndependentNestedFrom(t *testing.T) {
	def := parse(t, `extend as="helpers" {
    from "scratch" as="helper" { copy "payload" "/payload"; }
}
package as="assets" { copy "/payload" "/payload" from="helper"; }
extend as="output" { copy "/payload" "/payload" from="assets"; }
`)
	publication, err := Create(def, Options{Mode: Publish})
	if err != nil {
		t.Fatal(err)
	}
	if len(publication.Stages) != 2 || publication.Stages[0].Name != "helper" || publication.Stages[1].Kind != "package" {
		t.Fatalf("independent nested producer = %+v", publication.Stages)
	}
	invocation, err := Instantiate(publication.Component, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(invocation.Stages) != 2 || invocation.Stages[0].Kind != "package-input" {
		t.Fatalf("package producer replayed at invocation: %+v", invocation.Stages)
	}
}

func TestComponentPhasesRejectActualCallerDependencies(t *testing.T) {
	for _, source := range []string{
		`extend as="caller" { env value="caller"; }; package as="assets" { copy "/x" "/x" from="caller"; }; extend as="output" { copy "/x" "/x" from="assets"; }`,
		`extend as="caller" { env value="caller"; }; from "caller" as="helper" {}; package as="assets" { copy "/x" "/x" from="helper"; }; extend as="output" { copy "/x" "/x" from="assets"; }`,
	} {
		_, err := Create(parse(t, source), Options{Mode: Publish})
		if err == nil || !strings.Contains(err.Error(), "extend") {
			t.Fatalf("actual caller dependency accepted or incorrect error: %v", err)
		}
	}
}

func TestComponentPhasesTopLevelFromMayDependOnCaller(t *testing.T) {
	publication := makePlan(t, `extend as="caller" { env inherited="caller"; }
from "caller" as="helper" { env copied="${inherited}"; }
extend as="output" { copy "/payload" "/payload" from="helper"; }
`, Options{Mode: Publish})
	if len(publication.Stages) != 0 {
		t.Fatalf("caller-dependent helper scheduled at publication: %+v", publication.Stages)
	}
	invocation := invokePublished(t, publication, Options{})
	if len(invocation.Stages) != 3 || invocation.Stages[1].Name != "helper" || invocation.Stages[1].Operations[0].Properties["copied"] != "caller" {
		t.Fatalf("caller-dependent helper not preserved at invocation: %+v", invocation.Stages)
	}
}

func TestComponentPhasesLocalOnBuildKeepsPackageDependency(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `package as="assets" { copy "payload" "/payload"; }
from "scratch" as="helper" { onbuild { copy "/payload" "/payload" from="assets"; }; }
extend { from "helper" {}; }
`), Options{Mode: Publish}, func(source FromSource) (StageBind, error) {
		t.Fatalf("unexpected external base: %+v", source)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(publication.Stages) != 1 || publication.Stages[0].Name != "assets" {
		t.Fatalf("local ONBUILD package dependency pruned: %+v", publication.Stages)
	}
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(source FromSource) (StageBind, error) {
		t.Fatalf("unexpected invocation base: %+v", source)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range invocation.Stages {
		if len(stage.Operations) != 0 && stage.Operations[0].Name == "copy" {
			if len(stage.Dependencies) < 2 {
				t.Fatalf("local ONBUILD dependency absent at invocation: %+v", stage)
			}
			return
		}
	}
	t.Fatal("local ONBUILD was not planned at invocation")
}

func TestComponentPhasesDormantHelperUsesInvocationArguments(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `from "scratch" as="helper" {
    arg "input"
    copy "${input}" "/payload"
}
extend { from "registry.example/output:latest" {}; }
`), Options{Mode: Publish}, func(FromSource) (StageBind, error) {
		t.Fatal("invocation-only image resolved during publication")
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(publication.Stages) != 0 || len(publication.PackageArguments) != 0 {
		t.Fatalf("dormant helper executed or froze arguments: %+v", publication)
	}
	invocation, err := InstantiateDemandDriven(publication.Component, Options{Arguments: map[string]string{"input": "caller-file"}}, nil, func(FromSource) (StageBind, error) {
		return StageBind{Inherited: parse(t, `from "scratch" { copy "/payload" "/proof" from="helper"; }`).Instructions[0].Children}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range invocation.Stages {
		if stage.Name == "helper" {
			for _, operation := range stage.Operations {
				if operation.Name == "copy" && operation.Arguments[0] == "caller-file" && operation.InputContext == "caller" {
					return
				}
			}
			t.Fatalf("dormant helper lost invocation argument/context: %+v", stage)
		}
	}
	t.Fatal("inherited dependency did not activate dormant helper")
}
