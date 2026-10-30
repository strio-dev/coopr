package onbuildparse

import (
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/definition"
)

func TestRunRejectsSecurityFlag(t *testing.T) {
	identity := func(word string) (string, error) { return word, nil }
	for _, value := range []string{"insecure", "sandbox", "$mode"} {
		t.Run(value, func(t *testing.T) {
			raw := "RUN --security=" + value + " true"
			for name, parse := range map[string]func(string) ([]definition.Instruction, error){
				"normal":   Parse,
				"deferred": ParseDeferred,
				"expanded": func(raw string) ([]definition.Instruction, error) { return ParseExpanded(raw, identity) },
				"environment": func(raw string) ([]definition.Instruction, error) {
					return ParseExpandedEnvironment(raw, nil)
				},
			} {
				t.Run(name, func(t *testing.T) {
					if _, err := parse(raw); err == nil || !strings.Contains(err.Error(), "security") {
						t.Fatalf("RUN flag error = %v, want explicit security rejection", err)
					}
				})
			}
		})
	}
}
