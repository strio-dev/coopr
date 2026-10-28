package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"coopr/internal/buildcontext"
	"coopr/internal/cache"
	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"coopr/internal/stateidentity"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	orasoci "oras.land/oras-go/v2/content/oci"
)

type componentResolutionState struct {
	identity digest.Digest
	variants map[string]*ResolvedComponentPlan
}

func componentSelectionKey(reference string, target v1.Platform) (string, error) {
	platform, err := componentPlatformString(target)
	if err != nil {
		return "", err
	}
	selectionData, err := json.Marshal([]string{reference, platform})
	if err != nil {
		return "", fmt.Errorf("encode component selection key: %w", err)
	}
	return string(selectionData), nil
}

// resolveComponentInvocation memoizes the immutable selection separately from
// parameterized plans. Repeated calls reuse the frozen root, platform and
// delivery selection; changing caller parameters never reselects mutable
// destination records.
func (executor *graphExecutor) resolveComponentInvocation(ctx context.Context, reference string, parameters map[string]string, target v1.Platform, progressPrefix string, environment map[string]string, scopes ...*oci.DeliveryScope) (*ResolvedComponentPlan, error) {
	var scope *oci.DeliveryScope
	if len(scopes) != 0 {
		scope = scopes[0].Clone()
	}
	selectionKey, err := scopedComponentSelectionKey(reference, parameters, target, scope)
	if err != nil {
		return nil, err
	}
	normalized, err := planner.NormalizeParameters(parameters)
	if err != nil {
		return nil, fmt.Errorf("normalize component parameters: %w", err)
	}
	variantData, err := json.Marshal(struct {
		Parameters  json.RawMessage
		Environment map[string]string
	}{normalized, environment})
	if err != nil {
		return nil, fmt.Errorf("normalize component caller environment: %w", err)
	}
	variantKey := string(variantData)
	state := executor.resolvedComponents[selectionKey]
	if state != nil {
		if resolved := state.variants[variantKey]; resolved != nil {
			if err := executor.retainComponentBases(reference, resolved); err != nil {
				return nil, err
			}
			return resolved, nil
		}
	}
	resolve := executor.resolveComponent
	if resolve == nil {
		resolve = ResolveComponentPlan
	}
	if executor.componentSelectionHandler != nil {
		reply, err := executor.componentSelectionHandler(ctx, componentSelectionRequest{DeliveryScope: scope, Reference: reference, Parameters: maps.Clone(parameters), Platform: target, ProgressPrefix: progressPrefix, CallerEnvironment: maps.Clone(environment), ActiveComponents: maps.Clone(executor.activeComponents), ActiveLocalComponents: maps.Clone(executor.activeLocalComponents)})
		if err != nil {
			return nil, fmt.Errorf("select component %q: %w", reference, err)
		}
		if err := executor.adoptComponentContinuation(componentReturnReply{ResolvedBases: reply.ResolvedBases, ComponentPins: reply.ComponentPins, ComponentSources: reply.ComponentSources, ComponentSelections: reply.ComponentSelections}, nil, nil); err != nil {
			return nil, err
		}
		if executor.componentPins[selectionKey] == "" {
			return nil, fmt.Errorf("component %q was not pinned by coordinator", reference)
		}
		if _, ok := executor.componentSelections[selectionKey]; !ok {
			return nil, fmt.Errorf("component %q has no frozen coordinator selection", reference)
		}
	}
	resolveReference := reference
	if pinned := executor.componentPins[selectionKey]; pinned != "" {
		resolveReference = pinned
	}
	local := definition.IsLocalComponentReference(reference)
	localSource := executor.componentSources[selectionKey]
	if local && resolveReference == reference {
		if executor.componentSelectionHandler != nil {
			return nil, fmt.Errorf("local component %q was not frozen before stage execution", reference)
		}
		resolveReference, localSource, err = executor.materializeLocalComponent(ctx, reference, parameters, target, progressPrefix)
		if err != nil {
			return nil, err
		}
	}
	if local && localSource == "" {
		return nil, fmt.Errorf("local component %q has no frozen source identity", reference)
	}
	var selection *oci.ComponentSelection
	if frozen, ok := executor.componentSelections[selectionKey]; ok {
		copy := frozen.Clone()
		selection = &copy
	}
	resolved, err := resolve(ctx, ComponentPlanRequest{
		Resolver: executor.options.Resolver, Reference: resolveReference, Selection: selection, DeliveryScope: scope,
		Parameters: maps.Clone(parameters), Platform: target, LocalParameters: local,
		CallerEnvironment: maps.Clone(environment),
		DeferImageSource:  deferredPolicySource(executor.sourcePolicy), ResolveBase: executor.resolveBaseImage, TransientRunMounts: executor.options.TransientRunMounts,
		BuildContexts: executor.options.BuildContexts,
		ResolveContext: func(ctx context.Context, spec buildcontext.Spec, target v1.Platform) (ResolvedImageSource, error) {
			platform, err := componentPlatformString(target)
			if err != nil {
				return ResolvedImageSource{}, err
			}
			key := graphNamedContextKey(spec.Name, platform)
			if selected, found := executor.resolvedBases[key]; found {
				return selected, nil
			}
			return executor.materializeNamedContext(ctx, spec, target)
		},
	})
	if err != nil {
		return nil, err
	}
	if localSource != "" {
		if executor.componentSources == nil {
			executor.componentSources = make(map[string]string)
		}
		executor.componentSources[selectionKey] = localSource
	}
	resolved.localSourceIdentity = localSource
	if resolved.Artifact != nil {
		frozen := resolved.Artifact.ComponentSelection()
		if selection != nil && selection.Identity() != frozen.Identity() {
			return nil, fmt.Errorf("component reference %q changed frozen selection during build", reference)
		}
		if executor.componentSelections == nil {
			executor.componentSelections = make(map[string]oci.ComponentSelection)
		}
		executor.componentSelections[selectionKey] = frozen
		pin, err := pinnedComponentReference(resolved)
		if err != nil {
			return nil, err
		}
		if previous, ok := executor.componentPins[selectionKey]; ok && previous != pin {
			return nil, fmt.Errorf("component reference %q changed pinned root during build", reference)
		}
		if executor.componentPins == nil {
			executor.componentPins = make(map[string]string)
		}
		executor.componentPins[selectionKey] = pin
	}
	if err := executor.retainComponentBases(reference, resolved); err != nil {
		return nil, err
	}
	if state == nil {
		state = &componentResolutionState{identity: resolved.Identity, variants: make(map[string]*ResolvedComponentPlan)}
		if executor.resolvedComponents == nil {
			executor.resolvedComponents = make(map[string]*componentResolutionState)
		}
		executor.resolvedComponents[selectionKey] = state
	} else if state.identity != resolved.Identity {
		return nil, fmt.Errorf("component reference %q changed digest during build: %s then %s", reference, state.identity, resolved.Identity)
	}
	state.variants[variantKey] = resolved
	return resolved, nil
}

