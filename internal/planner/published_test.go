package planner

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/definition"
)

func TestPublishedGraphValidation(t *testing.T) {
	def := parse(t, `package as="pkg" {}
extend as="base" {
    copy "/src" "/dst" from="pkg"
}
extend as="final" {
    from "base" {}
}
`)
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
	for _, test := range []struct {
		name   string
		change func(*PublishedComponent)
		want   string
	}{
		{"selected package", func(c *PublishedComponent) { c.Output = "0" }, "invalid published output"},
		{"missing format", func(c *PublishedComponent) { c.Format = "" }, "residual component format"},
		{"unknown format", func(c *PublishedComponent) { c.Format = "future" }, "residual component format"},
		{"package producer", func(c *PublishedComponent) {
			c.Definition.Instructions[0].Children = []definition.Instruction{{Name: "run", Arguments: []string{"touch /mutated"}}}
		}, "producer instructions"},
		{"unavailable non-package", func(c *PublishedComponent) { c.Definition.Instructions[1].UnavailablePackage = true }, "unavailable marker requires PACKAGE"},
		{"duplicate alias", func(c *PublishedComponent) { c.Definition.Instructions[1].Properties["as"] = "PKG" }, "duplicate stage name"},
	} {
		t.Run(test.name, func(t *testing.T) {
			component := roundTripPublished(t, publication.Component)
			test.change(&component)
			if _, err := ValidatePublished(&component); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("tampered publication accepted or wrong error: %v", err)
			}
		})
	}
}

func TestPublishedGraphValidationMatchesStageAliasesCaseInsensitively(t *testing.T) {
	publication := makePlan(t, `
package as="pkg" {}
extend as="base" {
    copy "/src" "/dst" from="pkg"
}
extend as="final" {
    from "base" {}
}
`, Options{Mode: Publish})
	for i := range publication.Component.Definition.Instructions {
		inst := &publication.Component.Definition.Instructions[i]
		switch inst.Name {
		case "package":
			inst.Properties["as"] = "Pkg"
		case "extend":
			if inst.Properties["as"] == "base" {
				inst.Properties["as"] = "Base"
				inst.Children[0].Properties["from"] = "pKg"
			} else {
				inst.Properties["as"] = "Final"
				inst.Children[0].Arguments[0] = "BASE"
			}
		}
	}
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

func TestPublishedPackageOnBuildUsesVerifiedSnapshotAndOriginalNamespace(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `package as="assets" {}
package as="payload" { onbuild { copy "/asset" "/selected" from="assets"; }; }
extend {
    from "payload" as="prepared" {}
    copy "/selected" "/selected" from="prepared"
}
`), Options{Mode: Publish}, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	projected, err := ProjectPublished(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(projected.PackageLeaves, []string{"0", "1"}) || len(projected.Stages[1].Body) != 0 {
		t.Fatalf("package snapshots or erased bodies changed: %+v", projected)
	}
	bind := StageBind{Inherited: []definition.Instruction{{Name: "copy", Arguments: []string{"/asset", "/selected"}, Properties: map[string]string{"from": "assets"}}}}
	invocation, err := InstantiateWithPackageBinds(publication.Component, Options{}, map[string]StageBind{"0": {}, "1": bind})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stageIDs(invocation), []string{"1", "0", "3", "2"}) {
		t.Fatalf("verified trigger graph: %+v", invocation.Stages)
	}
	child := invocation.Stages[2]
	if !slices.Equal(child.Dependencies, []string{"1", "0"}) || len(child.Operations) != 1 || child.Operations[0].Properties["from"] != "assets" {
		t.Fatalf("verified package ONBUILD not applied: %+v", child)
	}
	for _, binds := range []map[string]StageBind{nil, {"0": {}, "3": bind}, {"0": {}, "1": bind, "3": {}}} {
		if _, err := InstantiateWithPackageBinds(publication.Component, Options{}, binds); err == nil {
			t.Fatalf("binds not keyed by captured original PACKAGE IDs accepted: %+v", binds)
		}
	}
}

