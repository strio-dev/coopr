package imageconfig

import (
	"coopr/internal/definition"
	"encoding/json"
	"testing"
)

func TestImportedExposeKeepsQuotedNativeToken(t *testing.T) {
	inst := definition.Instruction{Name: "expose", Arguments: []string{"80 443"}}
	data, err := json.Marshal(inst)
	if err != nil {
		t.Fatal(err)
	}
	var restored definition.Instruction
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	config := New()
	if err := config.Apply(restored); err != nil {
		t.Fatal(err)
	}
	var ports map[string]struct{}
	nested, err := config.nested()
	if err != nil {
		t.Fatal(err)
	}
	if err := decode(nested, "ExposedPorts", &ports); err != nil {
		t.Fatal(err)
	}
	if _, ok := ports["80 443/tcp"]; !ok || len(ports) != 1 {
		t.Fatalf("ports=%#v", ports)
	}
	// Cached logical configurations restore canonical native keys directly.
	cached, err := config.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	restoredConfig, err := Parse(cached)
	if err != nil {
		t.Fatal(err)
	}
	cachedAgain, err := restoredConfig.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(cachedAgain) != string(cached) {
		t.Fatalf("cached configuration changed: %s / %s", cached, cachedAgain)
	}

}
