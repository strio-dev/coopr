package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"coopr/internal/cache"
	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"github.com/containerd/platforms"
	"github.com/moby/buildkit/util/entitlements"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	buildahdocker "go.podman.io/buildah/docker"
	"go.podman.io/buildah/pkg/parse"
	nettypes "go.podman.io/common/libnetwork/types"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/system"
)

// BuildPlan executes the selected standalone container stage closure directly
// through Buildah, including component calls and dependencies on earlier
// stages or resolved images.
func BuildPlan(ctx context.Context, plan *planner.Plan, options PlanOptions) (result Result, retErr error) {
	if options.JobID != "" && !validWorkerJobID(options.JobID) {
		return Result{}, errors.New("invalid supervised build job ID")
	}
	stages, outputID, err := validateStandaloneGraph(plan)
	if err != nil {
		return Result{}, err
	}
	options, err = normalizePlanOptions(options)
	if err != nil {
		return Result{}, err
	}
	if options.SourceDateEpochOverride != nil {
		options.SourceDateEpoch = options.SourceDateEpochOverride
	} else {
		options.SourceDateEpoch = plan.SourceDateEpoch
	}
	if err := validateTimestampOptions(options.Timestamp, options.SourceDateEpoch, options.RewriteTimestamp); err != nil {
		return Result{}, err
	}
	for _, input := range plan.Inputs {
		if input.Kind != "image" || options.Resolver != nil {
			continue
		}
		selected := false
		for key, source := range options.ResolvedBases {
			if key.Reference == input.Reference && source.ImageID != "" {
				selected = true
				break
			}
		}
		if selected {
			continue
		}
		for _, stage := range stages {
			if stage.Source == input.Reference {
				return Result{}, fmt.Errorf("external image %q requires a Coopr image resolver", input.Reference)
			}
			for _, operation := range stage.Operations {
				for _, reference := range operationStageReferences(operation) {
					if reference.source == input.Reference {
						return Result{}, fmt.Errorf("external image %q requires a Coopr image resolver", input.Reference)
					}
				}
			}
		}
	}
	if err := validateRequest(Request{Store: options.Store, ContextDir: options.ContextDir, Output: options.Output}); err != nil {
		return Result{}, err
	}
	activity, err := acquirePlanActivity(ctx, options)
	if err != nil {
		return Result{}, fmt.Errorf("acquire build store activity lease: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, activity.Close()) }()
	return executePlanGraph(ctx, plan, options, stages, outputID, nil, nil)
}

type stageObserver func(storage.Store, planner.Stage, string, *imageconfig.Config, *PackageRootMetadata) error

type stageState struct {
	storageImageID string
	manifestDigest digest.Digest
	config         *imageconfig.Config
}

// graphExecutor owns resources and memoized inputs shared by one top-level
// build and any component graphs it invokes recursively. Stage images and
// aliases remain graph-local because stage IDs are only unique within a plan.
type graphExecutor struct {
	options                  PlanOptions
	progress                 io.Writer
	isolation                define.Isolation
	capabilities             []string
	storeLease               *storeLease
	store                    storage.Store
	network                  nettypes.ContainerNetwork
	system                   *types.SystemContext
	resolvedBases            map[ResolvedBaseKey]ResolvedImageSource
	replannedBaseDelta       func() map[ResolvedBaseKey]ResolvedImageSource
	packageImport            *ComponentPackageImporter
	componentCache           *componentCache
	instructionPortableCache *portableInstructionCache
	activeComponents         map[digest.Digest]bool
	resolvedComponents       map[string]*componentResolutionState
	componentPins            map[string]string
	resolveComponent         func(context.Context, ComponentPlanRequest) (*ResolvedComponentPlan, error)
	zeroLayerBases           map[digest.Digest]stageState
	instructionRuntime       string
	instructionRuntimeErr    error
	runtimeCacheWarning      sync.Once
	nextBuilderID            uint64
	sharedBuilderCounter     *uint64
	activeLocalComponents    map[string]bool
	allowedEntitlements      entitlements.Set
	stageRelayMu             sync.Mutex
}

func (executor *graphExecutor) nativeBuilder(builder *upstream.Builder, options upstream.BuilderOptions) nativeBuilder {
	controls := executor.options.RunControls
	return nativeBuilder{
		Builder: builder, isolation: options.Isolation,
		runtime: effectiveRuntime(executor.options.Runtime, controls), runtimeArgs: slices.Clone(controls.RuntimeFlags),
		secretSpecs: executor.options.Secrets, sshSpecs: executor.options.SSH,
		cgroupManager: controls.CgroupManager, cgroupManagerSet: controls.CgroupManagerSet,
		compatVolumes: executor.options.Lifecycle.CompatVolumes,
		runControls:   controls,
	}
}

func newGraphExecutor(ctx context.Context, options PlanOptions) (*graphExecutor, error) {
	options, err := normalizePlanOptions(options)
	if err != nil {
		return nil, err
	}
	if err := ConfigureRuntimeConfig(options.RunControls); err != nil {
		return nil, err
	}
	allowed, err := allowedEntitlements(options.Allow)
	if err != nil {
		return nil, err
	}
	format, err := normalizedOutputFormat(options.Output.Format)
	if err != nil {
		return nil, err
	}
	options.Output.Format = format
	isolation, err := parse.IsolationOption(options.Isolation)
	if err != nil {
		return nil, fmt.Errorf("select Buildah isolation: %w", err)
	}
	lease, err := acquireStore(options.Store)
	if err != nil {
		return nil, fmt.Errorf("open isolated containers/storage: %w", err)
	}
	network, err := newNetworkInterfaceWithControls(lease.store, options.RunControls)
	if err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	system := options.SystemContext
	if system == nil {
		system = &types.SystemContext{BigFilesTemporaryDir: os.TempDir()}
	}
	resolvedBases := make(map[ResolvedBaseKey]ResolvedImageSource, len(options.ResolvedBases))
	for key, resolved := range options.ResolvedBases {
		resolved.ConfigData = slices.Clone(resolved.ConfigData)
		resolvedBases[key] = resolved
	}
	executor := &graphExecutor{
		options: options, isolation: isolation, capabilities: rootCapabilities(),
		progress:   os.Stderr,
		storeLease: lease, store: lease.store, network: network, system: system,
		resolvedBases:         resolvedBases,
		replannedBaseDelta:    options.ReplannedBaseDelta,
		activeComponents:      make(map[digest.Digest]bool),
		resolvedComponents:    make(map[string]*componentResolutionState),
		resolveComponent:      ResolveComponentPlan,
		zeroLayerBases:        make(map[digest.Digest]stageState),
		activeLocalComponents: make(map[string]bool),
		allowedEntitlements:   allowed,
	}
	if parent := options.componentParent; parent != nil {
		executor.sharedBuilderCounter = parent.sharedBuilderCounter
		if executor.sharedBuilderCounter == nil {
			executor.sharedBuilderCounter = &parent.nextBuilderID
		}
		executor.activeLocalComponents = parent.activeLocalComponents
		executor.activeComponents = parent.activeComponents
		executor.resolvedComponents = parent.resolvedComponents
		executor.componentPins = parent.componentPins
	}
	executor.instructionRuntime, executor.instructionRuntimeErr = instructionRuntimeIdentity(effectiveRuntime(options.Runtime, options.RunControls))
	if options.Resolver != nil {
		executor.packageImport, err = NewComponentPackageImporter(options.Resolver, lease.store, system, os.TempDir())
		if err != nil {
			return nil, errors.Join(err, lease.Close())
		}
	}
	executor.componentCache, err = newComponentCache(ctx, options)
	if err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	executor.instructionPortableCache, err = newPortableInstructionCache(ctx, options)
	if err != nil {
		return nil, errors.Join(err, executor.componentCache.close(), lease.Close())
	}
	return executor, nil
}

func (executor *graphExecutor) Close() error {
	if executor == nil || executor.storeLease == nil {
		return nil
	}
	err := errors.Join(executor.componentCache.close(), executor.instructionPortableCache.close(), executor.storeLease.Close())
	executor.storeLease = nil
	return err
}

func (executor *graphExecutor) builderContainerName(stageID string) string {
	if executor.options.JobID == "" {
		return ""
	}
	counter := executor.sharedBuilderCounter
	if counter == nil {
		counter = &executor.nextBuilderID
	}
	id := atomic.AddUint64(counter, 1)
	return "coopr-" + executor.options.JobID + "-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatUint(id, 10) + "-" + stageID
}

// executePlanGraph owns the direct Buildah stage lifecycle shared by standalone
// builds and component package publication. Stages must already be validated
// and ordered by the planner.
func executePlanGraph(ctx context.Context, plan *planner.Plan, options PlanOptions, stages []planner.Stage, imageOutputID string, observed map[string]bool, observe stageObserver) (result Result, retErr error) {
	executor, err := newGraphExecutor(ctx, options)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		if closeErr := executor.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close containers/storage: %w", closeErr))
		}
	}()
	result, err = executor.executePlanGraph(ctx, plan, stages, imageOutputID, observed, observe, nil)
	if err == nil {
		var componentErr, instructionErr error
		if executor.componentCache != nil {
			componentErr = executor.componentCache.publish(ctx)
		}
		if executor.instructionPortableCache != nil {
			instructionErr = executor.instructionPortableCache.publish(ctx)
		}
		err = errors.Join(componentErr, instructionErr)
	}
	result.CacheStats = addCacheStats(cacheStats(executor.componentCache), portableInstructionCacheStats(executor.instructionPortableCache))
	if err == nil {
		err = ctx.Err()
	}
	return result, err
}

type graphBindings struct {
	caller             stageState
	packages           map[string]stageState
	cacheScope         CacheMountScope
	progressPrefix     string
	progressStageTotal int
	replannedBaseDelta func() map[ResolvedBaseKey]ResolvedImageSource
}

type preparedGraphStage struct {
	stage          planner.Stage
	base           string
	baseManifest   digest.Digest
	logical        *imageconfig.Config
	operations     []planner.Operation
	cacheMountID   CacheMountIDResolver
	cacheScope     *CacheMountScope
	bound          *stageState
	componentPins  map[string]string
	progressPrefix string
}

type executedGraphStage struct {
	state  stageState
	result Result
	root   *PackageRootMetadata
}