func TestDemandDrivenInvocationRejectsUncapturedPackageForExternalOnBuild(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `package as="assets" { copy "./asset" "/asset"; }
extend {
    from "helper" as="prepared" {}
    copy "/selected" "/selected" from="prepared"
}
`), Options{Mode: Publish}, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(publication.Outputs) != 0 || len(publication.Stages) != 0 || !publication.Component.Definition.Instructions[0].UnavailablePackage || len(publication.Component.Definition.Instructions[0].Children) != 0 {
		t.Fatalf("unselected package was captured or retained executable body: %+v", publication)
	}
	resolved := 0
	_, err = InstantiateDemandDriven(publication.Component, Options{}, nil, func(source FromSource) (StageBind, error) {
		resolved++
		if source.Source != "helper" {
			t.Fatalf("unexpected source: %+v", source)
		}
		return StageBind{Inherited: []definition.Instruction{{Name: "copy", Arguments: []string{"/asset", "/selected"}, Properties: map[string]string{"from": "assets"}}}}, nil
	})
	if resolved != 1 || err == nil || !strings.Contains(err.Error(), `package "assets" (original stage 0) was not captured at publication`) {
		t.Fatalf("late ONBUILD did not fail at unavailable package: resolutions=%d error=%v", resolved, err)
	}
}

