package planner

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"coopr/internal/definition"
)

func TestPublishedGraphValidation(t *testing.T) {
	def := parse(t, "package as=\"pkg\"\nextend as=\"base\"\ncopy \"/src\" \"/dst\" from=\"pkg\"\nfrom \"base\" as=\"final\"\n")
	publication, err := Create(def, Options{Mode: Publish, Platform: "linux/arm64"})
	if err != nil {
		t.Fatal(err)
	}
	packages, err := ValidatePublished(publication.Component)
	if err != nil || len(packages) != 1 || packages[0] != "pkg" {
		t.Fatalf("valid publication rejected: %v, %v", packages, err)
	}
	if _, err := Instantiate(publication.Component, Options{Platform: "linux/arm64/v8"}); err != nil {
		t.Fatalf("arm64/v8 alias rejected: %v", err)
	}
	clone := func() *PublishedComponent {
		data, _ := json.Marshal(publication.Component)
		var copy PublishedComponent
		if err := json.Unmarshal(data, &copy); err != nil {
			t.Fatal(err)
		}
		return &copy
	}
	for _, test := range []struct {
		name   string
		change func(*PublishedComponent)
		want   string
	}{
		{"selected package", func(c *PublishedComponent) { c.Output = "0" }, "invalid published output"},
		{"lost copy binding", func(c *PublishedComponent) { c.StageReferences = nil }, "stage reference missing"},
		{"wrong from binding", func(c *PublishedComponent) { c.FromBindings["2"] = FromBinding{Kind: "image"} }, "image binding reuses stage name"},
		{"unreachable retained package", func(c *PublishedComponent) {
			c.Definition.Instructions = append(c.Definition.Instructions, definition.Instruction{Name: "package", Properties: map[string]string{"as": "unused"}})
			c.ReservedStageNames = append(c.ReservedStageNames, "unused")
		}, "outside selected output closure"},
	} {
		t.Run(test.name, func(t *testing.T) {
			component := clone()
			test.change(component)
			if _, err := ValidatePublished(component); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("tampered publication accepted or wrong error: %v", err)
			}
		})
	}
}