func pinnedComponentReference(resolved *ResolvedComponentPlan) (string, error) {
	if resolved == nil || resolved.Artifact == nil || resolved.Identity == "" {
		return "", errors.New("resolved component has no immutable artifact identity")
	}
	if resolved.Artifact.Root.Digest.Validate() != nil {
		return "", errors.New("resolved component has no immutable root identity")
	}
	if resolved.Artifact.Local || resolved.Artifact.Repository == "local" {
		return resolved.Artifact.Root.Digest.String(), nil
	}
	if resolved.Artifact.Repository == "" {
		return "", errors.New("resolved component has no repository")
	}
	return resolved.Artifact.Repository + "@" + resolved.Artifact.Root.Digest.String(), nil
}

// prepareIsolatedStageComponents freezes immediate artifact selections without
// instantiating EXTEND. Its caller environment exists only at execution time;
// nested calls and invocation-only bases are selected at that boundary.
func (executor *graphExecutor) prepareIsolatedStageComponents(ctx context.Context, prepared *preparedGraphStage) error {
	if prepared == nil {
		return errors.New("prepared stage is nil")
	}
	target, err := componentPlatformFromString(prepared.stage.Platform)
	if err != nil {
		return err
	}
	pins := maps.Clone(executor.componentPins)
	sources := maps.Clone(executor.componentSources)
	selections := cloneComponentSelections(executor.componentSelections)
	if selections == nil {
		selections = make(map[string]oci.ComponentSelection)
	}
	if sources == nil {
		sources = make(map[string]string)
	}
	if pins == nil {
		pins = make(map[string]string)
	}
	for _, operation := range prepared.operations {
		if operation.Name != "component" {
			continue
		}
		if len(operation.Arguments) != 1 || operation.Arguments[0] == "" {
			return errors.New("component invocation requires one reference")
		}
		reference := operation.Arguments[0]
		parameters, err := operation.ComponentArguments()
		if err != nil {
			return err
		}
		scope, err := componentOperationDeliveryScope(prepared.delivery, operation)
		if err != nil {
			return err
		}
		key, err := scopedComponentSelectionKey(reference, parameters, target, scope)
		if err != nil {
			return err
		}
		if pins[key] != "" || executor.componentSelectionHandler != nil {
			// Isolated nested graphs ask the ancestor at actual invocation, when
			// caller ENV is known. They never look up mutable artifacts here.
			continue
		}
		resolveReference := reference
		if definition.IsLocalComponentReference(reference) {
			var identity string
			resolveReference, identity, err = executor.materializeLocalComponent(ctx, reference, parameters, target, prepared.progressPrefix)
			sources[key] = identity
			if err != nil {
				return err
			}
		}
		if executor.options.Resolver == nil {
			return errors.New("component selection requires a Coopr component resolver")
		}
		artifact, err := executor.options.Resolver.ResolveComponent(ctx, resolveReference, target, nil, scope)
		if err != nil {
			return fmt.Errorf("freeze component %q: %w", reference, err)
		}
		if artifact.Component == nil || artifact.Selected.Digest.Algorithm() != digest.SHA256 || artifact.Selected.Digest.Validate() != nil {
			return fmt.Errorf("component %q has no verified immutable publication", reference)
		}
		resolved := &ResolvedComponentPlan{Artifact: artifact, Identity: artifact.Selected.Digest}
		pinned, err := pinnedComponentReference(resolved)
		if err != nil {
			return err
		}
		state := executor.resolvedComponents[key]
		if state != nil && state.identity != resolved.Identity {
			return fmt.Errorf("component reference %q changed immutable selection", reference)
		}
		if state == nil {
			if executor.resolvedComponents == nil {
				executor.resolvedComponents = make(map[string]*componentResolutionState)
			}
			executor.resolvedComponents[key] = &componentResolutionState{identity: resolved.Identity, variants: make(map[string]*ResolvedComponentPlan)}
		}
		pins[key] = pinned
		selections[key] = artifact.ComponentSelection()
	}
	prepared.componentPins = pins
	prepared.componentSources = sources
	prepared.componentSelections = selections
	if executor.componentSelectionHandler == nil {
		executor.componentPins = maps.Clone(pins)
		executor.componentSources = maps.Clone(sources)
		executor.componentSelections = cloneComponentSelections(selections)
	}
	return nil
}

