package planner

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/imageconfig"
)

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
