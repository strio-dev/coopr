package buildah

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"

	"coopr/internal/definition"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
)

type componentControlEvent struct {
	stageID   string
	returned  *componentReturnRequest
	selection *componentSelectionRequest
	base      *baseSelectionRequest
	reply     chan componentControlResult
}

type componentControlResult struct {
	returned  componentReturnReply
	selection componentSelectionReply
	base      ResolvedImageSource
	err       error
}

func componentStageCallbacks(events chan<- componentControlEvent, stageID string) (componentReturnHandler, componentSelectionHandler) {
	exchange := func(ctx context.Context, event componentControlEvent) (componentControlResult, error) {
		event.stageID, event.reply = stageID, make(chan componentControlResult, 1)
		select {
		case events <- event:
		case <-ctx.Done():
			return componentControlResult{}, ctx.Err()
		}
		select {
		case result := <-event.reply:
			return result, result.err
		case <-ctx.Done():
			return componentControlResult{}, ctx.Err()
		}
	}
	returns := func(ctx context.Context, request componentReturnRequest) (componentReturnReply, error) {
		result, err := exchange(ctx, componentControlEvent{returned: &request})
		return result.returned, err
	}
	selections := func(ctx context.Context, request componentSelectionRequest) (componentSelectionReply, error) {
		result, err := exchange(ctx, componentControlEvent{selection: &request})
		return result.selection, err
	}
	return returns, selections
}

func componentContinuationOperations(mode planner.Mode, stage planner.Stage, options PlanOptions, aliases map[string]string, images map[string]stageState) ([]planner.Operation, error) {
	operations, err := mergeHistoryOperations(normalizeGraphOperationContexts(mode, stage.Operations), stage.History)
	if err != nil {
		return nil, err
	}
	stages, err := resolveAndValidateBuildNetwork([]planner.Stage{{ID: stage.ID, Operations: operations}}, options.Network, options.RunControls, options.Isolation)
	if err != nil {
		return nil, err
	}
	return graphStageLabelOperations(stages[0].Operations, stage, options.Lifecycle.StageLabels, aliases, images), nil
}

func componentContinuationStage(stages []planner.Stage, id string) (planner.Stage, error) {
	for _, stage := range stages {
		if stage.ID == id {
			return stage, nil
		}
	}
	return planner.Stage{}, fmt.Errorf("component return removed suspended stage %s", id)
}

func componentDependenciesComplete(stage planner.Stage, completed map[string]bool) bool {
	for _, id := range stage.Dependencies {
		if !completed[id] {
			return false
		}
	}
	return true
}

func (executor *graphExecutor) selectCoordinatedComponent(ctx context.Context, request componentSelectionRequest) (componentSelectionReply, error) {
	// Nested coordinators in an isolated worker retain their local canonical plan,
	// but delegate mutable artifact selection to the ancestor owning the build.
	if executor.componentSelectionHandler != nil {
		return executor.componentSelectionHandler(ctx, request)
	}
	previousDigests, previousSources := executor.activeComponents, executor.activeLocalComponents
	executor.activeComponents = maps.Clone(request.ActiveComponents)
	if executor.activeComponents == nil {
		executor.activeComponents = make(map[digest.Digest]bool)
	}
	executor.activeLocalComponents = maps.Clone(request.ActiveLocalComponents)
	defer func() { executor.activeComponents, executor.activeLocalComponents = previousDigests, previousSources }()
	resolved, err := executor.resolveComponentInvocation(ctx, request.Reference, request.Parameters, request.Platform, request.ProgressPrefix, request.CallerEnvironment)
	if err != nil {
		return componentSelectionReply{}, err
	}
	key, err := componentInvocationSelectionKey(request.Reference, request.Parameters, request.Platform)
	if err != nil {
		return componentSelectionReply{}, err
	}
	pinned, err := pinnedComponentReference(resolved)
	if err != nil {
		return componentSelectionReply{}, err
	}
	if executor.componentPins == nil {
		executor.componentPins = make(map[string]string)
	}
	if previous := executor.componentPins[key]; previous != "" && previous != pinned {
		return componentSelectionReply{}, errors.New("coordinator changed immutable component selection")
	}
	executor.componentPins[key] = pinned
	return componentSelectionReply{ComponentPins: maps.Clone(executor.componentPins), ComponentSources: maps.Clone(executor.componentSources), ResolvedBases: executor.componentContinuationBases()}, nil
}

func (executor *graphExecutor) componentContinuationBases() []stageWorkerBase {
	bases := make([]stageWorkerBase, 0, len(executor.resolvedBases))
	for key, source := range executor.resolvedBases {
		bases = append(bases, stageWorkerBase{Key: key, Source: source})
	}
	return bases
}

func validateComponentStageIdentity(previous, next planner.Stage) error {
	previous.Operations, next.Operations = nil, nil
	previous.History, next.History = nil, nil
	previous.Dependencies, next.Dependencies = nil, nil
	previous.AwaitingComponent, next.AwaitingComponent = nil, nil
	if !reflect.DeepEqual(previous, next) {
		return fmt.Errorf("component return changed suspended stage %s identity", previous.ID)
	}
	return nil
}

// Synthetic labels belong to the executed prefix, so initial execution and
// canonical continuation must construct them with the same stage/base inputs.
func graphStageLabelOperations(operations []planner.Operation, stage planner.Stage, enabled bool, aliases map[string]string, images map[string]stageState) []planner.Operation {
	if !enabled || len(operations) == 0 {
		return operations
	}
	name := stage.Name
	if name == "" {
		name = stage.ID
	}
	base := stage.Source
	if id, ok := aliases[strings.ToLower(base)]; ok {
		if selected, found := images[id]; found {
			base = selected.storageImageID
		}
	}
	label := definition.Instruction{Name: "label", Properties: map[string]string{"io.buildah.stage.name": name, "io.buildah.stage.base": base}}
	return append([]planner.Operation{{Instruction: label}}, operations...)
}

func componentInProcessActive(running map[string]bool, paused map[string]componentControlEvent, inProcess map[string]bool) bool {
	for id := range inProcess {
		if running[id] {
			if _, waiting := paused[id]; !waiting {
				return true
			}
		}
	}
	return false
}

func componentStageBaseCallback(events chan<- componentControlEvent, stageID string) baseSelectionHandler {
	return func(ctx context.Context, request baseSelectionRequest) (ResolvedImageSource, error) {
		event := componentControlEvent{stageID: stageID, base: &request, reply: make(chan componentControlResult, 1)}
		select {
		case events <- event:
		case <-ctx.Done():
			return ResolvedImageSource{}, ctx.Err()
		}
		select {
		case result := <-event.reply:
			return result.base, result.err
		case <-ctx.Done():
			return ResolvedImageSource{}, ctx.Err()
		}
	}
}
