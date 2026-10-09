package definition

import (
	"strings"
	"testing"
)

func TestSchemaErrorsIncludeSourceLocations(t *testing.T) {
	for _, tc := range []struct{ name, source, location, message string }{
		{"instruction", "from \"scratch\"\n  RUN \"true\"\n", "2:3", `use "run"`},
		{"nested", "layer {\n  copy \"a\" \"/a\" parnets=#true\n}\n", "2:3", `unsupported property "parnets"`},
		{"crlf", "from \"scratch\"\r\n  run \"true\" netwrok=\"none\"\r\n", "2:3", `unsupported property "netwrok"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.source))
			if err == nil || !strings.Contains(err.Error(), tc.location) || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error = %v, want source location %s and %s", err, tc.location, tc.message)
			}
		})
	}
}
