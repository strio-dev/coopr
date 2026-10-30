package definition

import (
	"strings"
	"testing"
)

func TestRunRejectsSecurityProperty(t *testing.T) {
	for _, value := range []string{"insecure", "sandbox", "${mode}"} {
		t.Run(value, func(t *testing.T) {
			inst := Instruction{Name: "run", Arguments: []string{"true"}, Properties: map[string]string{"security": value}}
			for name, validate := range map[string]func() error{
				"authored": func() error {
					_, err := parseFragment(strings.NewReader(`run "true" security="` + value + `"`))
					return err
				},
				"authored onbuild": func() error {
					_, err := parseFragment(strings.NewReader(`onbuild { run "true" security="` + value + `" }`))
					return err
				},
				"published": func() error { return ValidateOperation(inst) },
				"resolved":  func() error { return ValidateResolved(inst) },
				"onbuild": func() error {
					_, err := formatOnBuild(inst)
					return err
				},
			} {
				t.Run(name, func(t *testing.T) {
					if err := validate(); err == nil || !strings.Contains(err.Error(), "security") {
						t.Fatalf("security property error = %v, want explicit rejection", err)
					}
				})
			}
		})
	}
}