func TestPublishedGraphValidationMatchesStageAliasesCaseInsensitively(t *testing.T) {
	publication, err := Create(parse(t, `
package as="pkg"
extend as="base"
copy "/src" "/dst" from="pkg"
from "base" as="final"
`), Options{Mode: Publish})
	if err != nil {
		t.Fatal(err)
	}
	for i := range publication.Component.Definition.Instructions {
		inst := &publication.Component.Definition.Instructions[i]
		switch inst.Name {
		case "package":
			inst.Properties["as"] = "Pkg"
		case "extend":
			inst.Properties["as"] = "Base"
		case "copy":
			inst.Properties["from"] = "pKg"
		case "from":
			inst.Arguments[0] = "BASE"
			inst.Properties["as"] = "Final"
		}
	}
	publication.Component.ReservedStageNames = []string{"Base", "Final", "Pkg"}
	packages, err := ValidatePublished(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 1 || packages[0] != "Pkg" {
		t.Fatalf("published packages = %v, want [Pkg]", packages)
	}
	graph, err := ProjectPublished(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.PackageLeaves) != 1 || graph.PackageLeaves[0] != "0" {
		t.Fatalf("package leaves = %v, want [0]", graph.PackageLeaves)
	}
}

func TestPublishedPackageOnBuildReferenceIsFrozen(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `
package as="assets"
package as="payload"
onbuild { copy "/asset" "/selected" from="assets" }
from "payload" as="prepared"
extend
copy "/selected" "/selected" from="prepared"
`), Options{Mode: Publish}, func(source FromSource) (StageBind, error) {
		t.Fatalf("unexpected external source %+v", source)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidatePublished(publication.Component); err != nil {
		t.Fatal(err)
	}
	projection, err := ProjectPublished(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	prepared := ""
	for _, stage := range projection.Stages {
		if stage.Name == "prepared" {
			prepared = stage.ID
		}
	}
	if prepared == "" || len(projection.PackageLeaves) != 2 {
		t.Fatalf("published hidden package graph = %+v", projection)
	}
	bind := StageBind{Inherited: []definition.Instruction{{
		Name: "copy", Arguments: []string{"/asset", "/selected"}, Properties: map[string]string{"from": "assets"},
	}}}
	if _, err := InstantiateWithPackageBinds(publication.Component, Options{}, map[string]StageBind{prepared: bind}); err != nil {
		t.Fatalf("verified package trigger rejected: %v", err)
	}
	clone := func() *PublishedComponent {
		data, err := json.Marshal(publication.Component)
		if err != nil {
			t.Fatal(err)
		}
		var copy PublishedComponent
		if err := json.Unmarshal(data, &copy); err != nil {
			t.Fatal(err)
		}
		return &copy
	}
	onbuild := -1
	for i, ref := range publication.Component.StageReferences {
		if ref.Origin == "onbuild" {
			onbuild = i
		}
	}
	if onbuild < 0 {
		t.Fatalf("hidden ONBUILD reference missing: %+v", publication.Component.StageReferences)
	}
	for _, test := range []struct {
		name   string
		change func(*PublishedComponent)
		want   string
	}{
		{"missing", func(c *PublishedComponent) {
			c.StageReferences = append(c.StageReferences[:onbuild], c.StageReferences[onbuild+1:]...)
		}, "outside selected output closure"},
		{"wrong target", func(c *PublishedComponent) { c.StageReferences[onbuild].Target = "999" }, "invalid ONBUILD stage reference target"},
		{"wrong kind", func(c *PublishedComponent) { c.StageReferences[onbuild].Kind = "image" }, "invalid ONBUILD image reference target"},
		{"duplicate", func(c *PublishedComponent) {
			c.StageReferences = append(c.StageReferences[:onbuild+1], append([]StageReferenceBinding{c.StageReferences[onbuild]}, c.StageReferences[onbuild+1:]...)...)
		}, "invalid stage reference order"},
	} {
		t.Run(test.name, func(t *testing.T) {
			component := clone()
			test.change(component)
			if _, err := ValidatePublished(component); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("tampered publication accepted or wrong error: %v", err)
			}
		})
	}
	wrongOperation := clone()
	wrongOperation.StageReferences[onbuild].Operation = 17
	if _, err := ValidatePublished(wrongOperation); err != nil {
		t.Fatalf("static validation should defer ONBUILD operation count to package config: %v", err)
	}
	if _, err := InstantiateWithPackageBinds(wrongOperation, Options{}, map[string]StageBind{prepared: bind}); err == nil || !strings.Contains(err.Error(), "stage copy or mount references changed") {
		t.Fatalf("package trigger did not verify frozen operation index: %v", err)
	}
}

func TestDemandDrivenInvocationRetainsPackageForExternalOnBuild(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `package as="assets"
copy "./asset" "/asset"
from "helper" as="prepared"
extend
copy "/selected" "/selected" from="prepared"
`), Options{Mode: Publish}, func(source FromSource) (StageBind, error) {
		if source.Source == "helper" {
			t.Fatal("invocation-only base resolved during publication")
		}
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := publication.Outputs, []string{"0"}; !slices.Equal(got, want) {
		t.Fatalf("package outputs = %v, want %v", got, want)
	}
	if got, want := publication.Component.DormantRoots, []string{"0"}; !slices.Equal(got, want) {
		t.Fatalf("dormant roots = %v, want %v", got, want)
	}

	resolved := 0
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(source FromSource) (StageBind, error) {
		resolved++
		if source.Source != "helper" {
			t.Fatalf("resolved source = %+v", source)
		}
		return StageBind{Inherited: []definition.Instruction{{
			Name: "copy", Arguments: []string{"/asset", "/selected"}, Properties: map[string]string{"from": "assets"},
		}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved != 1 {
		t.Fatalf("external resolutions = %d, want 1", resolved)
	}
	if got, want := stageIDs(invocation), []string{"0", "1", "2"}; !slices.Equal(got, want) {
		t.Fatalf("invocation stages = %v, want %v", got, want)
	}
	if invocation.Stages[0].Kind != "package-input" || !invocation.Stages[1].InheritedOnBuildPlanned {
		t.Fatalf("invocation did not activate package-backed ONBUILD: %+v", invocation.Stages)
	}

	clone := func() *PublishedComponent {
		data, marshalErr := json.Marshal(publication.Component)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		var component PublishedComponent
		if unmarshalErr := json.Unmarshal(data, &component); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		return &component
	}
	for _, test := range []struct {
		name   string
		change func(*PublishedComponent)
	}{
		{"missing dormant root", func(component *PublishedComponent) { component.DormantRoots = nil }},
		{"unknown dormant root", func(component *PublishedComponent) { component.DormantRoots = []string{"999"} }},
		{"duplicate dormant root", func(component *PublishedComponent) { component.DormantRoots = []string{"0", "0"} }},
		{"selected dependency as dormant root", func(component *PublishedComponent) { component.DormantRoots = []string{"1"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			component := clone()
			test.change(component)
			if _, validateErr := ValidatePublished(component); validateErr == nil {
				t.Fatal("tampered dormant roots accepted")
			}
		})
	}
}

func TestDemandDrivenPublicationKeepsDormantInvocationBranchOffline(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `arg "runtime_base" "registry.invalid/runtime:latest"
from "registry.invalid/dormant:latest" as="unused"
arg "required"
env REQUIRED="${required}"
package as="assets"
copy "./asset" "/asset"
from "${runtime_base}" as="prepared"
extend
copy "/selected" "/selected" from="prepared"
`), Options{Mode: Publish}, func(source FromSource) (StageBind, error) {
		t.Fatalf("publication resolved invocation-only image: %+v", source)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved := 0
	if _, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(source FromSource) (StageBind, error) {
		resolved++
		if source.Source != "registry.invalid/runtime:latest" {
			t.Fatalf("dormant image was resolved: %+v", source)
		}
		return StageBind{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if resolved != 1 {
		t.Fatalf("resolved images = %d, want selected runtime only", resolved)
	}
}

func TestDemandDrivenInvocationSkipsUnselectedDormantBindings(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `arg "runtime_base" "registry.invalid/runtime:latest"
package as="assets"
copy "./asset" "/asset"
from "assets" as="unused"
copy "/asset" "/unused" from="assets"
from "${runtime_base}" as="prepared"
extend
copy "/selected" "/selected" from="prepared"
`), Options{Mode: Publish}, func(source FromSource) (StageBind, error) {
		t.Fatalf("publication resolved invocation-only image: %+v", source)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InstantiateDemandDriven(publication.Component, Options{
		Arguments: map[string]string{"runtime_base": "registry.example/base:latest"},
	}, map[string]StageBind{"1": {}}, func(source FromSource) (StageBind, error) {
		if source.Source != "registry.example/base:latest" {
			t.Fatalf("unexpected invocation base: %+v", source)
		}
		return StageBind{}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDemandDrivenInvocationRejectsFixedArgumentBeforeResolvingBase(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `arg "base" "registry.example/base:published"
package as="pkg"
env BASE="${base}"
from "${base}" as="helper"
extend
copy "/asset" "/asset" from="helper"
`), Options{Mode: Publish}, func(source FromSource) (StageBind, error) {
		t.Fatalf("publication resolved invocation-only image: %+v", source)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := publication.Component.PackageArguments["base"]; got != "registry.example/base:published" {
		t.Fatalf("fixed package argument = %q", got)
	}
	_, err = InstantiateDemandDriven(publication.Component, Options{
		Arguments: map[string]string{"base": "registry.example/base:changed"},
	}, nil, func(source FromSource) (StageBind, error) {
		t.Fatalf("resolved external image before rejecting fixed override: %+v", source)
		return StageBind{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "fixed at publication") {
		t.Fatalf("fixed argument error = %v", err)
	}
}

func TestDemandDrivenInvocationResolvesExternalOnBuildForwardReference(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `from "helper" as="prepared"
package as="assets"
copy "./asset" "/asset"
extend
copy "/selected" "/selected" from="prepared"
`), Options{Mode: Publish}, func(FromSource) (StageBind, error) { return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if got, want := publication.Component.DormantRoots, []string{"1"}; !slices.Equal(got, want) {
		t.Fatalf("dormant roots = %v, want %v", got, want)
	}
	if got, want := publication.Outputs, []string{"1"}; !slices.Equal(got, want) {
		t.Fatalf("package outputs = %v, want %v", got, want)
	}
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(source FromSource) (StageBind, error) {
		if source.Source != "helper" || source.Kind != FromSourceImage {
			t.Fatalf("resolved source = %+v", source)
		}
		return StageBind{Inherited: []definition.Instruction{{
			Name: "copy", Arguments: []string{"/asset", "/selected"}, Properties: map[string]string{"from": "assets"},
		}, {
			Name: "run", Form: "shell", Arguments: []string{"test -e /mnt/asset"}, Children: []definition.Instruction{{
				Name: "mount", Arguments: []string{"bind"}, Properties: map[string]string{
					"from": "assets", "source": "/", "target": "/mnt", "readonly": "true",
				},
			}},
		}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stageIDs(invocation), []string{"1", "0", "2"}; !slices.Equal(got, want) {
		t.Fatalf("invocation stages = %v, want %v", got, want)
	}
	if got := invocation.Stages[1].Operations[0].Properties["from"]; got != "assets" {
		t.Fatalf("inherited COPY source = %q", got)
	}
	if got := invocation.Stages[1].Operations[1].Children[0].Properties["from"]; got != "assets" {
		t.Fatalf("inherited RUN bind source = %q", got)
	}
	if invocation.Stages[0].Kind != "package-input" {
		t.Fatalf("forward package was not restored from its snapshot: %+v", invocation.Stages[0])
	}
}

func TestDemandDrivenInvocationPreservesExternalOnBuildForwardNumericReference(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `from "helper" as="prepared"
extend as="unused"
from "scratch"
run "prepare numeric asset"
extend
copy "/selected" "/selected" from="prepared"
`), Options{Mode: Publish}, func(FromSource) (StageBind, error) { return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if got, want := publication.Component.DormantRoots, []string{"1", "2"}; !slices.Equal(got, want) {
		t.Fatalf("dormant roots = %v, want %v", got, want)
	}
	if got := publication.Component.Definition.Instructions[1].Properties["as"]; got != "" {
		t.Fatalf("dormant extend alias remained invocation-visible: %q", got)
	}
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(source FromSource) (StageBind, error) {
		if source.Source != "helper" || source.Kind != FromSourceImage {
			t.Fatalf("resolved source = %+v", source)
		}
		return StageBind{Inherited: []definition.Instruction{{
			Name: "copy", Arguments: []string{"/asset", "/selected"}, Properties: map[string]string{"from": "2"},
		}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stageIDs(invocation), []string{"2", "0", "3"}; !slices.Equal(got, want) {
		t.Fatalf("invocation stages = %v, want %v", got, want)
	}
	if got := invocation.Stages[1].Operations[0].Properties["from"]; got != "2" {
		t.Fatalf("inherited numeric COPY source = %q", got)
	}
	if invocation.Stages[0].Source != "scratch" {
		t.Fatalf("numeric source rebound to wrong stage: %+v", invocation.Stages[0])
	}
}

func TestDemandDrivenInvocationResolvesExternalOnBuildImageSource(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `from "helper" as="prepared"
extend
copy "/selected" "/selected" from="prepared"
`), Options{Mode: Publish}, func(FromSource) (StageBind, error) { return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	var resolved []FromSource
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(source FromSource) (StageBind, error) {
		resolved = append(resolved, source)
		if source.Kind == FromSourceImage {
			return StageBind{Inherited: []definition.Instruction{{
				Name: "copy", Arguments: []string{"/asset", "/selected"}, Properties: map[string]string{"from": "registry.example/assets:latest"},
			}, {
				Name: "run", Form: "shell", Arguments: []string{"test -e /mnt/rootfs"}, Children: []definition.Instruction{{
					Name: "mount", Arguments: []string{"bind"}, Properties: map[string]string{
						"from": "registry.example/rootfs:latest", "source": "/", "target": "/mnt/rootfs", "readonly": "true",
					},
				}},
			}}}, nil
		}
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resolved, []FromSource{
		{StageID: "0", Source: "helper", Platform: "linux/amd64", Kind: FromSourceImage},
		{StageID: "0", Source: "registry.example/assets:latest", Platform: "linux/amd64", Kind: FromSourceCopyImage},
		{StageID: "0", Source: "registry.example/rootfs:latest", Platform: "linux/amd64", Kind: FromSourceCopyImage},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved sources = %+v, want %+v", got, want)
	}
	if got := invocation.Stages[0].Operations[0].Properties["from"]; got != "registry.example/assets:latest" {
		t.Fatalf("inherited COPY image source = %q", got)
	}
	if got := invocation.Stages[0].Operations[1].Children[0].Properties["from"]; got != "registry.example/rootfs:latest" {
		t.Fatalf("inherited RUN bind image source = %q", got)
	}
}

func TestDemandDrivenInvocationDoesNotBindCallerDerivedStage(t *testing.T) {
	publication := makePlan(t, "extend as=\"base\"\nfrom \"base\"\n", Options{Mode: Publish})
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, nil, func(source FromSource) (StageBind, error) {
		t.Fatalf("caller-derived stage requested external metadata: %+v", source)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if invocation.Stages[len(invocation.Stages)-1].InheritedOnBuildPlanned {
		t.Fatalf("caller-derived stage was marked statically planned: %+v", invocation.Stages)
	}
}