// applyComponentOperation executes one immutable component invocation against
// the caller state at this exact source position, then adopts the selected whole output image
// as the continuation base. The caller builder remains valid on error.
func (executor *graphExecutor) applyComponentOperation(ctx context.Context, caller *upstream.Builder, builderOptions *upstream.BuilderOptions, logical *imageconfig.Config, platform v1.Platform, operation planner.Operation, progress stageProgress, adoption ...componentAdoptionContext) (_ *upstream.Builder, retErr error) {
	if executor.options.Resolver == nil || executor.packageImport == nil {
		return nil, errors.New("component invocation requires a Coopr component resolver")
	}
	if caller == nil || builderOptions == nil || logical == nil {
		return nil, errors.New("component invocation requires caller builder, builder options, and logical config")
	}
	if len(operation.Arguments) != 1 || operation.Arguments[0] == "" {
		return nil, errors.New("component invocation requires one reference")
	}

	parameters, err := operation.ComponentArguments()
	if err != nil {
		return nil, err
	}
	environment, err := imageEnvironment(logical)
	if err != nil {
		return nil, fmt.Errorf("component caller environment: %w", err)
	}
	var delivery *oci.ComponentDelivery
	if len(adoption) != 0 {
		delivery = adoption[0].delivery
	}
	scope, err := componentOperationDeliveryScope(delivery, operation)
	if err != nil {
		return nil, err
	}
	resolved, err := executor.resolveComponentInvocation(ctx, operation.Arguments[0], parameters, platform, progress.prefix, environment, scope)
	if err != nil {
		return nil, err
	}
	extendsCaller := componentOutputExtendsCaller(resolved.Plan)
	release, err := executor.enterLocalComponent(resolved.localSourceIdentity)
	if err != nil {
		return nil, err
	}
	defer release()
	effectivePlan := *resolved.Plan
	effectivePlan.Stages, err = resolveAndValidateBuildNetwork(resolved.Plan.Stages, executor.options.Network, executor.options.RunControls, executor.options.Isolation)
	if err != nil {
		return nil, fmt.Errorf("component %s: %w", resolved.Identity, err)
	}
	effectiveResolved := *resolved
	effectiveResolved.Plan = &effectivePlan
	resolved = &effectiveResolved
	if executor.activeComponents[resolved.Identity] {
		return nil, fmt.Errorf("component invocation cycle at %s", resolved.Identity)
	}
	executor.activeComponents[resolved.Identity] = true
	defer delete(executor.activeComponents, resolved.Identity)

	localNames := make(map[string]bool, len(resolved.Plan.Stages))
	for _, stage := range resolved.Plan.Stages {
		localNames[stage.ID] = true
		if stage.Name != "" {
			localNames[strings.ToLower(stage.Name)] = true
		}
	}
	for _, stage := range resolved.Plan.Stages {
		if stage.Kind != "from" || stage.Source == "" || strings.EqualFold(stage.Source, "scratch") || localNames[strings.ToLower(stage.Source)] {
			continue
		}
		if stage.DeferredImageSource {
			// The executor selects and pins this source after its producers finish.
			continue
		}
		key := ResolvedBaseKey{Reference: stage.Source, Platform: stage.Platform}
		if stage.SourceContext != "" {
			key = graphNamedContextKey(stage.SourceContext, stage.Platform)
		}
		selected, selectedHere := resolved.SelectedBases[key]
		if !selectedHere {
			return nil, fmt.Errorf("component %s external base %q was not selected during planning", resolved.Identity, stage.Source)
		}
		pinned, found := executor.resolvedBases[key]
		if !found || pinned.ImageID != selected.ImageID || pinned.Selected.Digest != selected.Selected.Digest || !bytes.Equal(pinned.ConfigData, selected.ConfigData) {
			return nil, fmt.Errorf("component %s external base %q changed after planning", resolved.Identity, stage.Source)
		}
	}
	// Root metadata can change in a preceding RUN without an image layer.
	rootBaseline, err := capturePackageRootMetadata(executor.store, caller)
	if err != nil {
		return nil, fmt.Errorf("capture current component caller root: %w", err)
	}
	var cacheInputConfig json.RawMessage
	cacheable := executor.componentCache != nil && (len(executor.componentCache.readStores) != 0 || len(executor.componentCache.writeStores) != 0) && componentCacheEligible(resolved)

	if cacheable && executor.instructionRuntime == "" && componentCacheNeedsRuntime(resolved) {
		cacheable = false
	}
	var deviceInputs map[string]digest.Digest
	if cacheable {
		deviceInputs, err = cacheGraphDeviceInputs(resolved.Plan.Stages, executor.options.RunControls, executor.allowedEntitlements)
		if err != nil {
			return nil, err
		}
		// A whole-result hit skips stage execution, so request authorization
		// must precede the lookup just as it precedes instruction-cache hits.
		for _, stage := range resolved.Plan.Stages {
			for _, operation := range stage.Operations {
				if hasRunDeviceOptions(operation) {
					continue
				} // Checked during device selection above.
				if err := authorizePlannedOperation(operation, executor.allowedEntitlements, nil); err != nil {
					return nil, fmt.Errorf("component %s: %w", resolved.Identity, err)
				}
			}
		}
		preCommitConfig, err := logical.MarshalJSON()
		if err == nil {
			cacheInputConfig, err = cacheIdentityConfig(preCommitConfig)
		}
		if err != nil {
			executor.componentCache.stats.Skipped++
			cacheable = false
		}
	}
	var callerImageID string
	var callerManifest digest.Digest
	nonLayerBaseID := ""
	if executor.options.Lifecycle.NoLayers {
		nonLayerBaseID = caller.FromImageID
		if nonLayerBaseID == "" {
			nonLayerBaseID, _, err = executor.zeroLayerCallerImage(ctx, imageconfig.New(), platform, builderOptions.SystemContext)
			if err != nil {
				return nil, fmt.Errorf("create scratch nonlayered base: %w", err)
			}
		}
	}
	if caller.FromImageID == "" && !executor.options.Lifecycle.NoLayers {
		callerImageID, callerManifest, err = executor.zeroLayerCallerImage(ctx, logical, platform, builderOptions.SystemContext)
		if err != nil {
			return nil, fmt.Errorf("create scratch component caller: %w", err)
		}
	} else {
		filesystemChanged, err := componentBuilderHasFilesystemChanges(executor.store, caller)
		if err != nil {
			return nil, fmt.Errorf("inspect component caller changes: %w", err)
		}
		sourceManifest := digest.Digest("")
		if len(adoption) != 0 && adoption[0].manifest != nil {
			sourceManifest = *adoption[0].manifest
		}
		if sourceManifest == "" && caller.FromImageID != "" {
			sourceManifest, err = storedImageDefaultManifestDigest(executor.store, caller.FromImageID)
			if err != nil {
				return nil, err
			}
		}
		metadataChanged := false
		if caller.FromImageID != "" {
			annotations, err := componentImageAnnotations(ctx, executor.store, caller.FromImageID, sourceManifest, builderOptions.SystemContext)
			if err != nil {
				return nil, fmt.Errorf("read component caller annotations: %w", err)
			}
			metadataChanged = !maps.Equal(annotations, caller.Annotations())
		}
		if !filesystemChanged && !metadataChanged && caller.FromImageID != "" && len(caller.PrependedEmptyLayers) == 0 && len(caller.AppendedEmptyLayers) == 0 && len(caller.AppendedLinkedLayers) == 0 {
			callerImageID, callerManifest = caller.FromImageID, sourceManifest
		} else {
			commitOptions := upstream.CommitOptions{
				PreferredManifestType: builderOptions.Format,
				// This checkpoint is an internal component base, not a source
				// instruction. Do not materialize an empty user-visible layer.
				EmptyLayerIfEmptyDiff: true,
				OmitLayerHistoryEntry: !filesystemChanged,
				SystemContext:         builderOptions.SystemContext,
			}
			// A metadata-only internal checkpoint retains its parent's creation time,
			// avoiding a new caller identity on each otherwise unchanged invocation.
			if caller.FromImageID != "" && !filesystemChanged && len(caller.PrependedEmptyLayers) == 0 && len(caller.AppendedEmptyLayers) == 0 && len(caller.AppendedLinkedLayers) == 0 {
				raw, err := packageImageConfigSelected(ctx, executor.store, caller.FromImageID, builderOptions.SystemContext, sourceManifest)
				if err != nil {
					return nil, err
				}
				var image v1.Image
				if err := json.Unmarshal(raw, &image); err != nil {
					return nil, err
				}
				created := time.Time{}
				if image.Created != nil {
					created = *image.Created
				}
				commitOptions.HistoryTimestamp = &created
			}

			if err := syncBuilderConfig(caller, logical); err != nil {
				return nil, fmt.Errorf("synchronize component caller configuration: %w", err)
			}
			timestampPolicyFromOptions(executor.options).apply(&commitOptions)
			callerImageID, _, callerManifest, err = caller.Commit(ctx, nil, commitOptions)
			if err != nil {
				return nil, fmt.Errorf("checkpoint component caller: %w", err)
			}
		}
	}
	if err := adoptCommittedConfig(ctx, executor.store, callerImageID, builderOptions.SystemContext, logical); err != nil {
		return nil, fmt.Errorf("reconcile component caller config: %w", err)
	}
	var componentImageID string
	var componentManifest digest.Digest
	var componentBaseImageID string
	var componentConfig *imageconfig.Config
	var componentRoot *PackageRootMetadata
	var candidateKey *cache.Key
	cacheHit := false
	if len(resolved.Plan.Outputs) != 1 {
		return nil, fmt.Errorf("component invocation requires exactly one output, got %d", len(resolved.Plan.Outputs))
	}
	outputID := resolved.Plan.Outputs[0]
	if cacheable {
		postCommitConfig, configErr := logical.MarshalJSON()
		if configErr == nil {
			var identity stateidentity.Identity
			var snapshotPath string
			var eligible bool
			identity, _, snapshotPath, eligible, configErr = snapshotPortableState(ctx, executor.store, builderOptions.SystemContext, callerImageID, postCommitConfig, cacheInputConfig, rootBaseline, platform, executor.componentCache.stagingDir)
			if snapshotPath != "" {
				_ = os.Remove(snapshotPath)
			}
			if configErr == nil && eligible {
				key, keyErr := executor.componentCache.key(executor, identity, resolved, parameters, platform, logical.CommandSet())
				if keyErr == nil {
					if key.Inputs == nil {
						key.Inputs = map[string]digest.Digest{}
					}
					for name, identity := range deviceInputs {
						key.Inputs[name] = identity
					}
					key.Inputs["caller-image"] = callerManifest
					candidateKey = &key
					var entry instructionCacheEntry
					if !executor.options.NoCache {
						entry, componentConfig, configErr = executor.componentCache.lookup(ctx, executor, key, builderOptions.SystemContext, componentCacheCaller{ImageID: callerImageID, Manifest: callerManifest, RetainsCallerAllowed: componentOutputExtendsCaller(resolved.Plan)})
						if configErr != nil {
							return nil, fmt.Errorf("component cache lookup: %w", configErr)
						}
						if entry.ImageID != "" {
							componentImageID = entry.ImageID
							componentBaseImageID = entry.BaseImageID
							extendsCaller = *entry.RetainsCaller
							cacheHit = true
							progress.line("--> Using component cache %s", resolved.Identity)
							progress.image(componentImageID)
							componentRoot = entry.RootMetadata
							componentManifest = entry.ManifestDigest
						}
					}
				} else {
					configErr = keyErr
				}
			}
		}
		if configErr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			packageCacheWarning("prepare component cache", configErr)
			executor.componentCache.stats.Skipped++
		} else if candidateKey == nil {
			executor.componentCache.stats.Skipped++
		}
	}

	if !cacheHit {
		packages := make(map[string]stageState, len(resolved.PackageInputs))
		bindPackages := func(ctx context.Context, plan *planner.Plan) error {
			inputs, err := componentPackageInputs(plan, resolved.Artifact.Component.Packages)
			if err != nil {
				return err
			}
			selected := *resolved
			selected.PackageInputs = inputs
			for stageID := range inputs {
				if _, bound := packages[stageID]; bound {
					continue
				}
				imported, err := executor.packageImport.Import(ctx, &selected, stageID, platform)
				if err != nil {
					return fmt.Errorf("import component package stage %s: %w", stageID, err)
				}
				packages[stageID] = stageState{storageImageID: imported.ImageID, manifestDigest: imported.ManifestDigest, config: imported.Config}
			}
			return nil
		}
		if err := bindPackages(ctx, resolved.Plan); err != nil {
			return nil, err
		}
		stageTotal := 0
		if resolved.Artifact != nil && resolved.Artifact.Component != nil {
			stageTotal = definitionStageCount(resolved.Artifact.Component.Component.Definition)
		}
		bindings := &graphBindings{
			delivery: resolved.Artifact.Delivery,
			observeManifest: func(id string, manifest digest.Digest, baseID string, retainsCaller bool) {
				if id == outputID {
					componentManifest = manifest
					componentBaseImageID = baseID
					extendsCaller = retainsCaller
				}
			},
			progressStageTotal: stageTotal,
			progressPrefix:     progress.prefix + "[component " + progressText(operation.Arguments[0]) + "] ",
			caller:             stageState{storageImageID: callerImageID, manifestDigest: callerManifest, config: logical.Continue(), root: rootBaseline},
			packages:           packages,
			bindPackages:       bindPackages,
			cacheScope: CacheMountScope{
				Kind: "component", Source: resolved.Identity, Platform: platform,
				Parameters: maps.Clone(parameters),
			},
			replannedBaseDelta: resolved.replannedBaseDelta,
		}
		observed := map[string]bool{outputID: true}
		_, err = executor.executePlanGraph(ctx, resolved.Plan, resolved.Plan.Stages, "", observed, func(_ storage.Store, stage planner.Stage, imageID string, config *imageconfig.Config, root *PackageRootMetadata) error {
			if stage.ID == outputID {
				componentImageID = imageID
				componentConfig = config.Continue()
				componentRoot = root
			}
			return nil
		}, bindings)
		if err != nil {
			return nil, fmt.Errorf("execute component %s: %w", resolved.Identity, err)
		}
	}
	if componentImageID == "" || componentConfig == nil || componentRoot == nil {
		return nil, fmt.Errorf("component %s output stage %s was not executed", resolved.Identity, outputID)
	}
	if len(adoption) != 0 && adoption[0].grouped && !extendsCaller {
		return nil, errors.New("replacement component output cannot cross a layer group's image lineage")
	}
	if componentBaseImageID == "" {
		componentBaseImageID, _, err = executor.zeroLayerCallerImage(ctx, imageconfig.New(), platform, builderOptions.SystemContext)
		if err != nil {
			return nil, fmt.Errorf("capture replacement scratch base: %w", err)
		}
	}
	if candidateKey != nil && executor.componentCache != nil && !cacheHit {
		// Cache the selected image and its current root metadata together.
		executor.componentCache.record(ctx, executor, *candidateKey, componentImageID, componentManifest, componentConfig, componentRoot, builderOptions.SystemContext, componentBaseImageID, extendsCaller)
	}
	if executor.options.Lifecycle.NoLayers {
		if !extendsCaller {
			nonLayerBaseID = componentBaseImageID
			if nonLayerBaseID == "" {
				nonLayerBaseID, _, err = executor.zeroLayerCallerImage(ctx, imageconfig.New(), platform, builderOptions.SystemContext)
				if err != nil {
					return nil, err
				}
			}
		}

		nonLayerOptions := *builderOptions
		nonLayerOptions.FromImage = nonLayerBaseID
		nonLayerOptions.PreserveBaseImageAnns = true
		nonLayerOptions.PullPolicy = define.PullNever
		nonLayerOptions.Container = executor.builderContainerName("component")
		combined, err := upstream.NewBuilder(ctx, executor.store, nonLayerOptions)
		if err != nil {
			return nil, fmt.Errorf("recreate nonlayered component caller: %w", err)
		}
		combined.FromImage = nonLayerBaseID
		combined.FromImageID = nonLayerBaseID
		if extendsCaller {
			for _, key := range []string{v1.AnnotationBaseImageName, v1.AnnotationBaseImageDigest} {
				if value, found := caller.Annotations()[key]; found {
					combined.SetAnnotation(key, value)
				} else {
					combined.UnsetAnnotation(key)
				}
			}
		}
		keepCombined := false
		defer func() {
			if !keepCombined {
				retErr = errors.Join(retErr, combined.Delete())
			}
		}()
		if err := applyComponentDeltaMutable(ctx, executor.store, nonLayerBaseID, componentImageID, combined, os.TempDir()); err != nil {
			return nil, fmt.Errorf("apply nonlayered component %s: %w", resolved.Identity, err)
		}
		if err := restorePackageRootMetadata(executor.store, combined, componentRoot); err != nil {
			return nil, err
		}
		if err := syncBuilderConfig(combined, componentConfig); err != nil {
			return nil, fmt.Errorf("sync nonlayered component %s config: %w", resolved.Identity, err)
		}
		if err := caller.Delete(); err != nil {
			return nil, fmt.Errorf("delete nonlayered component caller: %w", err)
		}
		*logical = *componentConfig.Continue()
		*builderOptions = nonLayerOptions
		if len(adoption) != 0 && adoption[0].manifest != nil {
			// The returned builder has a mutable net delta against its effective base.
			*adoption[0].manifest = ""
		}
		if !extendsCaller && len(adoption) != 0 && adoption[0].effectiveBase != nil {
			*adoption[0].effectiveBase = nonLayerBaseID
		}
		if !extendsCaller && len(adoption) != 0 && adoption[0].lineage != nil {
			*adoption[0].lineage = false
		}
		keepCombined = true
		return combined, nil
	}

	adoptOptions := *builderOptions
	selectedOutput, err := oci.SelectedStoredImage(ctx, executor.store, componentImageID, componentManifest)
	if err != nil {
		return nil, fmt.Errorf("select component output manifest: %w", err)
	}
	adoptOptions.FromImage = selectedOutput
	adoptOptions.PreserveBaseImageAnns = true
	adoptOptions.PullPolicy = define.PullNever
	adoptOptions.Container = executor.builderContainerName("component")
	adopted, err := upstream.NewBuilder(ctx, executor.store, adoptOptions)
	if err != nil {
		return nil, fmt.Errorf("adopt component output: %w", err)
	}
	adopted.FromImage, adopted.FromImageID = componentImageID, componentImageID
	keep := false
	defer func() {
		if !keep {
			retErr = errors.Join(retErr, adopted.Delete())
		}
	}()
	if err := restorePackageRootMetadata(executor.store, adopted, componentRoot); err != nil {
		return nil, fmt.Errorf("restore component root: %w", err)
	}
	if err := syncBuilderConfig(adopted, componentConfig); err != nil {
		return nil, fmt.Errorf("sync component config: %w", err)
	}
	if err := caller.Delete(); err != nil {
		return nil, fmt.Errorf("delete component caller: %w", err)
	}
	*logical = *componentConfig.Continue()
	*builderOptions = adoptOptions
	if len(adoption) != 0 && adoption[0].manifest != nil {
		*adoption[0].manifest = componentManifest
	}
	if !extendsCaller && len(adoption) != 0 && adoption[0].effectiveBase != nil {
		*adoption[0].effectiveBase = componentBaseImageID
	}
	if !extendsCaller && len(adoption) != 0 && adoption[0].lineage != nil {
		*adoption[0].lineage = false
	}
	keep = true
	return adopted, nil
}

