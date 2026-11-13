package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/moby/buildkit/solver"
	"github.com/opencontainers/go-digest"
	"github.com/strio-dev/coopr/internal/imageconfig"
	"github.com/strio-dev/coopr/internal/planner"
	upstreamdefine "go.podman.io/buildah/define"
)

type declaredStageRequest struct {
	stageID string
	vertex  *instructionVertex
	images  map[string]stageState
	builder solver.Builder
	reply   chan instructionExecReply
}

// The coordinator owns admission, execution and replanning. This registry only
// declares native dependencies and requests demanded stage work.
type declaredStageGraph struct {
	executor  *graphExecutor
	vertices  map[string]*instructionVertex
	requests  chan declaredStageRequest
	options   PlanOptions
	capture   capturedContext
	namespace string
	observing bool
	outputID  string
}

func newDeclaredStageGraph(ctx context.Context, executor *graphExecutor, plan *planner.Plan, stages []planner.Stage, outputID string, bindings *graphBindings, observing bool) (*declaredStageGraph, error) {
	modules, err := cacheModuleVersions()
	if err != nil {
		return nil, err
	}
	namespace, err := componentCacheExecutorSemantics(upstreamdefine.Version, modules, executor.isolation.String(), executor.store.GraphDriverName(), executor.store.GraphOptions(), executor.instructionRuntime, executor.options.Output.Format)
	if err != nil {
		return nil, err
	}
	graph := &declaredStageGraph{executor: executor, vertices: map[string]*instructionVertex{}, requests: make(chan declaredStageRequest), options: executor.options, namespace: namespace, outputID: outputID, observing: observing}
	if err := graph.declare(ctx, plan, stages, bindings); err != nil {
		return nil, errors.Join(err, graph.close())
	}
	return graph, nil
}