func TestDemandDrivenPublicationKeepsDormantInvocationBranchOffline(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `arg "runtime_base" "registry.invalid/runtime:latest"
from "registry.invalid/dormant:latest" as="unused" {
    arg "required"
    env REQUIRED="${required}"
}
package as="assets" {
    copy "./asset" "/asset"
}
extend {
    from "${runtime_base}" as="prepared" {}
    copy "/selected" "/selected" from="prepared"
}
`), Options{Mode: Publish}, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	resolved := 0
	if _, err := InstantiateDemandDriven(publication.Component, Options{}, emptyPackageBinds(t, publication.Component), func(source FromSource) (StageBind, error) {
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

func TestDemandDrivenInvocationSkipsUnselectedUnavailablePackages(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `arg "runtime_base" "registry.invalid/runtime:latest"
package as="assets" {
    copy "./asset" "/asset"
}
extend as="unused" {
    from "assets" {
        copy "/asset" "/unused" from="assets"
    }
}
extend {
    from "${runtime_base}" as="prepared" {}
    copy "/selected" "/selected" from="prepared"
}
`), Options{Mode: Publish}, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InstantiateDemandDriven(publication.Component, Options{
		Arguments: map[string]string{"runtime_base": "registry.example/base:latest"},
	}, emptyPackageBinds(t, publication.Component), func(source FromSource) (StageBind, error) {
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
package as="pkg" {
    env BASE="${base}"
}
extend {
    from "${base}" as="helper" {}
    copy "/asset" "/asset" from="helper"
    copy "/fixed" "/fixed" from="pkg"
}
`), Options{Mode: Publish}, unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := publication.Component.PackageArguments["base"]; got != "registry.example/base:published" {
		t.Fatalf("fixed package argument = %q", got)
	}
	_, err = InstantiateDemandDriven(publication.Component, Options{
		Arguments: map[string]string{"base": "registry.example/base:changed"},
	}, emptyPackageBinds(t, publication.Component), unexpectedBindResolver(t))
	if err == nil || !strings.Contains(err.Error(), "fixed at publication") {
		t.Fatalf("fixed argument error = %v", err)
	}
}

func TestDemandDrivenInvocationResolvesExternalOnBuildEarlierReference(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `package as="assets" {
    copy "./asset" "/asset"
}
extend as="runtime" {
    from "helper" as="prepared" {}
    copy "/selected" "/selected" from="prepared"
    copy "/asset" "/captured" from="assets"
}
`), Options{Mode: Publish, Target: "runtime"}, func(FromSource) (StageBind, error) { return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	projected, err := ProjectPublished(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	if len(projected.Stages) != 3 || projected.Stages[0].Name != "assets" || !slices.Equal(projected.PackageLeaves, []string{"0"}) {
		t.Fatalf("original namespace lost: %+v", projected.Stages)
	}
	if got, want := publication.Outputs, []string{"0"}; !slices.Equal(got, want) {
		t.Fatalf("package outputs = %v, want %v", got, want)
	}
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, emptyPackageBinds(t, publication.Component), func(source FromSource) (StageBind, error) {
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
	if got, want := stageIDs(invocation), []string{"0", "2", "1"}; !slices.Equal(got, want) {
		t.Fatalf("invocation stages = %v, want %v", got, want)
	}
	if got := invocation.Stages[1].Operations[0].Properties["from"]; got != "assets" {
		t.Fatalf("inherited COPY source = %q", got)
	}
	if got := invocation.Stages[1].Operations[1].Children[0].Properties["from"]; got != "assets" {
		t.Fatalf("inherited RUN bind source = %q", got)
	}
	if invocation.Stages[0].Kind != "package-input" {
		t.Fatalf("earlier package was not restored from its snapshot: %+v", invocation.Stages[0])
	}
}

func TestDemandDrivenInvocationPreservesExternalOnBuildEarlierNumericReference(t *testing.T) {
	publication, err := CreateDemandDriven(parse(t, `extend as="unused" {}
extend {
    from "scratch" {
        run "prepare numeric asset"
    }
}
extend as="runtime" {
    from "helper" as="prepared" {}
    copy "/selected" "/selected" from="prepared"
}
`), Options{Mode: Publish, Target: "runtime"}, func(FromSource) (StageBind, error) { return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	projected, err := ProjectPublished(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	if len(projected.Stages) != 5 || projected.Stages[0].Name != "unused" || projected.Stages[2].Kind != "from" || projected.Stages[2].Head.Arguments[0] != "scratch" {
		t.Fatalf("original namespace lost: %+v", projected.Stages)
	}
	if got := publication.Component.Definition.Instructions[0].Properties["as"]; got != "unused" {
		t.Fatalf("dormant extend alias changed during publication: %q", got)
	}
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, emptyPackageBinds(t, publication.Component), func(source FromSource) (StageBind, error) {
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
	if got, want := stageIDs(invocation), []string{"2", "4", "3"}; !slices.Equal(got, want) {
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
	publication, err := CreateDemandDriven(parse(t, `extend {
    from "helper" as="prepared" {}
    copy "/selected" "/selected" from="prepared"
}
`), Options{Mode: Publish}, func(FromSource) (StageBind, error) { return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	var resolved []FromSource
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, emptyPackageBinds(t, publication.Component), func(source FromSource) (StageBind, error) {
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
		{StageID: "1", Source: "helper", Platform: "linux/amd64", Kind: FromSourceImage},
		{StageID: "1", Source: "registry.example/assets:latest", Platform: "linux/amd64", Kind: FromSourceCopyImage, Role: "copy"},
		{StageID: "1", Source: "registry.example/rootfs:latest", Platform: "linux/amd64", Kind: FromSourceCopyImage, Role: "run-mount"},
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
	publication := makePlan(t, `extend as="base" {}
extend {
    from "base" {}
}
`, Options{Mode: Publish})
	invocation, err := InstantiateDemandDriven(publication.Component, Options{}, emptyPackageBinds(t, publication.Component), unexpectedBindResolver(t))
	if err != nil {
		t.Fatal(err)
	}
	if invocation.Stages[len(invocation.Stages)-1].InheritedOnBuildPlanned {
		t.Fatalf("caller-derived stage was marked statically planned: %+v", invocation.Stages)
	}
}

func TestPublishedPlatformComparisonUsesNormalizedExactIdentity(t *testing.T) {
	for _, test := range []struct {
		left, right string
		equal       bool
	}{{"linux/arm64", "linux/arm64/v8", true}, {"linux/x86_64", "linux/amd64", true}, {"linux/arm/v6", "linux/arm/v7", false}, {"linux/amd64", "linux/arm64", false}, {"linux/amd64", "windows/amd64", false}} {
		if got := samePlatform(test.left, test.right); got != test.equal {
			t.Errorf("samePlatform(%q,%q) = %v", test.left, test.right, got)
		}
	}
}

// Fixtures that do not consume package configuration still supply each verified
// snapshot's empty configuration through the same original-ID boundary.
func emptyPackageBinds(t *testing.T, component *PublishedComponent) map[string]StageBind {
	t.Helper()
	raw, err := ProjectPublished(component)
	if err != nil {
		t.Fatal(err)
	}
	binds := map[string]StageBind{}
	for _, id := range raw.PackageLeaves {
		binds[id] = StageBind{}
	}
	return binds
}
