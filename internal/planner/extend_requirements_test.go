package planner

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"coopr/internal/definition"
)

func TestInstantiateRetainsResolvedRequirementsForEveryExtendedRoot(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`
arg "first_distro" "alpine"
extend as="first" distro="${first_distro}" distro-version="3.20" package-manager="apk" architecture="amd64"
extend as="second" distro="fedora" package-manager="/usr/bin/dnf" architecture="amd64"
copy "/artifact" "/artifact" from="first"
`))
	if err != nil {
		t.Fatal(err)
	}
	publication, err := Create(def, Options{Mode: Publish, Platform: "linux/amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if publication.Component == nil {
		t.Fatal("publication did not freeze a component")
	}
	invocation, err := Instantiate(publication.Component, Options{Mode: Invoke, Arguments: map[string]string{"first_distro": "debian"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(invocation.Stages) != 2 || invocation.Stages[0].Kind != "extend" || invocation.Stages[1].Kind != "extend" {
		t.Fatalf("unexpected invocation stages: %+v", invocation.Stages)
	}
	want := []map[string][]string{
		{"distro": {"debian"}, "distro-version": {"3.20"}, "package-manager": {"apk"}, "architecture": {"amd64"}},
		{"distro": {"fedora"}, "package-manager": {"/usr/bin/dnf"}, "architecture": {"amd64"}},
	}
	for index, stage := range invocation.Stages {
		if !reflect.DeepEqual(stage.Requirements, want[index]) {
			t.Fatalf("stage %s requirements = %v, want %v", stage.ID, stage.Requirements, want[index])
		}
	}
	encoded, err := json.Marshal(invocation)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Plan
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for index := range want {
		if !reflect.DeepEqual(decoded.Stages[index].Requirements, want[index]) {
			t.Fatalf("round-trip stage %d requirements = %v, want %v", index, decoded.Stages[index].Requirements, want[index])
		}
	}
}

func TestExtendRequirementListsExpandNormalizeAndDeduplicate(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`
arg "primary" "fedora"
extend {
  distro "rhel" "${primary}" "fedora"
  distro-version "42" "41" "42"
  package-manager "yum" "dnf" "dnf"
  architecture "x86_64" "amd64" "arm64"
}
`))
	if err != nil {
		t.Fatal(err)
	}
	publication, err := Create(def, Options{Mode: Publish, Platform: "linux/amd64"})
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := Instantiate(publication.Component, Options{Mode: Invoke, Arguments: map[string]string{"primary": "centos"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"distro":          {"centos", "fedora", "rhel"},
		"distro-version":  {"41", "42"},
		"package-manager": {"dnf", "yum"},
		"architecture":    {"amd64", "arm64"},
	}
	if got := invocation.Stages[0].Requirements; !reflect.DeepEqual(got, want) {
		t.Fatalf("requirements = %#v, want %#v", got, want)
	}
}

func TestPublishedRequirementListsRoundTripAuthoredExpressions(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`
arg "primary" "fedora"
extend {
  distro "rhel" "${primary}"
  architecture "x86_64" "arm64"
}
`))
	if err != nil {
		t.Fatal(err)
	}
	publication, err := Create(def, Options{Mode: Publish, Platform: "linux/amd64"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(publication.Component)
	if err != nil {
		t.Fatal(err)
	}
	var published PublishedComponent
	if err := json.Unmarshal(encoded, &published); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidatePublished(&published); err != nil {
		t.Fatal(err)
	}
	children := published.Definition.Instructions[1].Children
	if len(children) != 2 || !reflect.DeepEqual(children[0].Arguments, []string{"rhel", "${primary}"}) {
		t.Fatalf("published requirement expressions = %#v", children)
	}
	invocation, err := Instantiate(&published, Options{Mode: Invoke, Arguments: map[string]string{"primary": "centos"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"distro": {"centos", "rhel"}, "architecture": {"amd64", "arm64"}}
	if got := invocation.Stages[0].Requirements; !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip requirements = %#v, want %#v", got, want)
	}
	for _, mutate := range []func(*PublishedComponent){
		func(component *PublishedComponent) {
			extend := &component.Definition.Instructions[1]
			extend.Children = append(extend.Children, extend.Children[0])
		},
		func(component *PublishedComponent) {
			component.Definition.Instructions[1].Children[0].Arguments = nil
		},
	} {
		var tampered PublishedComponent
		if err := json.Unmarshal(encoded, &tampered); err != nil {
			t.Fatal(err)
		}
		mutate(&tampered)
		if _, err := ValidatePublished(&tampered); err == nil {
			t.Fatal("tampered published compatibility requirements passed validation")
		}
	}
}

func TestExtendRequirementsRejectUnresolvedOrEmptyValues(t *testing.T) {
	for _, source := range []string{
		`extend distro="${missing}"`,
		`arg "distro" ""` + "\n" + `extend distro="${distro}"`,
		`extend { distro "fedora" "${missing}" }`,
		`arg "distro" ""` + "\n" + `extend { distro "fedora" "${distro}" }`,
		`arg "arch" "linux/amd64"` + "\n" + `extend { architecture "${arch}" }`,
	} {
		def, err := definition.Parse(strings.NewReader(source))
		if err != nil {
			t.Fatal(err)
		}
		_, err = Create(def, Options{Mode: Publish, Platform: "linux/amd64"})
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte("extend")) {
			t.Fatalf("requirements error for %q = %v", source, err)
		}
	}
}