// Known operations are authorized before lookup. Dynamic and unproven stages
// remain forced vertices whose bodies run through the canonical coordinator.
func (graph *declaredStageGraph) declare(ctx context.Context, plan *planner.Plan, stages []planner.Stage, bindings *graphBindings) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	options, output := graph.options, graph.options.Output
	forceAll := graph.observing || bindings != nil || options.NoCache || options.Lifecycle.NoLayers || options.Lifecycle.KeepIntermediate || options.Lifecycle.KeepFailed || options.Lifecycle.BuildUnusedStages || len(options.RunControls.Volumes) != 0 || len(options.RunControls.Devices) != 0 || output.Squash || output.SquashAll || len(output.SBOM) != 0 || output.ConfidentialWorkload.Convert
	immutable := !options.Lifecycle.NoLayers && !options.Lifecycle.KeepFailed && len(options.RunControls.Volumes) == 0 && bindings == nil && len(plan.Contexts) == 0 && len(options.TransientRunMounts) == 0 && !planUsesWritableContext(plan, nil)
	forced := map[string]bool{}
	sources := map[string]map[string]digest.Digest{}
	aliasesByStage := graphStageAliases(stages)
	needsContext := false
	for _, stage := range stages {
		unsafe := stage.AwaitingComponent != nil || stage.DynamicBaseStage != "" || stage.DeferredImageSource || stage.Kind != "from" || stage.SourceContext != ""
		for _, operation := range stage.Operations {
			devices, err := runDeviceCacheForPlannedOperation(operation, options.RunControls)
			if err != nil {
				return err
			}
			if err := authorizePlannedOperation(operation, graph.executor.allowedEntitlements, devices); err != nil {
				return err
			}
			if operation.Name == "component" || operation.Name == "layer" || operation.Name == "endlayer" || operation.ComponentBoundary != nil || len(operation.HostMounts) != 0 || operation.TransientMountCount != 0 || operation.InputContext != "" && operation.InputContext != "build" || len(operation.MountContexts) != 0 || operation.Name == "run" && (len(operation.Children) != 0 || graph.executor.instructionRuntime == "") || operation.Name == "add" && hasSpecialAddSource(operation.Arguments[:max(0, len(operation.Arguments)-1)]) {
				unsafe = true
			}
			if (operation.Name == "copy" || operation.Name == "add") && operation.Properties["from"] == "" && len(operation.Arguments) > 1 {
				needsContext = true
			}
		}
		force := forceAll || unsafe
		resolved := map[string]digest.Digest{}
		// NoCache changes reuse, not the immutable identity of known sources.
		if !unsafe {
			references := []string{stage.Source}
			for _, operation := range stage.Operations {
				for _, reference := range operationStageReferences(operation) {
					references = append(references, reference.source)
				}
			}
			for index, reference := range references {
				if reference == "" || strings.EqualFold(reference, "scratch") || aliasesByStage[stage.ID][strings.ToLower(reference)] != "" {
					continue
				}
				platform, err := executionPlatform(stage.Platform)
				if err != nil {
					return err
				}
				value, err := graph.executor.resolveInput(ctx, reference, platform, index != 0)
				if err != nil {
					return err
				}
				resolved[reference] = value.Selected.Digest
				if value.SourceManifest != nil {
					resolved[reference] = value.SourceManifest.Digest
				}
				if index == 0 && !stage.InheritedOnBuildPlanned {
					logical, err := imageconfig.Parse(value.ConfigData)
					if err != nil {
						return err
					}
					triggers, err := logical.OnBuild()
					if err != nil {
						return err
					}
					unsafe = len(triggers) != 0
					force = force || unsafe
					if unsafe {
						break
					}
				}
			}
		}
		forced[stage.ID], sources[stage.ID] = force, resolved
		immutable = immutable && !unsafe
	}
	if !immutable && graph.capture.index != nil {
		return errors.New("declared graph context became mutable after capture")
	}
	if immutable && needsContext && graph.capture.index == nil {
		var err error
		graph.options, graph.capture, err = captureBuildContext(ctx, options, true)
		if err != nil {
			return err
		}
		options = graph.options
	}
	var policy contextPolicy
	if graph.capture.index != nil {
		var err error
		policy, err = prepareContextPolicyWithIgnore(options.ContextDir, nil, options.IgnoreFile)
		if err != nil {
			return err
		}
	}
	for index, stage := range stages {
		inputIDs := slices.Clone(stage.Dependencies)
		// Serial mutable plans retain native source order, including producers
		// selected by planning without an explicit filesystem dependency.
		if index > 0 && !immutable && graphParallelism(options.Jobs, len(stages)) == 1 {
			previous := stages[index-1].ID
			if !slices.Contains(inputIDs, previous) {
				inputIDs = append(inputIDs, previous)
			}
		}
		primary := aliasesByStage[stage.ID][strings.ToLower(stage.Source)]
		if index := slices.Index(inputIDs, primary); primary != "" && index >= 0 {
			copy(inputIDs[1:index+1], inputIDs[:index])
			inputIDs[0] = primary
		}
		inputs := make([]*instructionVertex, len(inputIDs))
		force := forced[stage.ID]
		for index, id := range inputIDs {
			inputs[index] = graph.vertices[id]
			if inputs[index] == nil {
				return fmt.Errorf("stage %s has undeclared dependency %s", stage.ID, id)
			}
			force = force || inputs[index].ignore
		}
		var content []digest.Digest
		for _, operation := range stage.Operations {
			if (operation.Name == "copy" || operation.Name == "add") && operation.Properties["from"] == "" {
				if !immutable && len(operation.Arguments) > 1 {
					force = true
					continue
				}
				input, err := capturedCopyInput(ctx, graph.capture, policy, operation)
				if err != nil {
					return err
				}
				content = append(content, input)
			}
		}
		keyOutput := options.Output
		keyOutput.Path, keyOutput.Reference, keyOutput.BlobDirectory = "", "", ""
		// Filesystem destinations are replayed after selecting the image, just
		// like the OCI destination. They do not change that image's identity.
		keyOutput.Filesystem, keyOutput.Filesystems = FilesystemOutput{}, nil
		raw, err := json.Marshal(struct {
			Namespace                  string
			Stage                      planner.Stage
			Inputs                     []digest.Digest
			Sources                    map[string]digest.Digest
			Final                      bool
			Output                     Output
			Controls                   ImageControls
			Run                        RunControls
			Lifecycle                  LifecycleControls
			AddHosts                   []string
			Timestamp, SourceDateEpoch *int64
			RewriteTimestamp           bool
		}{graph.namespace, stage, content, sources[stage.ID], stage.ID == graph.outputID, keyOutput, options.ImageControls, options.RunControls, options.Lifecycle, options.AddHosts, options.Timestamp, options.SourceDateEpoch, options.RewriteTimestamp})
		if err != nil {
			return err
		}
		key := digest.FromString("coopr.native-stage.v2\n" + string(raw))
		if stage.Kind == "package-input" {
			if bindings == nil {
				return fmt.Errorf("stage %s package input has no binding", stage.ID)
			}
			bound, found := bindings.packages[stage.ID]
			if !found || bound.storageImageID == "" || bound.config == nil {
				return fmt.Errorf("stage %s package input is unavailable", stage.ID)
			}
			value, err := instructionStageResult(executedGraphStage{state: bound, root: bound.root})
			if err != nil {
				return err
			}
			key = digest.FromString(key.String() + "\n" + value.ID())
			if previous := graph.vertices[stage.ID]; previous != nil && previous.source != nil && previous.key == key {
				continue
			}
			graph.vertices[stage.ID] = graph.executor.instructionSolver.source(key, *value, false)
			continue
		}
		previous := graph.vertices[stage.ID]
		if previous != nil && previous.key == key && previous.ignore == force && slices.Equal(previous.inputVertices(), inputs) {
			continue
		}
		var parent *instructionVertex
		dependencies := inputs
		if primary != "" && len(inputIDs) != 0 && inputIDs[0] == primary {
			parent, dependencies = inputs[0], inputs[1:]
		}
		vertex := graph.executor.instructionSolver.declare(parent, dependencies, key, "stage "+stage.ID, force, nil)
		vertex.run = func(ctx context.Context, builder solver.Builder, values []*instructionSolverResult) (*instructionSolverResult, error) {
			images := make(map[string]stageState, len(values))
			for index, value := range values {
				state, err := instructionStageState(value)
				if err != nil {
					return nil, err
				}
				state.graphEndpoint = inputs[index]
				images[inputIDs[index]] = state
			}
			request := declaredStageRequest{stageID: stage.ID, vertex: vertex, images: images, builder: builder, reply: make(chan instructionExecReply, 1)}
			select {
			case graph.requests <- request:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			select {
			case reply := <-request.reply:
				return reply.result, reply.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		graph.vertices[stage.ID] = vertex
	}
	return nil
}

func (graph *declaredStageGraph) close() error {
	if graph.capture.cleanup != nil {
		cleanup := graph.capture.cleanup
		graph.capture.cleanup = nil
		return cleanup()
	}
	return nil
}

func instructionStageResult(executed executedGraphStage) (*instructionSolverResult, error) {
	logical, err := executed.state.config.MarshalJSON()
	if err != nil {
		return nil, err
	}
	command, retained := executed.state.config.CommandSet(), executed.state.retainsCaller
	return &instructionSolverResult{Entry: instructionCacheEntry{ImageID: executed.state.storageImageID, ManifestDigest: executed.state.manifestDigest, RootMetadata: executed.root, LogicalConfig: logical, CommandSet: &command, RetainsCaller: &retained, BaseImageID: executed.state.baseImageID}}, nil
}

func instructionStageState(result *instructionSolverResult) (stageState, error) {
	if err := validateInstructionResultState(result); err != nil {
		return stageState{}, err
	}
	if len(result.Entry.LogicalConfig) == 0 {
		return stageState{}, errors.New("stage result has no logical config")
	}
	logical, err := imageconfig.Restore(result.Entry.LogicalConfig, *result.Entry.CommandSet)
	if err != nil {
		return stageState{}, err
	}
	return stageState{storageImageID: result.Entry.ImageID, manifestDigest: result.Entry.ManifestDigest, root: result.Entry.RootMetadata, config: logical, baseImageID: result.Entry.BaseImageID, retainsCaller: *result.Entry.RetainsCaller}, nil
}
