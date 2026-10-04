package buildah

import (
	"reflect"
	"runtime"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
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
	plan := testPlan(t, "from \"scratch\"\nenv HTTP_PROXY=\"http://image-env\"\nrun \"env\"\n")
	request, err := RequestFromPlan(plan, PlanOptions{ProxyArgs: map[string]string{"HTTP_PROXY": "http://cli"}})
	if err != nil {
		t.Fatal(err)
	}
	run := request.Operations[1].(Run)
	if len(run.Env) != 0 {
		t.Fatalf("RUN proxy environment overrides image ENV: %#v", run.Env)
	}
}

func TestUndeclaredProxyValueDoesNotChangeInstructionCacheKey(t *testing.T) {
	plan := proxyTestPlan(t, map[string]string{"HTTP_PROXY": "http://one"}, false)
	operation := plan.Stages[0].Operations[0]
	config := imageconfig.New()
	input := instructionCacheInput{
		ParentRootFS: "parent", Logical: config, Operation: operation,
		Platform:  v1.Platform{OS: "linux", Architecture: runtime.GOARCH},
		Isolation: "rootless", Runtime: digest.FromString("runtime").String(), Format: "oci",
	}
	first, cacheable, err := instructionCacheKey(input)
	if err != nil || !cacheable {
		t.Fatalf("first cache key = %s, %v, %v", first, cacheable, err)
	}
	second, cacheable, err := instructionCacheKey(input)
	if err != nil || !cacheable || first != second {
		t.Fatalf("proxy changed cache key: first=%s second=%s cacheable=%v err=%v", first, second, cacheable, err)
	}
}

func TestExplicitProxyARGValueChangesInstructionCacheKey(t *testing.T) {
	key := func(value string) digest.Digest {
		plan := proxyTestPlan(t, map[string]string{"HTTP_PROXY": value}, true)
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
	if one, two := key("http://one"), key("http://two"); one == two {
		t.Fatalf("explicit proxy ARG values produced the same cache key %s", one)
	}
}

func proxyTestPlan(t *testing.T, arguments map[string]string, declare bool) *planner.Plan {
	t.Helper()
	instructions := []definition.Instruction{{Name: "from", Arguments: []string{"scratch"}}}
	if declare {
		instructions = append(instructions, definition.Instruction{Name: "arg", Arguments: []string{"HTTP_PROXY"}})
	}
	instructions = append(instructions, definition.Instruction{Name: "run", Form: "shell", Arguments: []string{"env"}})
	plan, err := planner.Create(&definition.Definition{Instructions: instructions}, planner.Options{
		Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH, Arguments: arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