func componentBuilderHasFilesystemChanges(store storage.Store, builder *upstream.Builder) (bool, error) {
	container, err := store.Container(builder.ContainerID)
	if err != nil {
		return false, fmt.Errorf("inspect component output container: %w", err)
	}
	baseLayer := ""
	if builder.FromImageID != "" {
		base, err := store.Image(builder.FromImageID)
		if err != nil {
			return false, fmt.Errorf("inspect component output base: %w", err)
		}
		baseLayer = base.TopLayer
	}
	changes, err := store.Changes(baseLayer, container.LayerID)
	if err != nil {
		return false, fmt.Errorf("inspect component output changes: %w", err)
	}
	return len(changes) != 0, nil
}

func (executor *graphExecutor) zeroLayerCallerImage(ctx context.Context, logical *imageconfig.Config, platform v1.Platform, system *types.SystemContext) (string, digest.Digest, error) {
	raw, err := logical.MarshalJSON()
	if err != nil {
		return "", "", err
	}
	platformString, err := componentPlatformString(platform)
	if err != nil {
		return "", "", err
	}
	key := digest.FromBytes(append(append([]byte(nil), raw...), []byte("\x00"+platformString)...))
	if base := executor.zeroLayerBases[key]; base.storageImageID != "" {
		return base.storageImageID, base.manifestDigest, nil
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return "", "", fmt.Errorf("decode caller config: %w", err)
	}
	for name, value := range map[string]any{
		"architecture": platform.Architecture,
		"os":           platform.OS,
		"rootfs":       v1.RootFS{Type: "layers"},
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", "", err
		}
		document[name] = encoded
	}
	configData, err := json.Marshal(document)
	if err != nil {
		return "", "", fmt.Errorf("encode zero-layer caller config: %w", err)
	}
	dir, err := os.MkdirTemp(os.TempDir(), "coopr-zero-layer-")
	if err != nil {
		return "", "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	layout := filepath.Join(dir, "layout")
	source, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		return "", "", err
	}
	config := oci.Descriptor(v1.MediaTypeImageConfig, configData)
	if err := source.Push(ctx, config, bytes.NewReader(configData)); err != nil {
		return "", "", err
	}
	manifestData, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config,
	})
	if err != nil {
		return "", "", err
	}
	manifest := oci.Descriptor(v1.MediaTypeImageManifest, manifestData)
	if err := source.Push(ctx, manifest, bytes.NewReader(manifestData)); err != nil {
		return "", "", err
	}
	imageID, err := ImportSelectedImage(ctx, executor.store, system, layout, manifest)
	if err != nil {
		return "", "", err
	}
	executor.zeroLayerBases[key] = stageState{storageImageID: imageID, manifestDigest: manifest.Digest}
	return imageID, manifest.Digest, nil
}

