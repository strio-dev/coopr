package planner

import (
	"reflect"
	"testing"
)

func TestBuildUnusedStagesRetainsEarlierStagesAndOneTarget(t *testing.T) {
	def := parse(t, `from "scratch" as="unused"
label purpose="unused"
from "scratch" as="target"
from "unavailable.example/later" as="later"
`)
	for _, includeUnused := range []bool{false, true} {
		plan, err := CreateDemandDriven(def, Options{Mode: Build, Target: "target", BuildUnusedStages: includeUnused}, func(source FromSource) (StageBind, error) {
			t.Fatalf("resolved stage beyond selected target: %+v", source)
			return StageBind{}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"1"}
		if includeUnused {
			want = []string{"0", "1"}
		}
		if !reflect.DeepEqual(stageIDs(plan), want) {
			t.Fatalf("includeUnused=%v stages=%v want=%v", includeUnused, stageIDs(plan), want)
		}
		if !reflect.DeepEqual(plan.Outputs, []string{"1"}) {
			t.Fatalf("outputs=%v", plan.Outputs)
		}
	}
}

func TestBuildUnusedStagesResolvesPreviouslyDormantInputs(t *testing.T) {
	def := parse(t, `from "unused.example/base" as="unused"
from "scratch" as="target"
`)
	var calls []string
	plan, err := CreateDemandDriven(def, Options{Mode: Build, BuildUnusedStages: true}, func(source FromSource) (StageBind, error) {
		calls = append(calls, source.Source)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"unused.example/base"}) || len(plan.Stages) != 2 {
		t.Fatalf("calls=%v stages=%v", calls, stageIDs(plan))
	}
}

func TestBuildUnusedStagesForPublicationRunsOnlyEarlierPackageProducers(t *testing.T) {
	def := parse(t, `package as="unused"
copy "unused" "/unused"
package as="used"
copy "used" "/used"
extend as="target"
copy "/used" "/used" from="used"
run "invocation"
package as="later"
copy "later" "/later"
extend as="other"
`)
	for _, includeUnused := range []bool{false, true} {
		plan, err := CreateDemandDriven(def, Options{Mode: Publish, Target: "target", BuildUnusedStages: includeUnused}, func(source FromSource) (StageBind, error) {
			t.Fatalf("unexpected external source: %+v", source)
			return StageBind{}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"1"}
		if includeUnused {
			want = []string{"0", "1"}
		}
		if !reflect.DeepEqual(stageIDs(plan), want) {
			t.Fatalf("includeUnused=%v stages=%v want=%v", includeUnused, stageIDs(plan), want)
		}
		if !reflect.DeepEqual(plan.Outputs, []string{"1"}) {
			t.Fatalf("includeUnused=%v outputs=%v", includeUnused, plan.Outputs)
		}
		for _, stage := range plan.Stages {
			if stage.Kind != "package" {
				t.Fatalf("includeUnused=%v publication scheduled invocation stage: %+v", includeUnused, stage)
			}
		}
	}
}

func TestBuildUnusedStagesForPublicationResolvesPackageDependenciesOnly(t *testing.T) {
	def := parse(t, `from "unused.example/base" as="producer"
package as="unused"
copy "/payload" "/payload" from="producer"
extend as="target"
run "invocation"
`)
	var calls []string
	plan, err := CreateDemandDriven(def, Options{Mode: Publish, Target: "target", BuildUnusedStages: true}, func(source FromSource) (StageBind, error) {
		calls = append(calls, source.Source)
		return StageBind{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"unused.example/base"}) {
		t.Fatalf("resolved sources=%v", calls)
	}
	if got, want := stageIDs(plan), []string{"0", "1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stages=%v want=%v", got, want)
	}
	if len(plan.Outputs) != 0 {
		t.Fatalf("unused package became a published output: %v", plan.Outputs)
	}
	for _, stage := range plan.Stages {
		if stage.Kind == "extend" {
			t.Fatalf("publication scheduled invocation stage: %+v", stage)
		}
	}
}
