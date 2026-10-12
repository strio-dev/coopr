package buildah

import (
	"context"
	"errors"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestComponentStageCallbacksCancelBeforeDispatch(t *testing.T) {
	events := make(chan componentControlEvent)
	returned, selected := componentStageCallbacks(events, "stage")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := returned(ctx, componentReturnRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("return cancellation=%v", err)
	}
	if _, err := selected(ctx, componentSelectionRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("selection cancellation=%v", err)
	}
}

func TestComponentStageCallbacksKeepGraphLocalIdentity(t *testing.T) {
	events := make(chan componentControlEvent)
	returned, _ := componentStageCallbacks(events, "local")
	result := make(chan error, 1)
	go func() {
		reply, err := returned(context.Background(), componentReturnRequest{Revision: 2})
		if reply.Revision != 3 {
			t.Errorf("revision=%d", reply.Revision)
		}
		result <- err
	}()
	event := <-events
	if event.stageID != "local" || event.returned == nil || event.returned.Revision != 2 {
		t.Fatalf("event=%#v", event)
	}
	event.reply <- componentControlResult{returned: componentReturnReply{Revision: 3}}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestComponentContinuationProtectsStageIdentity(t *testing.T) {
	stage := planner.Stage{ID: "1", Kind: "from", Source: "scratch", Platform: "linux/amd64"}
	next := stage
	next.Dependencies = []string{"0"}
	next.AwaitingComponent = &planner.ComponentBoundary{StageID: "1", Instruction: 2}
	if err := validateComponentStageIdentity(stage, next); err != nil {
		t.Fatal(err)
	}
	next.Source = "replacement"
	if err := validateComponentStageIdentity(stage, next); err == nil {
		t.Fatal("return changed live builder base")
	}
	if componentDependenciesComplete(planner.Stage{Dependencies: []string{"0"}}, map[string]bool{}) {
		t.Fatal("unfinished dependency released suspended stage")
	}
	if !componentDependenciesComplete(planner.Stage{Dependencies: []string{"0"}}, map[string]bool{"0": true}) {
		t.Fatal("completed dependency did not release suspended stage")
	}
}

func TestComponentPausedInProcessStageReleasesCapacity(t *testing.T) {
	running := map[string]bool{"caller": true}
	inProcess := map[string]bool{"caller": true}
	paused := map[string]componentControlEvent{"caller": {}}
	if componentInProcessActive(running, paused, inProcess) {
		t.Fatal("paused stage still reserves native executor")
	}
	delete(paused, "caller")
	if !componentInProcessActive(running, paused, inProcess) {
		t.Fatal("active in-process stage may run concurrent executor work")
	}
	delete(running, "caller")
	if componentInProcessActive(running, paused, inProcess) {
		t.Fatal("completed stage still reserves native executor")
	}
}

func TestComponentContinuationReusesSyntheticLabelPreparation(t *testing.T) {
	stage := planner.Stage{ID: "1", Name: "child", Source: "parent", Operations: []planner.Operation{{Instruction: definition.Instruction{Name: "env", Properties: map[string]string{"proof": "value"}}}}}
	aliases := map[string]string{"parent": "0"}
	images := map[string]stageState{"0": {storageImageID: "native-parent"}}
	initial := graphStageLabelOperations(stage.Operations, stage, true, aliases, images)
	options := PlanOptions{Lifecycle: LifecycleControls{StageLabels: true}}
	continued, err := componentContinuationOperations(planner.Build, stage, options, aliases, images)
	if err != nil {
		t.Fatal(err)
	}
	if len(continued) != len(stage.Operations)+1 || continued[0].Properties["io.buildah.stage.base"] != "native-parent" || continued[0].Properties["io.buildah.stage.name"] != "child" {
		t.Fatalf("synthetic label=%v", continued)
	}
	if err := validateComponentReturnReply(componentReturnRequest{Executed: initial}, componentReturnReply{Operations: continued, Revision: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestComponentBaseCallbackCancellationAndGraphIdentity(t *testing.T) {
	events := make(chan componentControlEvent)
	callback := componentStageBaseCallback(events, "nested")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := callback(ctx, baseSelectionRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	done := make(chan error, 1)
	go func() {
		source, err := callback(context.Background(), baseSelectionRequest{Reference: "mutable"})
		if source.Reference != "immutable" {
			t.Errorf("source=%v", source)
		}
		done <- err
	}()
	event := <-events
	if event.stageID != "nested" || event.base == nil || event.base.Reference != "mutable" {
		t.Fatalf("event=%#v", event)
	}
	event.reply <- componentControlResult{base: ResolvedImageSource{Reference: "immutable"}}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestComponentSelectionAncestryIsDetachedAndRestored(t *testing.T) {
	rootDigest, ancestorDigest := digest.FromString("root"), digest.FromString("ancestor")
	executor := &graphExecutor{activeComponents: map[digest.Digest]bool{rootDigest: true}, activeLocalComponents: map[string]bool{"root#target": true}}
	executor.resolveComponent = func(context.Context, ComponentPlanRequest) (*ResolvedComponentPlan, error) {
		if !executor.activeComponents[ancestorDigest] || executor.activeComponents[rootDigest] || !executor.activeLocalComponents["caller#target"] {
			t.Fatal("selection did not use caller ancestry")
		}
		executor.activeComponents[digest.FromString("selection")] = true
		executor.activeLocalComponents["selection#target"] = true
		return nil, errors.New("selection failed")
	}
	request := componentSelectionRequest{Reference: "registry.invalid/component:tag", Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, ActiveComponents: map[digest.Digest]bool{ancestorDigest: true}, ActiveLocalComponents: map[string]bool{"caller#target": true}}
	if _, err := executor.selectCoordinatedComponent(context.Background(), request); err == nil {
		t.Fatal("missing resolver error")
	}
	if len(executor.activeComponents) != 1 || !executor.activeComponents[rootDigest] || len(executor.activeLocalComponents) != 1 || !executor.activeLocalComponents["root#target"] {
		t.Fatal("selection ancestry leaked into sibling scope")
	}
	if len(request.ActiveComponents) != 1 || len(request.ActiveLocalComponents) != 1 {
		t.Fatal("selection mutated caller ancestry")
	}
}

func TestPreparedStageSnapshotsInvocationAncestryBeforeWorkerLaunch(t *testing.T) {
	ancestor := digest.FromString("ancestor")
	executor := &graphExecutor{activeComponents: map[digest.Digest]bool{ancestor: true}, activeLocalComponents: map[string]bool{"source#target": true}}
	prepared, err := executor.prepareGraphStage(context.Background(), &planner.Plan{}, planner.Stage{ID: "pkg", Kind: "package-input"}, nil, nil, &graphBindings{packages: map[string]stageState{"pkg": {storageImageID: "bound", config: imageconfig.New()}}})
	if err != nil {
		t.Fatal(err)
	}
	delete(executor.activeComponents, ancestor)
	delete(executor.activeLocalComponents, "source#target")
	if !prepared.activeComponents[ancestor] || !prepared.activeLocalComponents["source#target"] {
		t.Fatal("worker ancestry changed after stage preparation")
	}
	prepared.activeLocalComponents["child#target"] = true
	if executor.activeLocalComponents["child#target"] {
		t.Fatal("worker ancestry shares coordinator map")
	}
}
