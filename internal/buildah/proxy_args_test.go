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

func TestPredefinedProxyEnvironmentHonorsDeclarationAndImageENV(t *testing.T) {
	for _, test := range []struct {
		name string
		plan *planner.Plan
		args map[string]string
		want []string
	}{
		{name: "undeclared", plan: proxyTestPlan(t, map[string]string{"HTTP_PROXY": "http://undeclared", "NO_PROXY": "example.test"}, false), args: map[string]string{"HTTP_PROXY": "http://undeclared", "NO_PROXY": "example.test", "NOT_PROXY": "must-not-leak"}, want: []string{"HTTP_PROXY=http://undeclared", "NO_PROXY=example.test"}},
		{name: "declared", plan: proxyTestPlan(t, map[string]string{"HTTP_PROXY": "http://explicit"}, true), args: map[string]string{"HTTP_PROXY": "http://executor"}, want: []string{"HTTP_PROXY=http://explicit"}},
		{name: "image ENV", plan: testPlan(t, `from "scratch" { env HTTP_PROXY="http://image-env"; run "env"; }`), args: map[string]string{"HTTP_PROXY": "http://cli"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			operation := test.plan.Stages[0].Operations[len(test.plan.Stages[0].Operations)-1]
			lowered, _, err := lowerOperationWithCacheMountIDs(operation, []string{"/bin/sh", "-c"}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			run := lowered[0].(Run)
			environment := mergePredefinedProxyEnvironment(run.Env, test.args, operation.DeclaredProxyArguments, operation.ShadowedProxyArguments)
			if len(environment) != len(test.want) || len(test.want) > 0 && !reflect.DeepEqual(environment, test.want) {
				t.Fatalf("RUN environment=%#v, want %#v", environment, test.want)
			}
		})
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
