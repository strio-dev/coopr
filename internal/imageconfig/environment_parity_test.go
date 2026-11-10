package imageconfig

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/strio-dev/coopr/internal/definition"
)

func TestInstructionAndStageEnvironmentPreserveSameOrdering(t *testing.T) {
	for _, tc := range []struct {
		name       string
		inherited  []string
		key, value string
		want       []string
	}{
		{"replace duplicates in place", []string{"A=first", "B=keep", "A=last"}, "A", "new", []string{"A=new", "B=keep"}},
		{"retain unrelated duplicates", []string{"A=first", "A=last"}, "B", "new", []string{"A=first", "A=last", "B=new"}},
		{"empty value", []string{"A=old", "B=keep"}, "A", "", []string{"A=", "B=keep"}},
		{"embedded equals", []string{"A=old"}, "A", "one=two", []string{"A=one=two"}},
		{"new key", nil, "A", "new", []string{"A=new"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"config": map[string]any{"Env": tc.inherited, "vendor": true}})
			if err != nil {
				t.Fatal(err)
			}
			base, err := Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			fromInstruction, fromFlag := base.Clone(), base.Clone()
			if err := fromInstruction.Apply(definition.Instruction{Name: "env", Arguments: []string{tc.key, tc.value}}); err != nil {
				t.Fatal(err)
			}
			if err := fromFlag.PrepareStage(OutputControls{Env: []string{tc.key + "=" + tc.value}}); err != nil {
				t.Fatal(err)
			}
			for _, config := range []*Config{fromInstruction, fromFlag} {
				fields, err := config.nested()
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				if err := json.Unmarshal(fields["Env"], &got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("Env=%v want=%v", got, tc.want)
				}
				if string(fields["vendor"]) != "true" {
					t.Fatal("lost unrelated extension")
				}
			}
			preserved, err := base.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			if string(preserved) != string(raw) {
				t.Fatalf("mutated inherited config: %s", preserved)
			}
		})
	}
}
