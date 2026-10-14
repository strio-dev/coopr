package definition_test

import (
	"strings"
	"testing"

	"github.com/openshift/imagebuilder"
)

func TestHealthcheckValidationMatchesNativeImagebuilder(t *testing.T) {
	for _, test := range []struct {
		kdl, native string
		valid       bool
	}{
		{`interval="1ns"`, "--interval=1ns", true},
		{`interval="0s"`, "--interval=0s", false},
		{`start-period="-1ns"`, "--start-period=-1ns", false},
		{`retries=0`, "--retries=0", false},
		{`retries=2147483647`, "--retries=2147483647", true},
		{`retries=2147483648`, "--retries=2147483648", false},
	} {
		t.Run(test.native, func(t *testing.T) {
			_, err := parseFragment(strings.NewReader(`healthcheck "true" ` + test.kdl))
			if (err == nil) != test.valid {
				t.Fatalf("Coopr validation: %v", err)
			}
			ast, err := imagebuilder.ParseDockerfile(strings.NewReader("HEALTHCHECK " + test.native + " CMD true"))
			if err != nil {
				t.Fatal(err)
			}
			builder := imagebuilder.NewBuilder(nil)
			step := builder.Step()
			if err := step.Resolve(ast.Children[0]); err != nil {
				t.Fatal(err)
			}
			if err := builder.Run(step, imagebuilder.NoopExecutor, true); (err == nil) != test.valid {
				t.Fatalf("native validation: %v", err)
			}
		})
	}
}