func componentInvocationSelectionKey(reference string, parameters map[string]string, target v1.Platform) (string, error) {
	key, err := componentSelectionKey(reference, target)
	if err != nil || !definition.IsLocalComponentReference(reference) {
		return key, err
	}
	normalized, err := planner.NormalizeParameters(parameters)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal([]string{key, string(normalized)})
	return string(encoded), err
}

// Scope is part of selection identity: identical component bytes in different
// captures may select different nested dependency policies.
func scopedComponentSelectionKey(reference string, parameters map[string]string, target v1.Platform, scope *oci.DeliveryScope) (string, error) {
	key, err := componentInvocationSelectionKey(reference, parameters, target)
	if err != nil || scope == nil {
		return key, err
	}
	data, err := json.Marshal(struct {
		Selection string             `json:"selection"`
		Scope     *oci.DeliveryScope `json:"scope"`
	}{key, scope})
	return string(data), err
}

func componentOperationDeliveryScope(delivery *oci.ComponentDelivery, operation planner.Operation) (*oci.DeliveryScope, error) {
	if delivery == nil {
		return nil, nil
	}
	reference := operation.OriginalComponentReference
	if reference == "" && len(operation.Arguments) != 0 {
		reference = operation.Arguments[0]
	}
	return delivery.DependencyScope(reference)
}

