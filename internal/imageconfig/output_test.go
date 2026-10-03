package imageconfig

import (
	"encoding/json"
	"testing"

	"coopr/internal/definition"
)

func TestOutputControlsFollowBuildahStageOrdering(t *testing.T) {
	config, err := Parse([]byte(`{"config":{"Env":["BASE=1","KEEP=base"],"Labels":{"base":"1","same":"base"}},"history":[{"created_by":"base"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	controls := OutputControls{
		Env: []string{"BASE=flag", "FLAG=1"}, DropInheritedLabels: true,
		Labels: []string{"cli=value"}, UnsetEnv: []string{"KEEP"}, UnsetLabels: []string{"remove"}, OmitHistory: true,
	}
	if err := config.PrepareStage(controls); err != nil {
		t.Fatal(err)
	}
	if err := config.Apply(definition.Instruction{Name: "env", Properties: map[string]string{"BASE": "definition"}}); err != nil {
		t.Fatal(err)
	}
	if err := config.Apply(definition.Instruction{Name: "label", Properties: map[string]string{"same": "definition", "remove": "definition"}}); err != nil {
		t.Fatal(err)
	}
	if err := config.ApplyOutput(controls); err != nil {
		t.Fatal(err)
	}
	raw, err := config.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var image struct {
		Config struct {
			Env    []string          `json:"Env"`
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
		History json.RawMessage `json:"history"`
	}
	if err := json.Unmarshal(raw, &image); err != nil {
		t.Fatal(err)
	}
	if got, want := image.Config.Env, []string{"BASE=definition", "FLAG=1"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("environment = %q, want %q", got, want)
	}
	if len(image.Config.Labels) != 2 || image.Config.Labels["same"] != "definition" || image.Config.Labels["cli"] != "value" {
		t.Fatalf("labels = %#v", image.Config.Labels)
	}
	if image.History != nil {
		t.Fatalf("history was not omitted: %s", image.History)
	}
}

func TestOutputControlsZeroValuePreservesInheritedMetadata(t *testing.T) {
	config, err := Parse([]byte(`{"config":{"Env":["BASE=1"],"Labels":{"base":"1"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.PrepareStage(OutputControls{}); err != nil {
		t.Fatal(err)
	}
	if err := config.ApplyOutput(OutputControls{}); err != nil {
		t.Fatal(err)
	}
	raw, _ := config.MarshalJSON()
	if string(raw) != `{"config":{"Env":["BASE=1"],"Labels":{"base":"1"}}}` {
		t.Fatalf("zero-value controls changed config: %s", raw)
	}
}

func TestOutputControlsApplyPlatformMetadata(t *testing.T) {
	config, err := Parse([]byte(`{"architecture":"amd64","os":"linux","os.version":"old","os.features":["keep","remove"],"config":{"Labels":{"base":"1"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	controls := OutputControls{OSVersion: "new", OSFeatures: []string{"remove-", "added", "keep"}}
	if err := config.ApplyOutput(controls); err != nil {
		t.Fatal(err)
	}
	raw, err := config.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var image struct {
		OSVersion  string   `json:"os.version"`
		OSFeatures []string `json:"os.features"`
	}
	if err := json.Unmarshal(raw, &image); err != nil {
		t.Fatal(err)
	}
	if image.OSVersion != "new" || len(image.OSFeatures) != 2 || image.OSFeatures[0] != "keep" || image.OSFeatures[1] != "added" {
		t.Fatalf("platform metadata = %#v", image)
	}
}

func TestOutputControlsValidateNewMetadata(t *testing.T) {
	for _, controls := range []OutputControls{
		{LayerLabels: []string{"missing-value"}},
		{OSFeatures: []string{"-"}},
		{OSVersion: "bad\nversion"},
	} {
		if err := controls.Validate(); err == nil {
			t.Fatalf("invalid controls accepted: %#v", controls)
		}
	}
}
