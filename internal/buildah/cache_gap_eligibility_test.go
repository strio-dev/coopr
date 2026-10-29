package buildah

import (
	"testing"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/planner"
)

func TestCacheGapKnownExecutionInputsCanReuseAggregateResults(t *testing.T) {
	for _, properties := range []map[string]string{
		{"network": "host"}, {"network": "private"}, {"network": "none", "security": "insecure"},
	} {
		t.Run(properties["network"]+"/"+properties["security"], func(t *testing.T) {
			stage := planner.Stage{ID: "output", Kind: "extend", Operations: []planner.Operation{{Instruction: definition.Instruction{Name: "run", Arguments: []string{"true"}, Properties: properties}}}}
			resolved := &ResolvedComponentPlan{Plan: &planner.Plan{Stages: []planner.Stage{stage}, Outputs: []string{"output"}}}
			if !componentCacheEligible(resolved) {
				t.Fatal("keyed execution settings prevent component result reuse")
			}
			if _, ok := packageCacheEligibility([]planner.Stage{stage}); !ok {
				t.Fatal("keyed execution settings prevent package result reuse")
			}
		})
	}
}

func TestCacheGapSelectedNamedContextCanReuseComponentResult(t *testing.T) {
	key := graphNamedContextKey("source", "linux/amd64")
	stage := planner.Stage{ID: "output", Kind: "from", Source: "source", SourceContext: "source", Platform: "linux/amd64"}
	resolved := &ResolvedComponentPlan{Plan: &planner.Plan{Stages: []planner.Stage{stage}, Outputs: []string{"output"}}, SelectedBases: map[ResolvedBaseKey]ResolvedImageSource{
		key: {Selected: v1.Descriptor{Digest: digest.FromString("immutable-context")}},
	}}
	if !componentCacheEligible(resolved) {
		t.Fatal("selected immutable named context prevents component result reuse")
	}
	delete(resolved.SelectedBases, key)
	if componentCacheEligible(resolved) {
		t.Fatal("unselected named context permits component result reuse")
	}
}
