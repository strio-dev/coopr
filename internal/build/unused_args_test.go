package build

import (
	"bytes"
	"coopr/internal/definition"
	"strings"
	"testing"
)

func TestUnusedBuildArgumentsWarnNamesOnlyAndRespectDeclarations(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`arg "base" "scratch"
from "$base" as="first"
arg "first"
from "scratch"
layer { arg "nested"; }
onbuild { arg "future"; }
`))
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]string{"base": "secret", "first": "secret", "nested": "secret", "future": "secret", "unknown": "secret", "HTTP_PROXY": "secret", "TARGETPLATFORM": "secret", "BUILDPLATFORM": "secret", "SOURCE_DATE_EPOCH": "secret"}
	var out bytes.Buffer
	warnUnusedBuildArguments(&out, def, args)
	if want := "[Warning] one or more build args were not consumed: [future unknown]\n"; out.String() != want {
		t.Fatalf("warning=%q, want %q", out.String(), want)
	}
}
