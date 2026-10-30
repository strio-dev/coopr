package buildah

import (
	"reflect"
	"runtime"
	"testing"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/imageconfig"
	"github.com/strio-dev/coopr/internal/planner"
)

func TestRequestFromPlanInjectsOnlyUndeclaredPredefinedProxyArgs(t *testing.T) {
	plan := proxyTestPlan(t, map[string]string{
		"HTTP_PROXY": "http://undeclared",
		"NO_PROXY":   "example.test",
	}, false)
	request, err := RequestFromPlan(plan, PlanOptions{ProxyArgs: map[string]string{
		"HTTP_PROXY": "http://undeclared",
		"NO_PROXY":   "example.test",
		"NOT_PROXY":  "must-not-leak",
	}})
	if err != nil {
		t.Fatal(err)
	}
	run := request.Operations[0].(Run)
	want := []string{"HTTP_PROXY=http://undeclared", "NO_PROXY=example.test"}
	if !reflect.DeepEqual(run.Env, want) {
		t.Fatalf("RUN environment = %#v, want %#v", run.Env, want)
	}

	explicit := proxyTestPlan(t, map[string]string{"HTTP_PROXY": "http://explicit"}, true)
	request, err = RequestFromPlan(explicit, PlanOptions{ProxyArgs: map[string]string{"HTTP_PROXY": "http://executor"}})
	if err != nil {
		t.Fatal(err)
	}
	run = request.Operations[0].(Run)
	if !reflect.DeepEqual(run.Env, []string{"HTTP_PROXY=http://explicit"}) {
		t.Fatalf("explicit proxy ARG environment = %#v", run.Env)
	}
}

func TestRequestFromPlanDoesNotOverrideProxyENV(t *testing.T) {
	plan := testPlan(t, `from "scratch" {
    env HTTP_PROXY="http://image-env"
    run "env"
}
`)
	request, err := RequestFromPlan(plan, PlanOptions{ProxyArgs: map[string]string{"HTTP_PROXY": "http://cli"}})
	if err != nil {
		t.Fatal(err)
	}
	run := request.Operations[1].(Run)
	if len(run.Env) != 0 {
		t.Fatalf("RUN proxy environment overrides image ENV: %#v", run.Env)
	}
}

func TestProxyARGDeclarationControlsInstructionCacheKey(t *testing.T) {
	for _, test := range []struct {
		name     string
		declared bool
	}{
		{name: "undeclared"},
		{name: "declared", declared: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			key := func(value string) digest.Digest {
				plan := proxyTestPlan(t, map[string]string{"HTTP_PROXY": value}, test.declared)
				cacheKey, cacheable, err := instructionCacheKey(instructionCacheInput{
					ParentRootFS: "parent", Logical: imageconfig.New(), Operation: plan.Stages[0].Operations[0],
					Platform:  v1.Platform{OS: "linux", Architecture: runtime.GOARCH},
					Isolation: "rootless", Runtime: digest.FromString("runtime").String(), Format: "oci",
				})
				if err != nil || !cacheable {
					t.Fatalf("cache key for %q = %s, %v, %v", value, cacheKey, cacheable, err)
				}
				return cacheKey
			}
			one, two := key("http://one"), key("http://two")
			if same := one == two; same != !test.declared {
				t.Fatalf("proxy ARG declared=%v: keys %s and %s, want equal=%v", test.declared, one, two, !test.declared)
			}
		})
	}
}

func proxyTestPlan(t *testing.T, arguments map[string]string, declare bool) *planner.Plan {
	t.Helper()
	instructions := []definition.Instruction{}
	if declare {
		instructions = append(instructions, definition.Instruction{Name: "arg", Arguments: []string{"HTTP_PROXY"}})
	}
	instructions = append(instructions, definition.Instruction{Name: "run", Form: "shell", Arguments: []string{"env"}})
	plan, err := planner.Create(&definition.Definition{Instructions: []definition.Instruction{{Name: "from", Arguments: []string{"scratch"}, Children: instructions}}}, planner.Options{
		Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH, Arguments: arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
