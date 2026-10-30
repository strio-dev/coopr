package planner

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/openshift/imagebuilder"
	"github.com/strio-dev/coopr/internal/imageconfig"
)

func TestInheritedBareEnvironmentMatchesNativeImagebuilder(t *testing.T) {
	entries := []string{"A=first", "BARE", "EMPTY=", "A=last=part", "RESET=value", "RESET"}
	logical, err := imageconfig.Parse([]byte(`{"config":{"Env":["A=first","BARE","EMPTY=","A=last=part","RESET=value","RESET"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	before, err := logical.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	native := imagebuilder.NewBuilder(nil)
	ast, err := imagebuilder.ParseDockerfile(strings.NewReader("FROM scratch\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := native.FromImage(&docker.Image{Config: &docker.Config{Env: entries}}, ast); err != nil {
		t.Fatal(err)
	}
	bind, err := StageBindFromImageConfig(logical)
	if err != nil {
		t.Fatalf("native inherited environment rejected: %v", err)
	}
	for _, name := range []string{"A", "BARE", "EMPTY", "RESET"} {
		want, err := imagebuilder.ProcessWord("${"+name+"}", native.Env)
		if err != nil {
			t.Fatal(err)
		}
		if got, present := bind.BaseEnvironment[name]; !present || got != want {
			t.Fatalf("inherited %s = %q, present=%t, native=%q", name, got, present, want)
		}
	}
	gotEntries, err := ImageEnvironmentEntries(logical)
	if err != nil || !reflect.DeepEqual(gotEntries, entries) {
		t.Fatalf("raw inherited environment changed: %q, %v", gotEntries, err)
	}
	after, err := logical.MarshalJSON()
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("image configuration changed: %s, %v", after, err)
	}
}

func TestImageEnvironmentEntriesPreserveImageValues(t *testing.T) {
	logical, err := imageconfig.Parse([]byte(`{"config":{"Env":["A=first","EMPTY=","A=last=part"],"OnBuild":["FROM scratch"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	before, err := logical.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ImageEnvironmentEntries(logical)
	if err != nil || !reflect.DeepEqual(entries, []string{"A=first", "EMPTY=", "A=last=part"}) {
		t.Fatalf("environment entries = %v, %v", entries, err)
	}
	values, err := ImageEnvironment(logical)
	if err != nil || !reflect.DeepEqual(values, map[string]string{"A": "last=part", "EMPTY": ""}) {
		t.Fatalf("environment = %v, %v", values, err)
	}
	entries[0] = "A=changed"
	values["A"] = "changed"
	after, err := logical.MarshalJSON()
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("image configuration changed: %s, %v", after, err)
	}
}

func TestStageBindFromImageConfigRejectsInvalidBase(t *testing.T) {
	if _, err := StageBindFromImageConfig(nil); err == nil || err.Error() != "ONBUILD base configuration is nil" {
		t.Fatalf("nil base: %v", err)
	}
	logical, err := imageconfig.Parse([]byte(`{"config":{"OnBuild":["FROM scratch"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StageBindFromImageConfig(logical); err == nil || !strings.Contains(err.Error(), "ONBUILD ingestion") {
		t.Fatalf("invalid inherited trigger: %v", err)
	}
}
