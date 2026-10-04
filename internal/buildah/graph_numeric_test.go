package buildah

import (
	"context"
	"path/filepath"
	"testing"
)

func TestNumericStageIDsReachValidationLoweringAndPackageClosure(t *testing.T) {
	plan := testPlan(t, `from "scratch"
from "0"
copy "/src" "/dst" from="0"
add "/src" "/added" from="0"
run "true" {
 mount "bind" from="0" source="/src" target="/bind"
 mount "cache" from="0" source="/src" target="/cache"
}
`)
	stages, _, err := validateStandaloneGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	aliases := stageAliases(stages)
	if aliases[1]["0"] != "0" || aliases[0]["0"] != "" {
		t.Fatalf("numeric aliases violate prior-stage scope: %v", aliases)
	}
	images := map[string]stageState{"0": {storageImageID: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}
	resolve, err := graphCacheMountIDResolver(plan, stages[1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lowerGraphOperations(stages[1].Operations, aliases[1], images, nil, []string{"/bin/sh", "-c"}, resolve); err != nil {
		t.Fatal(err)
	}
	publication := testPublicationPlan(t, `from "scratch"
from "0"
package as="payload"
copy "/src" "/dst" from="1"
extend
copy "/dst" "/dst" from="payload"
`)
	stages, _, err = validatePublicationGraph(publication, map[string]string{"payload": filepath.Join(t.TempDir(), "payload.tar")})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := selectPublicationBases(context.Background(), PlanOptions{}, stages)
	if err != nil {
		t.Fatalf("local numeric FROM attempted external resolution: %v", err)
	}
	bases, err := resolvePackageClosureBases(stages, selected)
	if err != nil || len(bases) != 0 {
		t.Fatalf("local numeric closure attempted external resolution: %v %v", bases, err)
	}
}
