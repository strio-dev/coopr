package planner

import (
	"coopr/internal/definition"
	"strings"
	"testing"
)

func TestFromAfterDefersGeneratedSource(t *testing.T) {
	def, err := definition.Parse(strings.NewReader("from \"scratch\" as=\"producer\"\nfrom \"oci:out\" after=\"producer\"\nenv result=\"yes\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	plan, err := CreateDemandDriven(def, Options{}, func(FromSource) (StageBind, error) { calls++; return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("generated source resolved before producer: %d calls", calls)
	}
	stage := plan.Stages[len(plan.Stages)-1]
	if !stage.DeferredImageSource || stage.AfterStage != "0" || len(stage.Dependencies) != 1 || stage.Dependencies[0] != "0" {
		t.Fatalf("missing producer dependency: %+v", stage)
	}
	next, err := plan.ReplanDynamicStage(stage.ID, StageBind{})
	if err != nil {
		t.Fatal(err)
	}
	if next.Stages[len(next.Stages)-1].DeferredImageSource {
		t.Fatal("bound image still deferred")
	}
}

func TestFromAfterRejectsForwardAndMissingStages(t *testing.T) {
	for _, after := range []string{"missing", "1"} {
		def, err := definition.Parse(strings.NewReader("from \"scratch\" after=\"" + after + "\"\nfrom \"scratch\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Create(def, Options{}); err == nil {
			t.Fatalf("accepted dependency %s", after)
		}
	}
}

func TestPublishedAfterPreservesNumericDependency(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`package as="unused"
from "scratch" as="producer"
extend as="base"
from "oci:out" as="generated" after="1"
from "base" as="result"
copy "/file" "/file" from="generated"
`))
	if err != nil {
		t.Fatal(err)
	}
	publication, err := Create(def, Options{Mode: Publish})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Instantiate(publication.Component, Options{})
	if err != nil {
		t.Fatal(err)
	}
	producerID := ""
	for _, stage := range plan.Stages {
		if stage.Name == "producer" {
			producerID = stage.ID
		}
	}
	found := false
	for _, stage := range plan.Stages {
		if stage.Name == "generated" {
			found = true
			if stage.AfterStage != producerID {
				t.Fatalf("after binding changed: %+v", stage)
			}
		}
	}
	if !found {
		t.Fatal("generated stage not retained")
	}
}

func TestFilesystemFromDefersWithoutExplicitAfter(t *testing.T) {
	def, err := definition.Parse(strings.NewReader("from \"scratch\" as=\"producer\"\nfrom \"oci:out\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	plan, err := CreateDemandDriven(def, Options{BuildUnusedStages: true}, func(FromSource) (StageBind, error) { calls++; return StageBind{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || len(plan.Stages) != 2 || !plan.Stages[1].DeferredImageSource {
		t.Fatalf("filesystem source selected before execution: %d,%+v", calls, plan.Stages)
	}
}
