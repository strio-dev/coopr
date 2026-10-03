package definition

import (
	"strings"
	"testing"
)

func TestLocalComponentReferences(t *testing.T) {
	for _, reference := range []string{
		"./shared.coopr", "../shared.coopr",
		"/components/shared.coopr", "./shared", "./shared:variant", "./shared@variant", "./*.coopr",
		"./registry.example.com/team/shared.coopr",
	} {
		t.Run(reference, func(t *testing.T) {
			if !IsLocalComponentReference(reference) {
				t.Fatal("local component path classified as OCI")
			}
			def, err := Parse(strings.NewReader(`component "` + reference + `" channel="preview"`))
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateResolved(def.Instructions[0]); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOCIComponentReferencesRemainOCI(t *testing.T) {
	for _, reference := range []string{
		"shared.coopr", "components/shared.coopr", "team/shared.coopr", "shared.coopr:stable",
		"registry.example.com/team/shared:v1", "registry.example.com/team/shared:variant.coopr",
		"registry.example.com/team/shared.coopr", "localhost/team/shared.coopr", "localhost:5000/team/shared.coopr",
		"registry.example.com/team/shared@sha256:deadbeef", "local:shared.coopr", "sha256:deadbeef",
		"${component}", "", "shared",
	} {
		if IsLocalComponentReference(reference) {
			t.Errorf("OCI or unresolved reference %q classified as a context path", reference)
		}
	}
}

func TestComponentReferencesAcceptShortRegistryNames(t *testing.T) {
	for _, reference := range []string{"shared", "shared:stable", "shared.coopr", "shared.coopr:stable", "team/shared.coopr"} {
		t.Run(reference, func(t *testing.T) {
			def, err := Parse(strings.NewReader(`component "` + reference + `"`))
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateResolved(def.Instructions[0]); err != nil {
				t.Fatal(err)
			}
		})
	}
}
