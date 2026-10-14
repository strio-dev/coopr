package imageconfig

import (
	"encoding/json"
	"testing"
)

func TestOnBuildInlineNULConfiguration(t *testing.T) {
	trigger := "COPY <<name /out/\nnul\x00exact\rafter\nname"
	raw, err := json.Marshal(map[string]any{"config": map[string]any{"OnBuild": []string{trigger}}})
	if err != nil {
		t.Fatal(err)
	}
	config, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	got, err := config.OnBuild()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != trigger {
		t.Fatalf("OnBuild = %#v", got)
	}
}