func cloneComponentSelections(source map[string]oci.ComponentSelection) map[string]oci.ComponentSelection {
	if source == nil {
		return nil
	}
	result := make(map[string]oci.ComponentSelection, len(source))
	for key, selection := range source {
		result[key] = selection.Clone()
	}
	return result
}

func (executor *graphExecutor) retainComponentBases(reference string, resolved *ResolvedComponentPlan) error {
	for key, selected := range resolved.SelectedBases {
		if selected.ImageID == "" {
			return fmt.Errorf("component reference %q selected an empty image for %q", reference, key.Reference)
		}
		if existing, found := executor.resolvedBases[key]; found && existing.ImageID != selected.ImageID {
			return fmt.Errorf("component image source %q changed selection during build: %s then %s", key.Reference, existing.ImageID, selected.ImageID)
		}
		if executor.resolvedBases == nil {
			executor.resolvedBases = make(map[ResolvedBaseKey]ResolvedImageSource)
		}
		executor.resolvedBases[key] = selected
	}
	return nil
}

// componentOutputExtendsCaller follows only filesystem ancestry, not COPY,
// mount or ordering dependencies, to distinguish replacement images.
func componentOutputExtendsCaller(plan *planner.Plan) bool {
	if plan == nil || len(plan.Outputs) != 1 {
		return false
	}
	stages := map[string]planner.Stage{}
	for _, stage := range plan.Stages {
		stages[stage.ID] = stage
		if stage.Name != "" {
			stages[strings.ToLower(stage.Name)] = stage
		}
	}
	current := plan.Outputs[0]
	seen := map[string]bool{}
	for !seen[current] {
		seen[current] = true
		stage, ok := stages[current]
		if !ok {
			return false
		}
		if stage.Kind == "extend" {
			return true
		}
		if stage.Kind != "from" || stage.SourceContext != "" {
			return false
		}
		current = strings.ToLower(stage.Source)
	}
	return false
}

type componentAdoptionContext struct {
	delivery      *oci.ComponentDelivery
	lineage       *bool
	effectiveBase *string
	grouped       bool
	manifest      *digest.Digest
}

func componentImageAnnotations(ctx context.Context, store storage.Store, imageID string, selected digest.Digest, system *types.SystemContext) (_ map[string]string, retErr error) {
	reference, err := imagestorage.Transport.NewStoreReference(store, nil, imageID)
	if err != nil {
		return nil, err
	}
	source, err := reference.NewImageSource(ctx, system)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, source.Close()) }()
	raw, _, err := source.GetManifest(ctx, optionalDigest(selected))
	if err != nil {
		return nil, err
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	return manifest.Annotations, nil
}