// executePlanGraph executes one graph while retaining executor-wide resources
// and resolved external bases for recursive component invocation.
func (executor *graphExecutor) executePlanGraph(ctx context.Context, plan *planner.Plan, stages []planner.Stage, imageOutputID string, observed map[string]bool, observe stageObserver, bindings *graphBindings) (Result, error) {
	stages, err := resolveAndValidateBuildNetwork(stages, executor.options.Network, executor.options.RunControls, executor.options.Isolation)
	if err != nil {
		return Result{}, err
	}
	restoreContexts, err := executor.materializePlanContexts(ctx, plan)
	if err != nil {
		return Result{}, err
	}
	restores := []func(){restoreContexts}
	defer func() {
		for index := len(restores) - 1; index >= 0; index-- {
			restores[index]()
		}
	}()
	images := make(map[string]stageState, len(stages))
	completed := make(map[string]bool, len(stages))
	running := make(map[string]bool, len(stages))
	finishedStages := make(map[string]executedGraphStage, len(stages))
	reported := make(map[string]bool, len(stages))
	parallelism := graphParallelism(executor.options.Jobs, len(stages))
	type stageCompletion struct {
		stageID string
		result  executedGraphStage
		err     error
	}
	graphCtx, cancelGraph := context.WithCancel(ctx)
	defer cancelGraph()
	results := make(chan stageCompletion, parallelism)
	runningCount := 0

	drain := func(primaryStage string, primary error) error {
		cancelGraph()
		failures := make(map[string]error)
		if primary != nil {
			failures[primaryStage] = primary
		}
		for runningCount > 0 {
			completion := <-results
			delete(running, completion.stageID)
			runningCount--
			if completion.err != nil {
				failures[completion.stageID] = errors.Join(failures[completion.stageID], completion.err)
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, stage := range stages {
			if err := failures[stage.ID]; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				return err
			}
		}
		for _, stage := range stages {
			if err := failures[stage.ID]; err != nil {
				return err
			}
		}
		return primary
	}
	reportCompleted := func() (Result, bool, error) {
		for _, stage := range stages {
			if !completed[stage.ID] {
				break
			}
			if reported[stage.ID] {
				continue
			}
			finished := finishedStages[stage.ID]
			if observe != nil && stage.Kind != "package-input" {
				if err := observe(executor.store, stage, finished.state.storageImageID, finished.state.config, finished.root); err != nil {
					return Result{}, false, fmt.Errorf("stage %s output: %w", stage.ID, err)
				}
			}
			if stage.ID == imageOutputID {
				if runningCount != 0 {
					return Result{}, false, fmt.Errorf("selected output stage %s completed before %d scheduled stages", imageOutputID, runningCount)
				}
				return finished.result, true, nil
			}
			reported[stage.ID] = true
		}
		return Result{}, false, nil
	}

	for len(completed) < len(stages) {
		ready, err := readyGraphStages(stages, completed, running, parallelism-runningCount)
		if err != nil {
			return Result{}, drain("", err)
		}
		replanned := false
		progressed := false
		aliasesByStage := graphStageAliases(stages)
		for _, index := range ready {
			stage := stages[index]
			if stage.DynamicBaseStage != "" {
				base, ok := images[stage.DynamicBaseStage]
				if !ok || base.config == nil || base.storageImageID == "" {
					return Result{}, drain(stage.ID, fmt.Errorf("stage %s dynamic base stage %s is unavailable", stage.ID, stage.DynamicBaseStage))
				}
				bind, err := stageBindFromImageConfig(base.config)
				if err != nil {
					return Result{}, drain(stage.ID, fmt.Errorf("stage %s dynamic base configuration: %w", stage.ID, err))
				}
				nextPlan, err := plan.ReplanDynamicStage(stage.ID, bind)
				if err != nil {
					return Result{}, drain(stage.ID, fmt.Errorf("stage %s dynamic base replan: %w", stage.ID, err))
				}
				nextStages, err := resolveAndValidateBuildNetwork(nextPlan.Stages, executor.options.Network, executor.options.RunControls, executor.options.Isolation)
				if err != nil {
					return Result{}, drain(stage.ID, fmt.Errorf("stage %s dynamic base replan: %w", stage.ID, err))
				}
				delta := executor.replannedBaseDelta
				if bindings != nil && bindings.replannedBaseDelta != nil {
					delta = bindings.replannedBaseDelta
				}
				if err := executor.adoptReplannedBases(graphCtx, delta); err != nil {
					return Result{}, drain(stage.ID, err)
				}
				restore, err := executor.materializePlanContexts(graphCtx, nextPlan)
				if err != nil {
					return Result{}, drain(stage.ID, err)
				}
				restores = append(restores, restore)
				protected := maps.Clone(completed)
				for stageID := range running {
					protected[stageID] = true
				}
				if err := validateReplannedStages(stages, nextStages, protected); err != nil {
					return Result{}, drain(stage.ID, err)
				}
				plan, stages = nextPlan, nextStages
				replanned = true
				break
			}
			aliases := aliasesByStage[stage.ID]
			prepared, err := executor.prepareGraphStage(graphCtx, plan, stage, aliases, images, bindings)
			if err != nil {
				return Result{}, drain(stage.ID, err)
			}
			if prepared.bound != nil {
				finished, err := executor.executeGraphStage(graphCtx, plan, prepared, aliases, images, imageOutputID, observed, true)
				if err != nil {
					return Result{}, drain(stage.ID, err)
				}
				images[stage.ID] = finished.state
				completed[stage.ID] = true
				finishedStages[stage.ID] = finished
				progressed = true
				continue
			}
			// Keep a lone ready stage in the current process. There is no work to
			// overlap, and Buildah network helpers must remain in the namespace
			// established by the supervised plan worker. Once another stage is
			// running, newly ready descendants still launch immediately.
			if parallelism == 1 || runningCount == 0 && len(ready) == 1 {
				finished, err := executor.executeGraphStage(graphCtx, plan, prepared, aliases, images, imageOutputID, observed, true)
				if err != nil {
					return Result{}, drain(stage.ID, err)
				}
				images[stage.ID] = finished.state
				completed[stage.ID] = true
				finishedStages[stage.ID] = finished
				progressed = true
				continue
			}
			if err := executor.prepareIsolatedStageComponents(graphCtx, &prepared); err != nil {
				return Result{}, drain(stage.ID, fmt.Errorf("stage %s component inputs: %w", stage.ID, err))
			}
			running[stage.ID] = true
			runningCount++
			currentPlan := plan
			currentImages := maps.Clone(images)
			currentResolvedBases := maps.Clone(executor.resolvedBases)
			stageID := stage.ID
			go func() {
				finished, err := executor.executeGraphStageIsolated(graphCtx, currentPlan, prepared, aliases, currentImages, currentResolvedBases, imageOutputID, observed)
				results <- stageCompletion{stageID: stageID, result: finished, err: err}
			}()
		}
		if replanned {
			continue
		}
		if progressed && runningCount == 0 {
			result, done, err := reportCompleted()
			if err != nil {
				return Result{}, drain("", err)
			}
			if done {
				return result, nil
			}
			continue
		}
		if runningCount == 0 {
			return Result{}, drain("", errors.New("build graph has no dependency-ready stage"))
		}
		completion := <-results
		delete(running, completion.stageID)
		runningCount--
		if completion.err != nil {
			return Result{}, drain(completion.stageID, completion.err)
		}
		images[completion.stageID] = completion.result.state
		completed[completion.stageID] = true
		finishedStages[completion.stageID] = completion.result
		result, done, err := reportCompleted()
		if err != nil {
			return Result{}, drain(completion.stageID, err)
		}
		if done {
			return result, nil
		}
	}
	if imageOutputID != "" {
		return Result{}, fmt.Errorf("selected output stage %s was not executed", imageOutputID)
	}
	return Result{}, nil
}

func graphStageAliases(stages []planner.Stage) map[string]map[string]string {
	ordered := stageAliases(stages)
	result := make(map[string]map[string]string, len(stages))
	for index, stage := range stages {
		result[stage.ID] = ordered[index]
	}
	return result
}

func validateReplannedStages(previous, next []planner.Stage, completed map[string]bool) error {
	previousByID := make(map[string]planner.Stage, len(previous))
	for _, stage := range previous {
		previousByID[stage.ID] = stage
	}
	nextByID := make(map[string]planner.Stage, len(next))
	for _, stage := range next {
		nextByID[stage.ID] = stage
	}
	for stageID := range completed {
		nextStage, retained := nextByID[stageID]
		if !retained {
			return fmt.Errorf("dynamic replan removed completed stage %s", stageID)
		}
		if !reflect.DeepEqual(previousByID[stageID], nextStage) {
			return fmt.Errorf("dynamic replan changed completed stage %s", stageID)
		}
	}
	return nil
}

func (executor *graphExecutor) adoptReplannedBases(ctx context.Context, takeDelta func() map[ResolvedBaseKey]ResolvedImageSource) error {
	if takeDelta == nil {
		return errors.New("dynamic replan produced no explicit selected-base delta")
	}
	for key, selected := range takeDelta() {
		if existing, found := executor.resolvedBases[key]; found {
			if existing.ImageID != selected.ImageID || existing.Selected.Digest != selected.Selected.Digest || !bytes.Equal(existing.ConfigData, selected.ConfigData) {
				return fmt.Errorf("replanned base %q changed immutable selection", key.Reference)
			}
			continue
		}
		platform, err := executionPlatform(key.Platform)
		if err != nil {
			return fmt.Errorf("replanned base %q platform: %w", key.Reference, err)
		}
		resolver := executor.options.Resolver
		if strings.HasPrefix(key.Reference, graphNamedContextPrefix) {
			resolver = nil
		}
		materialized, err := materializeImageSource(ctx, resolver, key.Reference, platform, executor.store, platformSystemContext(executor.system, platform), selected)
		if err != nil {
			return fmt.Errorf("materialize replanned base %q: %w", key.Reference, err)
		}
		executor.resolvedBases[key] = materialized
	}
	return nil
}

func (executor *graphExecutor) prepareGraphStage(ctx context.Context, plan *planner.Plan, stage planner.Stage, aliases map[string]string, images map[string]stageState, bindings *graphBindings) (preparedGraphStage, error) {
	parentPrefix := executor.options.ProgressPrefix
	stageTotal := executor.options.progressStageTotal
	if bindings != nil {
		parentPrefix = bindings.progressPrefix
		stageTotal = bindings.progressStageTotal
	}
	prepared := preparedGraphStage{stage: stage, base: "scratch", logical: imageconfig.New(), progressPrefix: graphProgressPrefix(stage, parentPrefix, stageTotal)}
	if stage.Kind == "package-input" {
		if bindings == nil {
			return preparedGraphStage{}, fmt.Errorf("stage %s package input has no component binding", stage.ID)
		}
		bound, ok := bindings.packages[stage.ID]
		if !ok || bound.storageImageID == "" || bound.config == nil {
			return preparedGraphStage{}, fmt.Errorf("stage %s package input is unavailable", stage.ID)
		}
		prepared.bound = &bound
		return prepared, nil
	}
	if stage.Kind == "extend" {
		if bindings == nil || bindings.caller.storageImageID == "" || bindings.caller.config == nil {
			return preparedGraphStage{}, fmt.Errorf("stage %s extend caller is unavailable", stage.ID)
		}
		prepared.base = bindings.caller.storageImageID
		prepared.baseManifest = bindings.caller.manifestDigest
		prepared.logical = bindings.caller.config.Clone()
	} else if stage.SourceContext != "" {
		resolved, ok := executor.namedContext(stage.SourceContext, stage.Platform)
		if !ok {
			return preparedGraphStage{}, fmt.Errorf("stage %s named context %q was not materialized", stage.ID, stage.SourceContext)
		}
		prepared.base = resolved.ImageID
		prepared.baseManifest = resolved.Selected.Digest
		var err error
		prepared.logical, err = imageconfig.Parse(resolved.ConfigData)
		if err != nil {
			return preparedGraphStage{}, fmt.Errorf("stage %s named context config: %w", stage.ID, err)
		}
	} else if baseStageID := aliases[strings.ToLower(stage.Source)]; baseStageID != "" {
		previous := images[baseStageID]
		if previous.storageImageID == "" || previous.config == nil {
			return preparedGraphStage{}, fmt.Errorf("stage %s depends on unavailable stage %s", stage.ID, baseStageID)
		}
		prepared.base = previous.storageImageID
		prepared.baseManifest = previous.manifestDigest
		prepared.logical = previous.config.Clone()
	} else if stage.Source != "" && !strings.EqualFold(stage.Source, "scratch") {
		key := ResolvedBaseKey{Reference: stage.Source, Platform: stage.Platform}
		resolved, ok := executor.resolvedBases[key]
		if !ok {
			if plan.Mode == planner.Invoke {
				return preparedGraphStage{}, fmt.Errorf("stage %s invocation base %q was not selected during planning", stage.ID, stage.Source)
			}
			platform, parseErr := platforms.Parse(stage.Platform)
			if parseErr != nil {
				return preparedGraphStage{}, fmt.Errorf("stage %s platform: %w", stage.ID, parseErr)
			}
			var err error
			resolved, err = executor.resolveBaseImage(ctx, stage.Source, platform)
			if err != nil {
				return preparedGraphStage{}, fmt.Errorf("stage %s: %w", stage.ID, err)
			}
		}
		prepared.base = resolved.ImageID
		prepared.baseManifest = resolved.Selected.Digest
		var err error
		prepared.logical, err = imageconfig.Parse(resolved.ConfigData)
		if err != nil {
			return preparedGraphStage{}, fmt.Errorf("stage %s base config: %w", stage.ID, err)
		}
	}
	if plan.Mode == planner.Build {
		if err := prepared.logical.PrepareStage(executor.options.ImageControls); err != nil {
			return preparedGraphStage{}, fmt.Errorf("stage %s apply image controls: %w", stage.ID, err)
		}
	}
	mergedOperations, historyErr := mergeHistoryOperations(normalizeGraphOperationContexts(plan.Mode, stage.Operations), stage.History)
	if historyErr != nil {
		return preparedGraphStage{}, fmt.Errorf("stage %s history: %w", stage.ID, historyErr)
	}
	prepared.operations = mergedOperations
	for operationIndex, operation := range prepared.operations {
		for _, reference := range operationStageReferences(operation) {
			if reference.context != "" {
				if _, ok := executor.namedContext(reference.context, stage.Platform); !ok {
					return preparedGraphStage{}, fmt.Errorf("stage %s operation %d named context %q was not materialized", stage.ID, operationIndex+1, reference.context)
				}
				continue
			}
			from := reference.source
			if aliases[strings.ToLower(from)] != "" {
				continue
			}
			key := ResolvedBaseKey{Reference: from, Platform: stage.Platform}
			if executor.resolvedBases[key].ImageID != "" {
				continue
			}
			if plan.Mode == planner.Invoke {
				return preparedGraphStage{}, fmt.Errorf("stage %s operation %d image source %q was not selected during planning", stage.ID, operationIndex+1, from)
			}
			platform, parseErr := platforms.Parse(stage.Platform)
			if parseErr != nil {
				return preparedGraphStage{}, fmt.Errorf("stage %s operation %d platform: %w", stage.ID, operationIndex+1, parseErr)
			}
			if _, err := executor.resolveBaseImage(ctx, from, platform); err != nil {
				return preparedGraphStage{}, fmt.Errorf("stage %s operation %d image source %q: %w", stage.ID, operationIndex+1, from, err)
			}
		}
	}
	if stage.Kind == "from" {
		if stage.InheritedOnBuildPlanned {
			if err := prepared.logical.ClearOnBuild(); err != nil {
				return preparedGraphStage{}, fmt.Errorf("stage %s clear planned ONBUILD: %w", stage.ID, err)
			}
		} else {
			inherited, err := consumeOnBuild(prepared.logical)
			if err != nil {
				return preparedGraphStage{}, fmt.Errorf("stage %s inherited ONBUILD: %w", stage.ID, err)
			}
			if len(inherited) != 0 {
				prepared.operations = append(inherited, prepared.operations...)
			}
		}
	}
	resolvedOperations, err := resolveAndValidateBuildNetwork([]planner.Stage{{ID: stage.ID, Operations: prepared.operations}}, executor.options.Network, executor.options.RunControls, executor.options.Isolation)
	if err != nil {
		return preparedGraphStage{}, err
	}
	prepared.operations = resolvedOperations[0].Operations
	if bindings != nil {
		scope := bindings.cacheScope
		scope.Stage = stage.ID
		prepared.cacheScope = &scope
		prepared.cacheMountID, err = scopedCacheMountIDResolver(scope)
	} else {
		scope, scopeErr := graphCacheMountScope(plan, stage)
		if scopeErr != nil {
			err = scopeErr
		} else {
			prepared.cacheScope = &scope
			prepared.cacheMountID, err = scopedCacheMountIDResolver(scope)
		}
	}
	if err != nil {
		return preparedGraphStage{}, fmt.Errorf("stage %s cache mount scope: %w", stage.ID, err)
	}
	if bindings == nil && executor.options.CacheMountID != nil {
		prepared.cacheScope = nil
		prepared.cacheMountID = executor.options.CacheMountID
	}
	return prepared, nil
}

// resolveBaseImage shares the immutable per-build image selection between
// invocation planning and graph execution.
func (executor *graphExecutor) resolveBaseImage(ctx context.Context, reference string, platform v1.Platform) (ResolvedImageSource, error) {
	platformString, err := componentPlatformString(platform)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	key := ResolvedBaseKey{Reference: reference, Platform: platformString}
	if resolved, found := executor.resolvedBases[key]; found {
		return resolved, nil
	}
	var resolved ResolvedImageSource
	if parent := executor.options.componentParent; parent != nil {
		resolved, err = parent.resolveBaseImage(ctx, reference, platform)
	} else {
		resolved, err = ResolveImageSource(ctx, executor.options.Resolver, reference, platform, executor.store, platformSystemContext(executor.system, platform))
	}
	if err != nil {
		return ResolvedImageSource{}, err
	}
	if executor.resolvedBases == nil {
		executor.resolvedBases = make(map[ResolvedBaseKey]ResolvedImageSource)
	}
	executor.resolvedBases[key] = resolved
	return resolved, nil
}

func (executor *graphExecutor) executeGraphStage(ctx context.Context, plan *planner.Plan, prepared preparedGraphStage, aliases map[string]string, images map[string]stageState, imageOutputID string, observed map[string]bool, allowInstructionCache bool) (executedGraphStage, error) {
	stage := prepared.stage
	if prepared.bound != nil {
		return executedGraphStage{state: *prepared.bound}, nil
	}
	progress := stageProgress{writer: executor.progress, prefix: prepared.progressPrefix, total: len(prepared.operations) + 1}
	instruction := definition.Instruction{Name: stage.Kind}
	if stage.Kind == "from" {
		instruction.Arguments = []string{stage.Source}
	}
	if stage.Name != "" {
		instruction.Arguments = append(instruction.Arguments, "AS", stage.Name)
	}
	progress.step(1, instruction)
	builderOptions, err := newBuilderOptionsWithHosts(prepared.base, executor.isolation, executor.capabilities, executor.network, os.TempDir(), executor.options.Output.Format, executor.options.AddHosts, executor.options.RunControls)
	if err != nil {
		return executedGraphStage{}, fmt.Errorf("stage %s output format: %w", stage.ID, err)
	}
	stagePlatform, err := executionPlatform(stage.Platform)
	if err != nil {
		return executedGraphStage{}, fmt.Errorf("stage %s platform: %w", stage.ID, err)
	}
	builderOptions.SystemContext = platformSystemContext(executor.system, stagePlatform)
	builderOptions.Container = executor.builderContainerName(stage.ID)
	builderBase := prepared.base
	if prepared.baseManifest != "" {
		builderBase, err = selectedBuilderBase(ctx, executor.store, prepared.base, prepared.baseManifest)
		if err != nil {
			return executedGraphStage{}, fmt.Errorf("stage %s select base manifest %s: %w", stage.ID, prepared.baseManifest, err)
		}
	}
	selectedBuilderOptions := builderOptions
	selectedBuilderOptions.FromImage = builderBase
	builder, err := upstream.NewBuilder(ctx, executor.store, selectedBuilderOptions)
	if err != nil {
		return executedGraphStage{}, fmt.Errorf("create Buildah builder for stage %s from %q: %w", stage.ID, prepared.base, err)
	}
	if prepared.baseManifest != "" {
		builder.FromImage = prepared.base
		builder.FromImageID = prepared.base
	}
	builder.SetOS(stagePlatform.OS)
	builder.SetArchitecture(stagePlatform.Architecture)
	builder.SetVariant(stagePlatform.Variant)
	rootBaseline, err := capturePackageRootMetadata(executor.store, builder)
	if err != nil {
		_ = builder.Delete()
		return executedGraphStage{}, fmt.Errorf("stage %s root metadata baseline: %w", stage.ID, err)
	}
	imageID, stageResult, rootMetadata, err := executor.executePlanStage(ctx, builder, builderOptions, prepared.operations, aliases, images, stagePlatform, prepared.cacheMountID, prepared.logical, prepared.baseManifest, rootBaseline, stage.ID == imageOutputID, observed[stage.ID], allowInstructionCache, progress)
	if err != nil {
		return executedGraphStage{}, fmt.Errorf("stage %s: %w", stage.ID, err)
	}
	if stage.ID == imageOutputID {
		progress.commit(executor.options.ProgressReference)
	}
	progress.image(imageID)
	manifestDigest := digest.Digest(stageResult.ManifestDigest)
	return executedGraphStage{state: stageState{storageImageID: imageID, manifestDigest: manifestDigest, config: prepared.logical}, result: stageResult, root: rootMetadata}, nil
}

func normalizeGraphOperationContexts(mode planner.Mode, operations []planner.Operation) []planner.Operation {
	want := ""
	switch mode {
	case planner.Publish:
		want = "publisher"
	case planner.Invoke:
		want = "caller"
	}
	if want == "" {
		return operations
	}
	result := slices.Clone(operations)
	for index := range result {
		if result[index].InputContext == want {
			result[index].InputContext = "build"
		}
		if len(result[index].MountContexts) != 0 {
			result[index].MountContexts = maps.Clone(result[index].MountContexts)
			for child, contextName := range result[index].MountContexts {
				if contextName == want {
					result[index].MountContexts[child] = "build"
				}
			}
		}
	}
	return result
}

func mergeHistoryOperations(operations []planner.Operation, history []planner.HistoryOperation) ([]planner.Operation, error) {
	if len(history) == 0 {
		return operations, nil
	}
	previous := 0
	for index, entry := range history {
		if entry.Before < previous || entry.Before > len(operations) {
			return nil, fmt.Errorf("entry %d has invalid position %d", index+1, entry.Before)
		}
		if entry.Operation.Name != "arg" {
			return nil, fmt.Errorf("entry %d contains executable instruction %q", index+1, entry.Operation.Name)
		}
		if err := definition.ValidateResolved(entry.Operation.Instruction); err != nil {
			return nil, fmt.Errorf("entry %d: %w", index+1, err)
		}
		previous = entry.Before
	}
	result := make([]planner.Operation, 0, len(operations)+len(history))
	historyIndex := 0
	for operationIndex := 0; operationIndex <= len(operations); operationIndex++ {
		for historyIndex < len(history) && history[historyIndex].Before == operationIndex {
			result = append(result, history[historyIndex].Operation)
			historyIndex++
		}
		if operationIndex < len(operations) {
			result = append(result, operations[operationIndex])
		}
	}
	return result, nil
}

type stageReference struct {
	source  string
	name    string
	context string
}

func operationStageReferences(operation planner.Operation) []stageReference {
	references := []stageReference{}
	if (operation.Name == "copy" || operation.Name == "add") && operation.Properties["from"] != "" {
		references = append(references, stageReference{source: operation.Properties["from"], name: strings.ToUpper(operation.Name) + " --from", context: operation.InputContext})
	}
	for mountIndex, mount := range operation.Children {
		if mount.Properties["from"] != "" {
			references = append(references, stageReference{source: mount.Properties["from"], name: fmt.Sprintf("RUN mount %d from", mountIndex+1), context: operation.MountContexts[mountIndex]})
		}
	}
	return references
}

func validateStandaloneGraph(plan *planner.Plan) ([]planner.Stage, string, error) {
	if plan == nil {
		return nil, "", errors.New("plan is nil")
	}
	if plan.Mode != planner.Build || plan.DefinitionType != "container" {
		return nil, "", errors.New("direct Buildah graph requires a standalone container build plan")
	}
	if len(plan.Outputs) != 1 {
		return nil, "", fmt.Errorf("direct Buildah graph requires exactly one output, got %d", len(plan.Outputs))
	}
	if len(plan.Stages) == 0 {
		return nil, "", errors.New("direct Buildah graph requires at least one stage")
	}
	externalInputs := make(map[string]bool, len(plan.Inputs))
	namedContexts := make(map[string]bool, len(plan.Contexts))
	for _, input := range plan.Contexts {
		name := strings.ToLower(input.Name)
		if name == "" || namedContexts[name] {
			return nil, "", fmt.Errorf("plan contains invalid or duplicate named context %q", input.Name)
		}
		namedContexts[name] = true
	}
	for _, input := range plan.Inputs {
		switch input.Kind {
		case "image":
			externalInputs[input.Reference] = true
		case "context":
			if !namedContexts[strings.ToLower(input.Reference)] {
				return nil, "", fmt.Errorf("plan context input %q has no materialization specification", input.Reference)
			}
		case "component":
			// Component references are resolved at their source-ordered operation.
		default:
			return nil, "", fmt.Errorf("plan input kind %q is not supported", input.Kind)
		}
	}
	if _, err := executionPlatform(plan.Platform); err != nil {
		return nil, "", fmt.Errorf("build platform: %w", err)
	}

	knownIDs := make(map[string]bool, len(plan.Stages))
	stagePlatforms := make(map[string]string, len(plan.Stages))
	aliases := make(map[string]string, len(plan.Stages))
	for _, stage := range plan.Stages {
		if stage.Kind != "from" {
			return nil, "", fmt.Errorf("stage %s kind %q is not supported", stage.ID, stage.Kind)
		}
		if _, err := executionPlatform(stage.Platform); err != nil {
			return nil, "", fmt.Errorf("stage %s platform: %w", stage.ID, err)
		}
		if knownIDs[stage.ID] {
			return nil, "", fmt.Errorf("duplicate stage ID %q", stage.ID)
		}
		baseID := ""
		if stage.SourceContext != "" {
			if !namedContexts[strings.ToLower(stage.SourceContext)] {
				return nil, "", fmt.Errorf("stage %s named context %q is unavailable", stage.ID, stage.SourceContext)
			}
		} else if !strings.EqualFold(stage.Source, "scratch") {
			baseID = aliases[strings.ToLower(stage.Source)]
			if baseID == "" && !externalInputs[stage.Source] {
				return nil, "", fmt.Errorf("stage %s external base %q lacks a declared image input", stage.ID, stage.Source)
			}
			if baseID != "" && stagePlatforms[baseID] != stage.Platform {
				return nil, "", fmt.Errorf("stage %s platform %q differs from base stage %s platform %q", stage.ID, stage.Platform, baseID, stagePlatforms[baseID])
			}
		}
		wantDependencies := []string{}
		if baseID != "" {
			wantDependencies = append(wantDependencies, baseID)
		}
		for operationIndex, operation := range stage.Operations {
			for _, reference := range operationStageReferences(operation) {
				if reference.context != "" {
					if !namedContexts[strings.ToLower(reference.context)] {
						return nil, "", fmt.Errorf("stage %s operation %d: %s references unavailable named context %q", stage.ID, operationIndex+1, reference.name, reference.context)
					}
					continue
				}
				sourceID := aliases[strings.ToLower(reference.source)]
				if sourceID == "" {
					if externalInputs[reference.source] {
						continue
					}
					return nil, "", fmt.Errorf("stage %s operation %d: %s references an unavailable stage %q", stage.ID, operationIndex+1, reference.name, reference.source)
				}
				if !slices.Contains(wantDependencies, sourceID) {
					wantDependencies = append(wantDependencies, sourceID)
				}
			}
		}
		if len(stage.Dependencies) != len(wantDependencies) {
			return nil, "", fmt.Errorf("stage %s dependencies %v do not match source dependencies %v", stage.ID, stage.Dependencies, wantDependencies)
		}
		for _, dependency := range stage.Dependencies {
			if !slices.Contains(wantDependencies, dependency) {
				return nil, "", fmt.Errorf("stage %s dependency %q is not a source dependency", stage.ID, dependency)
			}
		}
		knownIDs[stage.ID] = true
		stagePlatforms[stage.ID] = stage.Platform
		aliases[stage.ID] = stage.ID
		if stage.Name != "" {
			aliases[strings.ToLower(stage.Name)] = stage.ID
		}
	}
	outputID := plan.Outputs[0]
	if !knownIDs[outputID] {
		return nil, "", fmt.Errorf("output %q does not identify an executed stage", outputID)
	}
	if stagePlatforms[outputID] != plan.Platform {
		return nil, "", fmt.Errorf("output stage %s platform %q differs from plan platform %q", outputID, stagePlatforms[outputID], plan.Platform)
	}
	if plan.Stages[len(plan.Stages)-1].ID != outputID {
		return nil, "", fmt.Errorf("selected output must be the final stage in its retained graph")
	}
	return slices.Clone(plan.Stages), outputID, nil
}

func lowerGraphOperations(planned []planner.Operation, aliases map[string]string, images map[string]stageState, store storage.Store, shell []string, cacheMountID CacheMountIDResolver) ([]Operation, error) {
	return lowerGraphOperationsWithSelectedImages(planned, aliases, images, store, shell, cacheMountID, nil)
}

func lowerGraphOperationsWithSelectedImages(planned []planner.Operation, aliases map[string]string, images map[string]stageState, store storage.Store, shell []string, cacheMountID CacheMountIDResolver, selectedBases map[ResolvedBaseKey]ResolvedImageSource) ([]Operation, error) {
	result := make([]Operation, 0, len(planned))
	for index, operation := range planned {
		if operation.Name == "component" {
			result = append(result, componentGraphOperation{planned: operation})
			continue
		}
		from := operation.Properties["from"]
		if (operation.Name == "copy" || operation.Name == "add") && from != "" {
			stageID := aliases[strings.ToLower(from)]
			imageID := ""
			if operation.InputContext != "" {
				imageID = selectedNamedContextImage(selectedBases, operation.InputContext)
				if imageID == "" {
					return nil, fmt.Errorf("operation %d %s: named context %q is unavailable", index+1, operation.Name, operation.InputContext)
				}
			}
			if imageID == "" {
				if stageID != "" {
					imageID = images[stageID].storageImageID
				} else {
					for key, selected := range selectedBases {
						if key.Reference == from {
							if imageID != "" && imageID != selected.ImageID {
								return nil, fmt.Errorf("operation %d %s: image source %q has multiple selected platforms", index+1, operation.Name, from)
							}
							imageID = selected.ImageID
						}
					}
				}
			}
			if imageID == "" {
				return nil, fmt.Errorf("operation %d %s: source stage or selected image %q is unavailable", index+1, operation.Name, from)
			}
			instruction := operation.Instruction
			instruction.Properties = maps.Clone(instruction.Properties)
			delete(instruction.Properties, "from")
			lowered, _, err := lowerCopyOrAdd(instruction, "build")
			if err != nil {
				return nil, fmt.Errorf("operation %d %s: %w", index+1, operation.Name, err)
			}
			switch copied := lowered[0].(type) {
			case Copy:
				result = append(result, copyFromImageOperation{
					store: store, imageID: imageID, sources: copied.Sources, destination: copied.Destination,
					inlineFiles: copied.InlineFiles,
					options:     upstream.AddAndCopyOptions{Chown: copied.Chown, Chmod: copied.Chmod, Parents: copied.Parents, Excludes: copied.Excludes, Link: copied.Link},
				})
			case Add:
				extract := copied.Unpack == nil || *copied.Unpack
				result = append(result, copyFromImageOperation{
					store: store, imageID: imageID, sources: copied.Sources, destination: copied.Destination,
					inlineFiles: copied.InlineFiles,
					extract:     extract, options: upstream.AddAndCopyOptions{Chown: copied.Chown, Chmod: copied.Chmod, Checksum: copied.Checksum, Parents: copied.Parents, Excludes: copied.Excludes, Link: copied.Link},
				})
			default:
				return nil, fmt.Errorf("operation %d %s: unexpected lowering type %T", index+1, operation.Name, lowered[0])
			}
			continue
		}
		var boundMountSources map[int]string
		if operation.Name == "run" {
			for mountIndex, mount := range operation.Children {
				from := mount.Properties["from"]
				if from == "" {
					continue
				}
				stageID := aliases[strings.ToLower(from)]
				imageID := ""
				if contextName := operation.MountContexts[mountIndex]; contextName != "" {
					imageID = selectedNamedContextImage(selectedBases, contextName)
					if imageID == "" {
						return nil, fmt.Errorf("operation %d RUN mount %d: named context %q is unavailable", index+1, mountIndex+1, contextName)
					}
				}
				if imageID == "" {
					if stageID != "" {
						imageID = images[stageID].storageImageID
					} else {
						for key, selected := range selectedBases {
							if key.Reference == from {
								if imageID != "" && imageID != selected.ImageID {
									return nil, fmt.Errorf("operation %d RUN mount %d: image source %q has multiple selected platforms", index+1, mountIndex+1, from)
								}
								imageID = selected.ImageID
							}
						}
					}
				}
				if imageID == "" {
					return nil, fmt.Errorf("operation %d RUN mount %d: source stage or selected image %q is unavailable", index+1, mountIndex+1, from)
				}
				if boundMountSources == nil {
					boundMountSources = make(map[int]string)
				}
				boundMountSources[mountIndex] = imageID
			}
		}
		lowered, nextShell, err := lowerOperationWithCacheMountIDs(operation, shell, cacheMountID, boundMountSources)
		if err != nil {
			return nil, fmt.Errorf("operation %d %q: %w", index+1, operation.Name, err)
		}
		if len(boundMountSources) != 0 {
			for loweredIndex, item := range lowered {
				if run, ok := item.(Run); ok {
					run.sourceStore = store
					lowered[loweredIndex] = run
				}
			}
		}
		result = append(result, lowered...)
		if nextShell != nil {
			shell = nextShell
		}
	}
	return result, nil
}

func selectedNamedContextImage(selected map[ResolvedBaseKey]ResolvedImageSource, name string) string {
	reference := graphNamedContextPrefix + strings.ToLower(name)
	imageID := ""
	for key, source := range selected {
		if key.Reference != reference || source.ImageID == "" {
			continue
		}
		if imageID != "" && imageID != source.ImageID {
			return ""
		}
		imageID = source.ImageID
	}
	return imageID
}

// componentGraphOperation preserves a component call's position among lowered
// operations. graphExecutor intercepts it before the generic operation path.
type componentGraphOperation struct {
	planned planner.Operation
}

func (componentGraphOperation) apply(operationBuilder, string) error {
	return errors.New("component operation was not intercepted by graph executor")
}

type historyOnlyOperation struct{}

func (historyOnlyOperation) apply(operationBuilder, string) error { return nil }

type copyFromImageOperation struct {
	store       storage.Store
	imageID     string
	sources     []string
	inlineFiles []definition.InlineFile
	destination string
	extract     bool
	options     upstream.AddAndCopyOptions
}

func (operation copyFromImageOperation) apply(builder operationBuilder, _ string) error {
	native, ok := builder.(nativeBuilder)
	if !ok {
		return errors.New("COPY --from requires a native Buildah builder")
	}
	if len(operation.sources) != 0 {
		if err := CopyFromImage(operation.store, operation.imageID, native.Builder, operation.destination, operation.extract, operation.options, operation.sources...); err != nil {
			return err
		}
	}
	for _, source := range operation.inlineFiles {
		if err := addInlineData(builder, operation.destination, source, inlineCopyOptions(operation.options)); err != nil {
			return err
		}
	}
	return nil
}

func linkedCopyOrAdd(operation Operation) bool {
	switch value := operation.(type) {
	case Copy:
		return value.Link
	case Add:
		return value.Link
	case copyFromImageOperation:
		return value.options.Link
	default:
		return false
	}
}

func (executor *graphExecutor) executePlanStage(ctx context.Context, builder *upstream.Builder, builderOptions upstream.BuilderOptions, planned []planner.Operation, aliases map[string]string, images map[string]stageState, platform v1.Platform, cacheMountID CacheMountIDResolver, logical *imageconfig.Config, baseManifest digest.Digest, rootBaseline *PackageRootMetadata, output, captureRoot, allowInstructionCache bool, progress stageProgress) (imageID string, result Result, root *PackageRootMetadata, retErr error) {
	store := executor.store
	options := executor.options
	current := builder
	currentManifest := baseManifest
	rusageLogger, rusageErr := newInstructionRusageLogger(options)
	if rusageErr != nil {
		return "", Result{}, nil, rusageErr
	}
	if rusageLogger != nil {
		defer func() { retErr = errors.Join(retErr, rusageLogger.log(), rusageLogger.close()) }()
	}
	defer func() {
		if current != nil && options.Lifecycle.removeBuilder(retErr != nil, ctx.Err() != nil) {
			retErr = errors.Join(retErr, current.Delete())
		}
	}()
	originalBaseID := builder.FromImageID
	if output && options.Output.Squash && !options.Output.SquashAll && originalBaseID == "" {
		var err error
		originalBaseID, _, err = commitStoredSnapshotSelected(ctx, builder, builderOptions.SystemContext, builderOptions.Format, true, timestampPolicyFromOptions(options))
		if err != nil {
			return "", Result{}, nil, fmt.Errorf("capture empty squash base: %w", err)
		}
	}
	if err := syncBuilderConfig(current, logical); err != nil {
		return "", Result{}, nil, fmt.Errorf("synchronize base image configuration: %w", err)
	}
	var currentRoot *PackageRootMetadata
	if captureRoot {
		currentRoot = rootBaseline
	}

	remainingFilesystem := plannedFilesystemOperationCount(planned)
	var finalCachedImageID string
	var finalCachedManifest digest.Digest
	var finalCacheKey digest.Digest
	var finalPortableCacheKey cache.ImageKey
	finalCacheEligible := false
	finalPortableCacheEligible := false
	finalCacheDirect := false
	pendingLinked := false
	cachedAnnotationsChanged := false
	historyOnlyTail := len(planned) == 0
	artifacts := append(slices.Clone(options.ContextArtifacts), options.Store.RunRoot, options.Store.GraphRoot, options.Output.Path, options.CacheLocalDir)
	if executor.componentCache != nil {
		artifacts = append(artifacts, executor.componentCache.stagingDir)
	}
	for index, plannedOperation := range planned {
		progress.step(index+2, plannedOperation.Instruction)
		componentFilesystemChanged := true
		if err := rusageLogger.log(); err != nil {
			return "", Result{}, nil, err
		}
		if err := ctx.Err(); err != nil {
			return "", Result{}, nil, err
		}
		// Entitlements are request authorization, not cache inputs. Check them
		// before lowering and before any instruction-cache lookup.
		deviceCache, err := runDeviceCacheForPlannedOperation(plannedOperation)
		if err != nil {
			return "", Result{}, nil, fmt.Errorf("operation %d: load CDI devices: %w", index+1, err)
		}
		if err := authorizePlannedOperation(plannedOperation, executor.allowedEntitlements, deviceCache); err != nil {
			return "", Result{}, nil, fmt.Errorf("operation %d: %w", index+1, err)
		}
		platformString := platforms.Format(platform)
		operations, err := lowerPlannedGraphOperation(plannedOperation, aliases, images, store, platformString, executor.resolvedBases, logical, cacheMountID)
		if err != nil {
			return "", Result{}, nil, fmt.Errorf("operation %d: %w", index+1, err)
		}
		if options.Lifecycle.NoLayers {
			for index, operation := range operations {
				operations[index] = withoutLinkedLayers(operation)
			}
		}
		for operationIndex, lowered := range operations {
			if run, ok := lowered.(Run); ok {
				run.ContextIgnoreFile = options.IgnoreFile
				run.cacheLockRoot = store.GraphRoot()
				run.Devices, _ = applyRunDeviceEntitlementAliases(run.Devices, executor.allowedEntitlements)
				run.SecretSpecs = slices.Clone(options.Secrets)
				run.SSHSpecs = slices.Clone(options.SSH)
				run.Env = mergePredefinedProxyEnvironment(run.Env, options.ProxyArgs, plannedOperation.DeclaredProxyArguments, plannedOperation.ShadowedProxyArguments)
				operations[operationIndex] = run
			} else if copied, ok := lowered.(Copy); ok {
				copied.IgnoreFile = options.IgnoreFile
				operations[operationIndex] = copied
			} else if added, ok := lowered.(Add); ok {
				added.IgnoreFile = options.IgnoreFile
				operations[operationIndex] = added
			}
		}
		linkedLayerStart := len(current.AppendedLinkedLayers)
		var cacheKey digest.Digest
		var portableCacheKey cache.ImageKey
		portableCacheable := false
		cacheHit := false
		var inputDigest digest.Digest
		var inputDigestCandidates []digest.Digest
		inputApplied := false
		inputProbed := false
		var prefetchedCacheEntry *instructionCacheEntry
		var preparedRun *preparedRunInput
		finalFilesystemOperation := remainingFilesystem == 1
		cacheable := !options.Lifecycle.NoLayers && allowInstructionCache && remainingFilesystem > 0 && len(operations) == 1
		if cacheable {
			switch operation := operations[0].(type) {
			case Run:
				if operation.Stdin != nil {
					cacheable = false
					break
				}
				if executor.instructionRuntimeErr != nil {
					executor.runtimeCacheWarning.Do(func() {
						_, _ = fmt.Fprintf(os.Stderr, "coopr: RUN instruction cache disabled: %v\n", executor.instructionRuntimeErr)
					})
					cacheable = false
					break
				}
				adapter := executor.nativeBuilder(current, builderOptions)
				prepared, prepareErr := prepareRunInput(ctx, adapter, options.ContextDir, artifacts, operation)
				if prepareErr != nil {
					return "", Result{}, nil, fmt.Errorf("operation %d prepare RUN inputs: %w", index+1, prepareErr)
				}
				preparedRun = &prepared
				defer func() {
					if prepared.cleanup != nil {
						retErr = errors.Join(retErr, prepared.cleanup())
					}
				}()
			case WorkDir:
			case Copy:
				adapter := executor.nativeBuilder(current, builderOptions)
				if candidates, eligible := simpleLocalCopyDigestCandidates(options.ContextDir, artifacts, operation); eligible {
					inputDigestCandidates = candidates
				} else if copyAddProbeEligible(operation) {
					inputDigest, err = probeCopyAddDigestContext(ctx, adapter, options.ContextDir, artifacts, operation)
					inputProbed = err == nil
				} else {
					inputDigest, err = applyCopyAddWithDigestContext(ctx, adapter, options.ContextDir, artifacts, operation)
					inputApplied = err == nil
				}
				if err != nil {
					return "", Result{}, nil, fmt.Errorf("operation %d: %w", index+1, err)
				}
			case Add:
				adapter := executor.nativeBuilder(current, builderOptions)
				if pinned, declared, digestErr := pinnedHTTPAddInputDigest(operation); digestErr != nil {
					err = digestErr
				} else if declared {
					inputDigest = pinned
				} else if copyAddProbeEligible(operation) {
					inputDigest, err = probeCopyAddDigestContext(ctx, adapter, options.ContextDir, artifacts, operation)
					inputProbed = err == nil
				} else {
					inputDigest, err = applyCopyAddWithDigestContext(ctx, adapter, options.ContextDir, artifacts, operation)
					inputApplied = err == nil
				}
				if err != nil {
					return "", Result{}, nil, fmt.Errorf("operation %d: %w", index+1, err)
				}
			case copyFromImageOperation:
				adapter := executor.nativeBuilder(current, builderOptions)
				if copyAddProbeEligible(operation) {
					inputDigest, err = probeCopyAddDigestContext(ctx, adapter, options.ContextDir, artifacts, operation)
					inputProbed = err == nil
				} else {
					inputDigest, err = applyCopyAddWithDigestContext(ctx, adapter, options.ContextDir, artifacts, operation)
					inputApplied = err == nil
				}
				if err != nil {
					return "", Result{}, nil, fmt.Errorf("operation %d: %w", index+1, err)
				}
			default:
				cacheable = false
			}
		}
		if cacheable {
			var resolvedInputs []instructionCacheResolvedInput
			var resolvedInputsComplete bool
			if preparedRun != nil {
				resolvedInputs = preparedRun.resolved
				resolvedInputsComplete = preparedRun.complete
			}
			var cacheRoot *PackageRootMetadata
			if captureRoot {
				cacheRoot = currentRoot
			}
			// Cache filesystem inputs rather than the checkpoint's creation time.
			// Fresh scratch builders have no committed RootFS yet.
			cacheParent := "empty"
			if current.FromImageID != "" {
				cacheParent, err = rootFSIdentity(current.OCIv1.RootFS)
				if err != nil {
					return "", Result{}, nil, fmt.Errorf("operation %d cache parent: %w", index+1, err)
				}
			}
			cacheInput := instructionCacheInput{
				ParentRootFS: cacheParent, Logical: logical, Operation: plannedOperation,
				Platform: current.OCIv1.Platform, Isolation: builderOptions.Isolation.String(), Runtime: executor.instructionRuntime,
				Format: executor.options.Output.Format, InputDigest: inputDigest,
				ResolvedInputs: resolvedInputs, ResolvedInputsComplete: resolvedInputsComplete,
				RootMetadata: cacheRoot,
				Timestamp:    options.Timestamp, SourceDateEpoch: options.SourceDateEpoch, RewriteTimestamp: options.RewriteTimestamp,
				AddHosts:      slices.Clone(options.AddHosts),
				RunControls:   options.RunControls,
				CompatVolumes: options.Lifecycle.CompatVolumes,
			}
			if len(inputDigestCandidates) != 0 && !options.NoCache {
				hits := make([]simpleCopyCacheCandidate, 0, len(inputDigestCandidates))
				seenKeys := make(map[digest.Digest]bool, len(inputDigestCandidates))
				for _, candidate := range inputDigestCandidates {
					cacheInput.InputDigest = candidate
					candidateKey, candidateCacheable, candidateErr := instructionCacheKey(cacheInput)
					if candidateErr != nil {
						return "", Result{}, nil, fmt.Errorf("operation %d cache key: %w", index+1, candidateErr)
					}
					if !candidateCacheable || seenKeys[candidateKey] {
						continue
					}
					seenKeys[candidateKey] = true
					entry, lookupErr := findInstructionCacheEntry(store, candidateKey, options.CacheTTL)
					if lookupErr != nil {
						return "", Result{}, nil, fmt.Errorf("operation %d cache lookup: %w", index+1, lookupErr)
					}
					if captureRoot && entry.RootMetadata == nil {
						entry.ImageID = ""
					}
					if entry.ImageID != "" {
						hits = append(hits, simpleCopyCacheCandidate{key: candidateKey, input: candidate, entry: entry})
					}
				}
				unique, ambiguous := uniqueSimpleCopyCacheCandidate(hits)
				if unique != nil {
					cacheKey = unique.key
					inputDigest = unique.input
					cacheInput.InputDigest = inputDigest
					prefetchedCacheEntry = &unique.entry
				} else if !ambiguous {
					adapter := executor.nativeBuilder(current, builderOptions)
					inputDigest, err = applyCopyAddWithDigestContext(ctx, adapter, options.ContextDir, artifacts, operations[0])
					if err != nil {
						return "", Result{}, nil, fmt.Errorf("operation %d: %w", index+1, err)
					}
					inputApplied = true
					cacheInput.InputDigest = inputDigest
					cacheKey, cacheable, err = instructionCacheKey(cacheInput)
				} else {
					adapter := executor.nativeBuilder(current, builderOptions)
					inputDigest, err = probeCopyAddDigestContext(ctx, adapter, options.ContextDir, artifacts, operations[0])
					inputProbed = err == nil
					cacheInput.InputDigest = inputDigest
					cacheKey, cacheable, err = instructionCacheKey(cacheInput)
				}
			} else if len(inputDigestCandidates) != 0 {
				adapter := executor.nativeBuilder(current, builderOptions)
				inputDigest, err = applyCopyAddWithDigestContext(ctx, adapter, options.ContextDir, artifacts, operations[0])
				if err != nil {
					return "", Result{}, nil, fmt.Errorf("operation %d: %w", index+1, err)
				}
				inputApplied = true
				cacheInput.InputDigest = inputDigest
				cacheKey, cacheable, err = instructionCacheKey(cacheInput)
			} else {
				cacheKey, cacheable, err = instructionCacheKey(cacheInput)
			}
			if err != nil {
				return "", Result{}, nil, fmt.Errorf("operation %d cache key: %w", index+1, err)
			}
			if cacheable && finalFilesystemOperation {
				finalCacheKey = cacheKey
				finalCacheEligible = true
				finalCacheDirect = index == len(planned)-1
			}
			if cacheable && portableInstructionEligible(plannedOperation, operations[0], options.RunControls) {
				portableCacheKey, portableCacheable, err = portableInstructionKey(executor, cacheInput, cacheParent)
				if err != nil {
					return "", Result{}, nil, fmt.Errorf("operation %d portable cache key: %w", index+1, err)
				}
				if portableCacheable && finalFilesystemOperation {
					finalPortableCacheKey = portableCacheKey
					finalPortableCacheEligible = true
				}
			}
		}
		if cacheable && !options.NoCache {
			cacheEntry := instructionCacheEntry{}
			if prefetchedCacheEntry != nil {
				cacheEntry = *prefetchedCacheEntry
			} else {
				cacheEntry, err = findInstructionCacheEntry(store, cacheKey, options.CacheTTL)
				if err != nil {
					return "", Result{}, nil, fmt.Errorf("operation %d cache lookup: %w", index+1, err)
				}
			}
			localCacheHit := cacheEntry.ImageID != ""
			if cacheEntry.ImageID == "" && portableCacheable {
				cacheEntry, err = executor.instructionPortableCache.lookup(ctx, executor, portableCacheKey, cacheKey, builderOptions.SystemContext)
				if err != nil {
					return "", Result{}, nil, fmt.Errorf("operation %d portable cache lookup: %w", index+1, err)
				}
			}
			cachedImageID := cacheEntry.ImageID
			if captureRoot && cacheEntry.RootMetadata == nil {
				cachedImageID = ""
			}
			if cachedImageID != "" {
				if localCacheHit && portableCacheable {
					// Promote an ordinary containers/storage hit into the opt-in OCI
					// cache, but publish it only after the complete build succeeds.
					executor.instructionPortableCache.record(ctx, executor, portableCacheKey, cachedImageID, cacheEntry.ManifestDigest, cacheEntry.RootMetadata, builderOptions.SystemContext)
				}
				uncachedMetadata := cachedMetadataReplacement{
					OCIBase:     slices.Clone(current.OCIv1.History),
					DockerBase:  slices.Clone(current.Docker.History),
					Pending:     slices.Clone(current.PrependedEmptyLayers),
					Annotations: current.Annotations(),
				}
				if builderOptions.Format != define.OCIv1ImageManifest {
					// Docker schema 2 cannot retain manifest annotations.
					uncachedMetadata.Annotations = nil
				}
				if err := current.Delete(); err != nil {
					return "", Result{}, nil, fmt.Errorf("operation %d delete cache-replaced builder: %w", index+1, err)
				}
				selectedCacheBase, selectErr := selectedBuilderBase(ctx, store, cachedImageID, cacheEntry.ManifestDigest)
				if selectErr != nil {
					return "", Result{}, nil, fmt.Errorf("operation %d select instruction cache manifest %s: %w", index+1, cacheEntry.ManifestDigest, selectErr)
				}
				builderOptions.FromImage = selectedCacheBase
				builderOptions.PullPolicy = define.PullNever
				builderOptions.Container = executor.builderContainerName("cache")
				cacheBuilderOptions := builderOptions
				// Opening a checkpoint must not replace its base provenance with
				// the internal cache image's name and digest.
				cacheBuilderOptions.PreserveBaseImageAnns = true
				current, err = upstream.NewBuilder(ctx, store, cacheBuilderOptions)
				if err != nil {
					return "", Result{}, nil, fmt.Errorf("operation %d restore instruction cache %s: %w", index+1, cachedImageID, err)
				}
				current.FromImage = cachedImageID
				current.FromImageID = cachedImageID
				builderOptions.FromImage = cachedImageID
				currentManifest = cacheEntry.ManifestDigest
				if err := adoptCommittedConfig(ctx, store, cachedImageID, builderOptions.SystemContext, logical); err != nil {
					return "", Result{}, nil, fmt.Errorf("operation %d cached config: %w", index+1, err)
				}
				createdBy, _ := plannedOperationHistory(plannedOperation, operations)
				// Buildah assigns this annotation at commit. It describes the
				// cached instruction result, not an inherited caller input.
				if created, ok := current.Annotations()[v1.AnnotationCreated]; ok {
					if uncachedMetadata.Annotations == nil {
						uncachedMetadata.Annotations = make(map[string]string)
					}
					uncachedMetadata.Annotations[v1.AnnotationCreated] = created
				}
				cachedAnnotationsChanged = !maps.Equal(current.Annotations(), uncachedMetadata.Annotations)
				replaced, replaceErr := replaceCachedMetadata(current, uncachedMetadata, createdBy)
				if replaceErr != nil {
					return "", Result{}, nil, fmt.Errorf("operation %d cached history: %w", index+1, replaceErr)
				}
				if replaced {
					finalCachedImageID = ""
					finalCachedManifest = ""
					historyOnlyTail = true
				}
				if captureRoot {
					if err := restorePackageRootMetadata(store, current, cacheEntry.RootMetadata); err != nil {
						return "", Result{}, nil, fmt.Errorf("operation %d cached root metadata: %w", index+1, err)
					}
					currentRoot = cacheEntry.RootMetadata
				}
				cacheHit = true
				progress.cache(cachedImageID)
				if index < len(planned)-1 {
					progress.image(cachedImageID)
				}
				if finalFilesystemOperation && !replaced {
					finalCachedImageID = cachedImageID
					finalCachedManifest = cacheEntry.ManifestDigest
				}
			}
		}
		if cacheable && inputProbed && !cacheHit {
			adapter := executor.nativeBuilder(current, builderOptions)
			appliedDigest, applyErr := applyCopyAddWithDigestContext(ctx, adapter, options.ContextDir, artifacts, operations[0])
			if applyErr != nil {
				return "", Result{}, nil, fmt.Errorf("operation %d: %w", index+1, applyErr)
			}
			if appliedDigest != inputDigest {
				return "", Result{}, nil, fmt.Errorf("operation %d COPY/ADD input changed during cache probe: probed %s, applied %s", index+1, inputDigest, appliedDigest)
			}
			inputApplied = true
		}
		for _, operation := range operations {
			if operation == nil {
				return "", Result{}, nil, fmt.Errorf("operation %d is nil", index+1)
			}
			if !cacheHit && !inputApplied {
				if component, ok := operation.(componentGraphOperation); ok {
					replacement, err := executor.applyComponentOperation(ctx, current, &builderOptions, logical, rootBaseline, platform, component.planned, progress)
					if err != nil {
						return "", Result{}, nil, fmt.Errorf("operation %d: %w", index+1, err)
					}
					if replacement == nil {
						return "", Result{}, nil, fmt.Errorf("operation %d: component returned a nil builder", index+1)
					}
					current = replacement
				} else {
					adapter := executor.nativeBuilder(current, builderOptions)
					if preparedRun != nil {
						err = preparedRun.operation.apply(adapter, preparedRun.contextDir)
					} else {
						err = applyOperationWithContextContext(ctx, adapter, options.ContextDir, artifacts, operation)
					}
					if err != nil {
						return "", Result{}, nil, fmt.Errorf("operation %d: %w", index+1, err)
					}
				}
			}
			if err := trackConfigOperation(logical, operation); err != nil {
				return "", Result{}, nil, fmt.Errorf("operation %d config: %w", index+1, err)
			}
			if !cacheHit && linkedCopyOrAdd(operation) {
				pendingLinked = true
			}
		}
		if plannedOperation.Name == "component" {
			componentFilesystemChanged, err = componentBuilderHasFilesystemChanges(store, current)
			if err != nil {
				return "", Result{}, nil, fmt.Errorf("operation %d: %w", index+1, err)
			}
		}
		if !cacheHit {
			createdBy, record := plannedOperationHistory(plannedOperation, operations)
			switch {
			case !record:
			case options.Lifecycle.NoLayers:
				if index < len(planned)-1 {
					current.AddPrependedEmptyLayer(historyTimestamp(timestampPolicyFromOptions(options)), createdBy, "", "")
				} else {
					current.SetCreatedBy(createdBy)
				}
				historyOnlyTail = plannedFilesystemOperationCount(planned) == 0
			case isPlannedFilesystemOperation(plannedOperation) && componentFilesystemChanged:
				if pendingLinked {
					for linkedIndex := linkedLayerStart; linkedIndex < len(current.AppendedLinkedLayers); linkedIndex++ {
						current.AppendedLinkedLayers[linkedIndex].History.CreatedBy = createdBy
					}
				} else {
					current.SetCreatedBy(createdBy)
				}
				historyOnlyTail = false
			case isPlannedFilesystemOperation(plannedOperation):
				current.AddPrependedEmptyLayer(historyTimestamp(timestampPolicyFromOptions(options)), createdBy, "", "")
				historyOnlyTail = true
			case createdBy != "":
				current.AddPrependedEmptyLayer(historyTimestamp(timestampPolicyFromOptions(options)), createdBy, "", "")
				historyOnlyTail = true
				// A cached final filesystem instruction is only a base for this
				// metadata tail.  Exporting it directly would drop these entries.
				finalCachedImageID = ""
				finalCachedManifest = ""
			}
		}
		if cacheHit {
			// A filesystem snapshot may carry older output-only metadata.
			// Keep the current logical config authoritative for later operations.
			if err := syncBuilderConfig(current, logical); err != nil {
				return "", Result{}, nil, fmt.Errorf("operation %d cached config sync: %w", index+1, err)
			}
		}
		if preparedRun != nil && preparedRun.cleanup != nil {
			if err := preparedRun.cleanup(); err != nil {
				return "", Result{}, nil, fmt.Errorf("operation %d remove RUN context snapshot: %w", index+1, err)
			}
			preparedRun.cleanup = nil
		}
		if isPlannedFilesystemOperation(plannedOperation) {
			remainingFilesystem--
			checkpointForConfigTail := finalFilesystemOperation && finalCacheEligible && index < len(planned)-1
			checkpointForHistory := !options.Lifecycle.NoLayers && filesystemOperationNeedsHistoryCheckpoint(operations)
			if index < len(planned)-1 && !cacheHit && (cacheable || checkpointForHistory) {
				var replacement *upstream.Builder
				var checkpointID string
				var checkpointManifest digest.Digest
				var checkpointRoot *PackageRootMetadata
				if err := applyBuilderCheckpointControls(current, options.ImageControls, timestampPolicyFromOptions(options)); err != nil {
					return "", Result{}, nil, fmt.Errorf("checkpoint operation %d metadata: %w", index+1, err)
				}
				if captureRoot {
					replacement, checkpointID, checkpointManifest, checkpointRoot, err = checkpointPreservingPackageRootSelected(ctx, store, current, builderOptions, pendingLinked, timestampPolicyFromOptions(options), options.Lifecycle.KeepIntermediate)
				} else {
					replacement, checkpointID, checkpointManifest, err = checkpointSelected(ctx, store, current, builderOptions, pendingLinked, timestampPolicyFromOptions(options), options.Lifecycle.KeepIntermediate)
				}
				if err != nil {
					return "", Result{}, nil, fmt.Errorf("checkpoint operation %d: %w", index+1, err)
				}
				progress.image(checkpointID)
				current = replacement
				currentManifest = checkpointManifest
				cachedAnnotationsChanged = false
				if captureRoot {
					currentRoot = checkpointRoot
				}
				pendingLinked = false
				builderOptions.FromImage = checkpointID
				if err := adoptCommittedConfig(ctx, store, checkpointID, builderOptions.SystemContext, logical); err != nil {
					return "", Result{}, nil, fmt.Errorf("checkpoint operation %d config: %w", index+1, err)
				}
				if cacheable {
					if err := storeInstructionCacheEntrySelected(store, checkpointID, checkpointManifest, cacheKey, checkpointRoot); err != nil {
						return "", Result{}, nil, fmt.Errorf("checkpoint operation %d cache: %w", index+1, err)
					}
					if portableCacheable {
						executor.instructionPortableCache.record(ctx, executor, portableCacheKey, checkpointID, checkpointManifest, checkpointRoot, builderOptions.SystemContext)
					}
				}
				if checkpointForConfigTail {
					finalCachedImageID = checkpointID
					finalCachedManifest = checkpointManifest
				}
			}
		}
	}
	var err error
	if captureRoot {
		root, retErr = capturePackageRootMetadata(store, current)
		if retErr != nil {
			return "", Result{}, nil, retErr
		}
		currentRoot = root
	}
	// Buildah/Podman define --timestamp as having no image effect for a stage
	// whose only instruction is FROM. SOURCE_DATE_EPOCH still rewrites the
	// output image's creation metadata.
	unchangedBase := len(planned) == 0 && current.FromImageID != "" && options.SourceDateEpoch == nil && !options.RewriteTimestamp && !options.ImageControls.HasChanges()
	if output {
		var committed Result
		var metadataProvenance []byte
		var err error
		if unchangedBase {
			selected := optionalDigest(baseManifest)
			compatible, compatibilityErr := storedImageMatchesOutputFormat(ctx, store, current.FromImageID, options.Output.Format, builderOptions.SystemContext, selected)
			if compatibilityErr != nil {
				return "", Result{}, nil, fmt.Errorf("inspect unchanged base image format: %w", compatibilityErr)
			}
			if compatible {
				committed, err = exportStoredImageVariantRaw(ctx, store, current.FromImageID, options.Output, builderOptions.SystemContext, selected)
				if errors.Is(err, errRetainedStorageBlobUnavailable) {
					committed, err = copyStoredOutputSelected(ctx, store, current.FromImageID, options.Output, builderOptions.SystemContext, selected)
				}
				if err == nil {
					err = oci.SetLayoutCreatedAnnotation(ctx, committed.Layout, options.SourceDateEpoch)
				}
				if err != nil {
					return "", Result{}, nil, err
				}
				committed, err = executor.finalizeOutput(ctx, committed, originalBaseID, builderOptions, logical)
				return committed.ImageID, committed, root, err
			}
		}
		if historyOnlyTail {
			// Buildah otherwise annotates the first metadata history entry with
			// the internal instruction-cache image name used as the base.
			current.FromImage = current.FromImageID
		}
		preserveMetadataBase := historyOnlyTail && !cachedAnnotationsChanged && current.FromImageID != "" && currentManifest != "" && options.Timestamp == nil && options.SourceDateEpoch == nil && !options.RewriteTimestamp && !options.ImageControls.HasChanges()
		if preserveMetadataBase {
			filesystemChanged, changeErr := componentBuilderHasFilesystemChanges(store, current)
			if changeErr != nil {
				return "", Result{}, nil, fmt.Errorf("inspect metadata-only filesystem changes: %w", changeErr)
			}
			preserveMetadataBase = !filesystemChanged
		}
		if preserveMetadataBase {
			selected := optionalDigest(currentManifest)
			compatible, compatibilityErr := storedImageMatchesOutputFormat(ctx, store, current.FromImageID, options.Output.Format, builderOptions.SystemContext, selected)
			if compatibilityErr != nil && !errors.Is(compatibilityErr, os.ErrNotExist) {
				return "", Result{}, nil, fmt.Errorf("inspect metadata-only base image format: %w", compatibilityErr)
			}
			if compatibilityErr == nil && compatible {
				if options.Output.DisableCompression {
					committed, err = exportStoredImageVariantRaw(ctx, store, current.FromImageID, options.Output, builderOptions.SystemContext, selected)
				} else {
					committed, err = copyStoredOutputSelected(ctx, store, current.FromImageID, options.Output, builderOptions.SystemContext, selected)
				}
				if errors.Is(err, errRetainedStorageBlobUnavailable) {
					committed = Result{}
					err = nil
				} else if err == nil {
					if builderOptions.Format == define.Dockerv2ImageManifest {
						provenance := current.Docker
						provenance.History = append(slices.Clone(provenance.History), dockerHistory(current.PrependedEmptyLayers)...)
						metadataProvenance, err = json.Marshal(provenance)
					} else {
						provenance := current.OCIv1
						provenance.History = append(slices.Clone(provenance.History), current.PrependedEmptyLayers...)
						metadataProvenance, err = json.Marshal(provenance)
					}
				}
			}
		}
		if committed.Layout != "" {
			// The logical configuration is reconciled below while the exact
			// filesystem descriptors from the selected base remain untouched.
		} else if finalCachedImageID != "" {
			committed, err = copyStoredOutputSelected(ctx, store, finalCachedImageID, options.Output, builderOptions.SystemContext, optionalDigest(finalCachedManifest))
		} else if finalCacheEligible && finalCacheDirect {
			var storedImageID string
			var storedManifest digest.Digest
			storedImageID, storedManifest, err = commitStoredSnapshotSelected(ctx, current, builderOptions.SystemContext, builderOptions.Format, pendingLinked || historyOnlyTail, timestampPolicyFromOptions(options))
			if err == nil {
				err = storeInstructionCacheEntrySelected(store, storedImageID, storedManifest, finalCacheKey, currentRoot)
			}
			if err == nil && finalPortableCacheEligible {
				executor.instructionPortableCache.record(ctx, executor, finalPortableCacheKey, storedImageID, storedManifest, currentRoot, builderOptions.SystemContext)
			}
			if err == nil {
				committed, err = copyStoredOutputSelected(ctx, store, storedImageID, options.Output, builderOptions.SystemContext, optionalDigest(storedManifest))
			}
		} else {
			committed, err = commitOutput(ctx, store, current, options.Output, builderOptions.SystemContext, pendingLinked || historyOnlyTail, timestampPolicyFromOptions(options), options.ImageControls)
		}
		if err != nil {
			return "", Result{}, nil, err
		}
		committed, err = executor.reconcileOutputConfig(ctx, committed, builderOptions.SystemContext, logical, historyOnlyTail, metadataProvenance)
		if err != nil {
			return "", Result{}, nil, err
		}
		committed, err = executor.finalizeOutput(ctx, committed, originalBaseID, builderOptions, logical)
		return committed.ImageID, committed, root, err
	}
	if unchangedBase {
		return current.FromImageID, Result{ManifestDigest: baseManifest.String()}, root, nil
	}
	if finalCachedImageID != "" && finalCacheDirect {
		imageID = finalCachedImageID
		result.ManifestDigest = finalCachedManifest.String()
	} else if finalCacheEligible && finalCacheDirect {
		var manifestDigest digest.Digest
		imageID, manifestDigest, err = commitStoredSnapshotSelected(ctx, current, builderOptions.SystemContext, builderOptions.Format, pendingLinked || historyOnlyTail, timestampPolicyFromOptions(options))
		if err == nil {
			err = storeInstructionCacheEntrySelected(store, imageID, manifestDigest, finalCacheKey, currentRoot)
		}
		if err == nil && finalPortableCacheEligible {
			executor.instructionPortableCache.record(ctx, executor, finalPortableCacheKey, imageID, manifestDigest, currentRoot, builderOptions.SystemContext)
		}
		result.ManifestDigest = manifestDigest.String()
	} else {
		commitOptions := upstream.CommitOptions{
			PreferredManifestType: builderOptions.Format,
			EmptyLayerIfEmptyDiff: keepEmptyFilesystemLayer(builderOptions.Format),
			OmitLayerHistoryEntry: pendingLinked || historyOnlyTail,
			SystemContext:         builderOptions.SystemContext,
		}
		timestampPolicyFromOptions(options).apply(&commitOptions)
		var manifestDigest digest.Digest
		imageID, _, manifestDigest, err = current.Commit(ctx, nil, commitOptions)
		result.ManifestDigest = manifestDigest.String()
	}
	if err != nil {
		return "", Result{}, nil, fmt.Errorf("commit intermediate stage: %w", err)
	}
	if err := adoptCommittedConfig(ctx, store, imageID, builderOptions.SystemContext, logical); err != nil {
		return "", Result{}, nil, fmt.Errorf("reconcile intermediate stage config: %w", err)
	}
	return imageID, result, root, nil
}

// plannedOperationHistory renders stable, user-facing history in the same
// instruction-oriented style used by modern Docker/BuildKit images.  Runtime
// injected proxy values are deliberately absent: only planner-visible ARGs
// can appear in a RUN prefix.
func plannedOperationHistory(operation planner.Operation, lowered []Operation) (string, bool) {
	name := strings.ToUpper(operation.Name)
	if name == "" {
		return "", false
	}
	if name == "RUN" {
		command := historyInstructionArguments(operation.Instruction)
		if len(operation.ArgumentsInScope) == 0 {
			return name + " " + command, true
		}
		names := slices.Sorted(maps.Keys(operation.ArgumentsInScope))
		var prefix strings.Builder
		fmt.Fprintf(&prefix, "RUN |%d", len(names))
		for _, argument := range names {
			fmt.Fprintf(&prefix, " %s=%s", argument, operation.ArgumentsInScope[argument])
		}
		return prefix.String() + " " + command, true
	}
	if len(lowered) == 1 {
		switch value := lowered[0].(type) {
		case Cmd:
			return name + " " + historyJSON([]string(value)), true
		case Entrypoint:
			return name + " " + historyJSON([]string(value)), true
		case Shell:
			return name + " " + historyJSON([]string(value)), true
		case WorkDir:
			return name + " " + string(value), true
		}
	}
	arguments := historyInstructionArguments(operation.Instruction)
	if arguments == "" {
		return name, true
	}
	return name + " " + arguments, true
}

func historyInstructionArguments(instruction definition.Instruction) string {
	if instruction.Name == "arg" && len(instruction.Arguments) > 0 {
		if len(instruction.Arguments) == 1 {
			return instruction.Arguments[0]
		}
		return instruction.Arguments[0] + "=" + instruction.Arguments[1]
	}
	if instruction.Name == "env" || instruction.Name == "label" {
		values := make(map[string]string, len(instruction.Properties)+len(instruction.Arguments)/2)
		for name, value := range instruction.Properties {
			values[name] = value
		}
		for index := 0; index+1 < len(instruction.Arguments); index += 2 {
			values[instruction.Arguments[index]] = instruction.Arguments[index+1]
		}
		fields := make([]string, 0, len(values))
		for _, name := range slices.Sorted(maps.Keys(values)) {
			fields = append(fields, name+"="+values[name])
		}
		return strings.Join(fields, " ")
	}
	var fields []string
	for _, name := range slices.Sorted(maps.Keys(instruction.Properties)) {
		fields = append(fields, "--"+name+"="+instruction.Properties[name])
	}
	for _, child := range instruction.Children {
		if child.Name == "exclude" || child.Name == "device" {
			for _, argument := range child.Arguments {
				value := argument
				if child.Name == "device" {
					if required, exists := child.Properties["required"]; exists {
						value += ",required=" + required
					}
				}
				fields = append(fields, "--"+child.Name+"="+value)
			}
			continue
		}
		if child.Name != "mount" || len(child.Arguments) != 1 {
			continue
		}
		parts := []string{"type=" + child.Arguments[0]}
		for _, name := range slices.Sorted(maps.Keys(child.Properties)) {
			parts = append(parts, name+"="+child.Properties[name])
		}
		fields = append(fields, "--mount="+strings.Join(parts, ","))
	}
	if instruction.Form == "exec" {
		fields = append(fields, historyJSON(instruction.Arguments))
	} else {
		fields = append(fields, instruction.Arguments...)
	}
	return strings.Join(fields, " ")
}

func historyJSON(arguments []string) string {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

func lowerPlannedGraphOperation(operation planner.Operation, aliases map[string]string, images map[string]stageState, store storage.Store, platform string, selected map[ResolvedBaseKey]ResolvedImageSource, logical *imageconfig.Config, cacheMountID CacheMountIDResolver) ([]Operation, error) {
	shell, err := logical.Shell([]string{"/bin/sh", "-c"})
	if err != nil {
		return nil, fmt.Errorf("select shell: %w", err)
	}
	resolved := make(map[ResolvedBaseKey]ResolvedImageSource)
	for key, source := range selected {
		if key.Platform == platform {
			resolved[key] = source
		}
	}
	return lowerGraphOperationsWithSelectedImages([]planner.Operation{operation}, aliases, images, store, shell, cacheMountID, resolved)
}

func adoptCommittedConfig(ctx context.Context, store storage.Store, imageID string, system *types.SystemContext, logical *imageconfig.Config) error {
	emitted, err := packageImageConfig(ctx, store, imageID, system)
	if err != nil {
		return err
	}
	image, err := store.Image(imageID)
	if err != nil {
		return fmt.Errorf("inspect executor image layers: %w", err)
	}
	emitted, err = normalizeZeroLayerExecutorConfig(emitted, image.TopLayer == "")
	if err != nil {
		return err
	}
	return logical.AdoptExecutorProvenance(emitted)
}

func normalizeZeroLayerExecutorConfig(raw []byte, provenZeroLayer bool) ([]byte, error) {
	if !provenZeroLayer {
		return raw, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("decode executor image configuration: %w", err)
	}
	rootRaw, exists := root["rootfs"]
	if !exists {
		return raw, nil
	}
	var rootFS map[string]json.RawMessage
	if err := json.Unmarshal(rootRaw, &rootFS); err != nil {
		return raw, nil
	}
	if _, exists := rootFS["diff_ids"]; exists {
		return raw, nil
	}
	var rootType string
	if err := json.Unmarshal(rootFS["type"], &rootType); err != nil || rootType != "layers" {
		return raw, nil
	}
	var history []v1.History
	if historyRaw, exists := root["history"]; exists {
		if err := json.Unmarshal(historyRaw, &history); err != nil {
			return raw, nil
		}
	}
	for _, entry := range history {
		if !entry.EmptyLayer {
			return raw, nil
		}
	}
	empty, err := json.Marshal([]digest.Digest{})
	if err != nil {
		return nil, err
	}
	rootFS["diff_ids"] = empty
	root["rootfs"], err = json.Marshal(rootFS)
	if err != nil {
		return nil, err
	}
	return json.Marshal(root)
}

func layoutImageHasNoLayers(layout string) (bool, error) {
	root, err := oci.LayoutRoot(layout)
	if err != nil {
		return false, err
	}
	manifestData, err := os.ReadFile(filepath.Join(layout, "blobs", root.Digest.Algorithm().String(), root.Digest.Encoded()))
	if err != nil {
		return false, err
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return false, err
	}
	if manifest.SchemaVersion != 2 || manifest.Config.Digest == "" {
		return false, errors.New("committed image manifest is invalid")
	}
	return len(manifest.Layers) == 0, nil
}

type cachedMetadataReplacement struct {
	OCIBase     []v1.History
	DockerBase  []buildahdocker.V2S2History
	Pending     []v1.History
	Annotations map[string]string
}

// replaceCachedMetadata retains the cached filesystem instruction but
// reconstructs its prefix and annotations from the current, uncached builder. The
// instruction cache intentionally excludes output-only metadata, so a warm
// hit must support metadata being added, removed, or reordered without
// inheriting stale history from the image that originally populated the cache.
func replaceCachedMetadata(builder *upstream.Builder, replacement cachedMetadataReplacement, createdBy string) (bool, error) {
	if builder == nil {
		return false, nil
	}
	if len(builder.OCIv1.History) == 0 || len(builder.Docker.History) == 0 {
		return false, errors.New("cached image history is missing its filesystem instruction")
	}
	ociOperation := builder.OCIv1.History[len(builder.OCIv1.History)-1]
	dockerOperation := builder.Docker.History[len(builder.Docker.History)-1]
	// An instruction such as RUN test may leave the filesystem unchanged.
	// Validate its identity instead of requiring it to contribute a layer.
	if createdBy == "" || ociOperation.CreatedBy != createdBy || dockerOperation.CreatedBy != createdBy || ociOperation.EmptyLayer != dockerOperation.EmptyLayer {
		return false, fmt.Errorf("cached image history does not end with the expected instruction %q", createdBy)
	}

	wantOCI := append(slices.Clone(replacement.OCIBase), replacement.Pending...)
	wantOCI = append(wantOCI, ociOperation)
	wantDocker := append(slices.Clone(replacement.DockerBase), dockerHistory(replacement.Pending)...)
	wantDocker = append(wantDocker, dockerOperation)
	historyChanged := !slices.EqualFunc(builder.OCIv1.History, wantOCI, equalOCIHistory)
	if builder.Format == define.Dockerv2ImageManifest {
		// The other format's nil/zero timestamp conversion is not a change
		// to the history represented by this image.
		historyChanged = !slices.Equal(builder.Docker.History, wantDocker)
	}
	changed := historyChanged || !maps.Equal(builder.Annotations(), replacement.Annotations)
	if !changed {
		return false, nil
	}
	builder.OCIv1.History = wantOCI
	builder.Docker.History = wantDocker
	builder.ClearAnnotations()
	for key, value := range replacement.Annotations {
		builder.SetAnnotation(key, value)
	}
	if err := builder.Save(); err != nil {
		return false, fmt.Errorf("save restored cached metadata: %w", err)
	}
	return true, nil
}

func preserveLogicalCreated(emitted []byte, logical *imageconfig.Config) ([]byte, error) {
	if logical == nil {
		return emitted, nil
	}
	logicalRaw, err := logical.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var source map[string]json.RawMessage
	if err := json.Unmarshal(logicalRaw, &source); err != nil {
		return nil, err
	}
	created, exists := source["created"]
	if !exists {
		return emitted, nil
	}
	var target map[string]json.RawMessage
	if err := json.Unmarshal(emitted, &target); err != nil {
		return nil, err
	}
	target["created"] = slices.Clone(created)
	return json.Marshal(target)
}

func dockerHistory(history []v1.History) []buildahdocker.V2S2History {
	converted := make([]buildahdocker.V2S2History, 0, len(history))
	for _, entry := range history {
		var created time.Time
		if entry.Created != nil {
			created = *entry.Created
		}
		converted = append(converted, buildahdocker.V2S2History{
			Created: created, CreatedBy: entry.CreatedBy, Author: entry.Author,
			Comment: entry.Comment, EmptyLayer: entry.EmptyLayer,
		})
	}
	return converted
}

func equalOCIHistory(left, right v1.History) bool {
	if left.CreatedBy != right.CreatedBy || left.Author != right.Author || left.Comment != right.Comment || left.EmptyLayer != right.EmptyLayer {
		return false
	}
	if left.Created == nil || right.Created == nil {
		return left.Created == nil && right.Created == nil
	}
	return left.Created.Equal(*right.Created)
}

func filesystemOperationNeedsHistoryCheckpoint(operations []Operation) bool {
	for _, operation := range operations {
		switch operation.(type) {
		case Run, WorkDir, Copy, Add, copyFromImageOperation, componentGraphOperation:
			return true
		}
	}
	return false
}

func trackConfigOperation(logical *imageconfig.Config, operation Operation) error {
	var instruction definition.Instruction
	switch value := operation.(type) {
	case Env:
		instruction = definition.Instruction{Name: "env", Arguments: []string{value.Name, value.Value}}
	case Label:
		instruction = definition.Instruction{Name: "label", Arguments: []string{value.Name, value.Value}}
	case WorkDir:
		instruction = definition.Instruction{Name: "workdir", Arguments: []string{string(value)}}
	case User:
		instruction = definition.Instruction{Name: "user", Arguments: []string{string(value)}}
	case Cmd:
		instruction = definition.Instruction{Name: "cmd", Form: "exec", Arguments: slices.Clone([]string(value))}
	case Entrypoint:
		instruction = definition.Instruction{Name: "entrypoint", Form: "exec", Arguments: slices.Clone([]string(value))}
	case Shell:
		instruction = definition.Instruction{Name: "shell", Arguments: slices.Clone([]string(value))}
	case StopSignal:
		instruction = definition.Instruction{Name: "stopsignal", Arguments: []string{string(value)}}
	case Expose:
		instruction = definition.Instruction{Name: "expose", Arguments: []string{string(value)}}
	case Volume:
		instruction = definition.Instruction{Name: "volume", Arguments: []string{string(value)}}
	case Maintainer:
		instruction = definition.Instruction{Name: "maintainer", Arguments: []string{string(value)}}
	case Healthcheck:
		instruction = healthcheckInstruction(value)
	case OnBuild:
		instruction = definition.Instruction{Name: "onbuild", Arguments: []string{string(value)}}
	default:
		return nil
	}
	return logical.Apply(instruction)
}

func capturePackageRootMetadata(store storage.Store, builder *upstream.Builder) (metadata *PackageRootMetadata, retErr error) {
	container, err := store.Container(builder.ContainerID)
	if err != nil {
		return nil, fmt.Errorf("inspect package builder container: %w", err)
	}
	layer, err := store.Layer(container.LayerID)
	if err != nil {
		return nil, fmt.Errorf("inspect package builder layer: %w", err)
	}
	mountPoint, err := builder.Mount(builder.MountLabel)
	if err != nil {
		return nil, fmt.Errorf("mount package builder rootfs: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, builder.Unmount())
	}()
	var storageLabel []byte
	if builder.MountLabel == "" {
		// The graphroot is outside the mutable image and supplies the host
		// context inherited when SELinux is unavailable inside a container.
		storageLabel, err = system.Lgetxattr(store.GraphRoot(), "security.selinux")
		if err != nil && !errors.Is(err, system.ENOTSUP) && !errors.Is(err, system.ErrNotSupportedPlatform) {
			return nil, fmt.Errorf("read storage SELinux context: %w", err)
		}
	}
	ambientSELinux := storageAmbientSELinux(builder.MountLabel, storageLabel)
	ambientSELinux, err = mountedAmbientSELinux(mountPoint, store.GraphRoot(), ambientSELinux)
	if err != nil {
		return nil, err
	}
	metadata, err = packageRootMetadataFromMount(mountPoint, layer.UIDMap, layer.GIDMap, builder.MountLabel, ambientSELinux)
	if err != nil {
		return nil, fmt.Errorf("read package builder root metadata: %w", err)
	}
	return metadata, nil
}

func plannedFilesystemOperationCount(operations []planner.Operation) int {
	count := 0
	for _, operation := range operations {
		if isPlannedFilesystemOperation(operation) {
			count++
		}
	}
	return count
}

func isPlannedFilesystemOperation(operation planner.Operation) bool {
	switch operation.Name {
	case "run", "copy", "add", "workdir", "component":
		return true
	default:
		return false
	}
}
