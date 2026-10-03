package buildah

import (
	"strings"
	"testing"

	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestGraphCacheMountIDsAreStableAndScoped(t *testing.T) {
	first := testPlan(t, "from \"scratch\"\nrun \"true\" { mount \"cache\" target=\"/cache\" }\n")
	again := testPlan(t, "from \"scratch\"\nrun \"true\" { mount \"cache\" target=\"/cache\" }\n")
	changed := testPlan(t, "from \"scratch\"\nrun \"false\" { mount \"cache\" target=\"/cache\" }\n")
	id := func(planIndex int, otherTarget bool) string {
		plans := []*planner.Plan{first, again, changed}
		plan := plans[planIndex]
		resolve, err := graphCacheMountIDResolver(plan, plan.Stages[0])
		if err != nil {
			t.Fatal(err)
		}
		operation := plan.Stages[0].Operations[0]
		if otherTarget {
			operation.Children[0].Properties["target"] = "/another"
		}
		value, err := resolve(operation, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(value, "coopr-") {
			t.Fatalf("generated cache ID = %q", value)
		}
		return value
	}
	if id(0, false) != id(1, false) {
		t.Fatal("equivalent plans generated different cache IDs")
	}
	if id(0, false) == id(2, false) {
		t.Fatal("changed plan reused a cache ID")
	}
	if id(0, false) == id(0, true) {
		t.Fatal("different mount targets reused a cache ID")
	}
}

func TestComponentCacheMountIDsBindSelectedArtifactAndParameters(t *testing.T) {
	plan := testPlan(t, "from \"scratch\"\nrun \"true\" { mount \"cache\" target=\"/cache\" }\n")
	operation := plan.Stages[0].Operations[0]
	scope := CacheMountScope{
		Kind: "component", Source: digest.FromString("artifact-with-package-a"),
		Platform:   v1.Platform{OS: "linux", Architecture: "amd64"},
		Parameters: map[string]string{"flavor": "a"}, Stage: "0",
	}
	resolve := func(scope CacheMountScope) string {
		t.Helper()
		resolver, err := scopedCacheMountIDResolver(scope)
		if err != nil {
			t.Fatal(err)
		}
		id, err := resolver(operation, 0)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := resolve(scope)
	if first != resolve(scope) {
		t.Fatal("equivalent component artifact generated a different cache ID")
	}
	scope.Source = digest.FromString("artifact-with-package-b")
	if first == resolve(scope) {
		t.Fatal("different component artifacts reused a cache ID")
	}
	scope.Source = digest.FromString("artifact-with-package-a")
	scope.Parameters["flavor"] = "b"
	if first == resolve(scope) {
		t.Fatal("different invocation parameters reused a cache ID")
	}
}
