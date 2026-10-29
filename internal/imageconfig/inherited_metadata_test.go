package imageconfig

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/strio-dev/coopr/internal/definition"
)

func TestInheritedRuntimeMetadataPreservesNativeShape(t *testing.T) {
	for _, metadata := range []string{
		`"Healthcheck":{}`,
		`"Healthcheck":{"Test":["NONE"],"Interval":1000000000}`,
		`"Healthcheck":{"Test":["OTHER","true"]}`,
		`"Healthcheck":{"Test":["CMD-SHELL","one","two"]}`,
		`"Healthcheck":{"Test":["CMD","true"],"Interval":-1000000000}`,
		`"Healthcheck":{"Test":["CMD","true"],"Retries":-1}`,
		`"Shell":[]`,
	} {
		t.Run(metadata, func(t *testing.T) {
			raw := []byte(`{"vendor":{"keep":true},"config":{"vendorConfig":{"keep":true},` + metadata + `}}`)
			parsed, err := Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := parsed.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			var want, got map[string]any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("inherited metadata changed: %s", encoded)
			}
			restored, err := Restore(encoded, true)
			if err != nil {
				t.Fatal(err)
			}
			again, err := restored.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(encoded, again) || !restored.CommandSet() {
				t.Fatal("cache restore lost raw inherited metadata or authorship")
			}
		})
	}
}

func TestAuthoredRuntimeMetadataStillValidatesInstructions(t *testing.T) {
	for _, inst := range []definition.Instruction{
		{Name: "healthcheck", Arguments: []string{"OTHER", "true"}},
		{Name: "healthcheck", Arguments: []string{"CMD-SHELL", "one", "two"}},
		{Name: "healthcheck", Arguments: []string{"NONE"}, Properties: map[string]string{"interval": "1s"}},
		{Name: "healthcheck", Arguments: []string{"CMD", "true"}, Properties: map[string]string{"interval": "-1s"}},
		{Name: "healthcheck", Arguments: []string{"CMD", "true"}, Properties: map[string]string{"retries": "-1"}},
		{Name: "shell", Arguments: []string{}},
	} {
		if err := New().Apply(inst); err == nil {
			t.Errorf("invalid authored instruction admitted: %+v", inst)
		}
	}
}

func TestInheritedShellUsesNativeStringDecoderAndCanonicalVectors(t *testing.T) {
	config, err := Parse([]byte(`{"config":{"Shell":"/bin/sh","vendor":{"keep":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	shell, err := config.Shell([]string{"fallback"})
	if err != nil || !reflect.DeepEqual(shell, []string{"/bin/sh"}) {
		t.Fatalf("native inherited shell=%v: %v", shell, err)
	}
	raw, err := config.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var normalized struct {
		Config struct {
			Shell  []string
			Vendor map[string]bool
		}
	}
	if err := json.Unmarshal(raw, &normalized); err != nil || !reflect.DeepEqual(normalized.Config.Shell, []string{"/bin/sh"}) || !normalized.Config.Vendor["keep"] {
		t.Fatalf("canonical shell/extension changed: %s: %v", raw, err)
	}
	restored, err := Restore(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	again, err := restored.MarshalJSON()
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatalf("cache vector restore changed: %s: %v", again, err)
	}
	empty, err := Parse([]byte(`{"config":{"Shell":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	fallback := []string{"/bin/sh", "-c"}
	got, err := empty.Shell(fallback)
	if err != nil || !reflect.DeepEqual(got, fallback) {
		t.Fatalf("empty native shell fallback=%v: %v", got, err)
	}
	got[0] = "changed"
	if fallback[0] != "/bin/sh" {
		t.Fatal("fallback aliases returned shell")
	}
}
