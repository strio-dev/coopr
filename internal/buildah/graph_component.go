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
// parameterized plans. Repeated identical calls avoid another mutable tag
// lookup; a tag that changes while resolving a second parameter set aborts the
// build before two artifact digests can be mixed.
func (executor *graphExecutor) resolveComponentInvocation(ctx context.Context, reference string, parameters map[string]string, target v1.Platform, progressPrefix string) (*ResolvedComponentPlan, error) {
	selectionKey, err := componentInvocationSelectionKey(reference, parameters, target)
	if err != nil {
		return nil, err
	}
	normalized, err := planner.NormalizeParameters(parameters)
	if err != nil {
		return nil, fmt.Errorf("normalize component parameters: %w", err)
	}
	variantKey := string(normalized)
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
	resolveReference := reference
	if pinned := executor.componentPins[selectionKey]; pinned != "" {
		resolveReference = pinned
	}
	local := definition.IsLocalComponentReference(reference)
	localSource := ""
	if local && resolveReference == reference {
		if executor.componentPins != nil {
			return nil, fmt.Errorf("local component %q was not frozen before stage execution", reference)
		}
		resolveReference, localSource, err = executor.materializeLocalComponent(ctx, reference, parameters, target, progressPrefix)
		if err != nil {
			return nil, err
		}
	}
	resolved, err := resolve(ctx, ComponentPlanRequest{
		Resolver: executor.options.Resolver, Reference: resolveReference,
		Parameters: maps.Clone(parameters), Platform: target, LocalParameters: local,
		ResolveBase: executor.resolveBaseImage,
	})
	if err != nil {
		return nil, err
	}
	resolved.localSourceIdentity = localSource
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
	if resolved.Artifact.Repository == "local" {
		return resolved.Identity.String(), nil
	}
	if resolved.Artifact.Repository == "" {
		return "", errors.New("resolved component has no repository")
	}
	return resolved.Artifact.Repository + "@" + resolved.Identity.String(), nil
}

// prepareIsolatedStageComponents selects every component reachable from a
// stage before it is launched in another process. Workers resolve only these
// immutable references, so parallel stages cannot observe different values of
// the same mutable component tag.
func (executor *graphExecutor) prepareIsolatedStageComponents(ctx context.Context, prepared *preparedGraphStage) error {
	if prepared == nil {
		return errors.New("prepared stage is nil")
	}
	pins := make(map[string]string)
	active := make(map[digest.Digest]bool)
	var visitOperations func([]planner.Operation, v1.Platform, string) error
	visitOperations = func(operations []planner.Operation, target v1.Platform, prefix string) error {
		for _, operation := range operations {
			if operation.Name != "component" {
				continue
			}
			if len(operation.Arguments) != 1 || operation.Arguments[0] == "" {
				return errors.New("component invocation requires one reference")
			}
			reference := operation.Arguments[0]
			resolved, err := executor.resolveComponentInvocation(ctx, reference, operation.Properties, target, prefix)
			if err != nil {
				return err
			}
			key, err := componentInvocationSelectionKey(reference, operation.Properties, target)
			if err != nil {
				return err
			}
			pinned, err := pinnedComponentReference(resolved)
			if err != nil {
				return err
			}
			if existing := pins[key]; existing != "" && existing != pinned {
				return fmt.Errorf("component reference %q changed immutable selection", reference)
			}
			pins[key] = pinned
			if err := func() error {
				release, err := executor.enterLocalComponent(resolved.localSourceIdentity)
				if err != nil {
					return err
				}
				defer release()
				if active[resolved.Identity] {
					return fmt.Errorf("component invocation cycle at %s", resolved.Identity)
				}
				active[resolved.Identity] = true
				defer delete(active, resolved.Identity)
				for _, stage := range resolved.Plan.Stages {
					platform, err := componentPlatformFromString(stage.Platform)
					if err != nil {
						return err
					}
					if err := visitOperations(stage.Operations, platform, graphProgressPrefix(stage, prefix+"[component "+progressText(reference)+"] ", 0)); err != nil {
						return err
					}
				}
				return nil
			}(); err != nil {
				return err
			}
		}
		return nil
	}
	target, err := componentPlatformFromString(prepared.stage.Platform)
	if err != nil {
		return err
	}
	if err := visitOperations(prepared.operations, target, prepared.progressPrefix); err != nil {
		return err
	}
	prepared.componentPins = pins
	return nil
}

// applyComponentOperation executes one immutable component invocation against
// the caller state at this exact source position, then compacts its net change
// onto a fresh caller-based builder. The caller builder remains valid on error.
func (executor *graphExecutor) applyComponentOperation(ctx context.Context, caller *upstream.Builder, builderOptions *upstream.BuilderOptions, logical *imageconfig.Config, rootBaseline *PackageRootMetadata, platform v1.Platform, operation planner.Operation, progress stageProgress) (_ *upstream.Builder, retErr error) {
	if executor.options.Resolver == nil || executor.packageImport == nil {
		return nil, errors.New("component invocation requires a Coopr component resolver")
	}
	if caller == nil || builderOptions == nil || logical == nil {
		return nil, errors.New("component invocation requires caller builder, builder options, and logical config")
	}
	if len(operation.Arguments) != 1 || operation.Arguments[0] == "" {
		return nil, errors.New("component invocation requires one reference")
	}

	resolved, err := executor.resolveComponentInvocation(ctx, operation.Arguments[0], operation.Properties, platform, progress.prefix)
	if err != nil {
		return nil, err
	}
	release, err := executor.enterLocalComponent(resolved.localSourceIdentity)
	if err != nil {
		return nil, err
	}
	defer release()
	effectivePlan := *resolved.Plan
	effectivePlan.Stages, err = resolveAndValidateBuildNetwork(resolved.Plan.Stages, executor.options.Network, executor.options.RunControls)
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

	if err := checkCallerCompatibility(caller, logical, resolved.Plan); err != nil {
		return nil, fmt.Errorf("component %s compatibility: %w", resolved.Identity, err)
	}
	localNames := make(map[string]bool, len(resolved.Plan.Stages))
	for _, stage := range resolved.Plan.Stages {
		if stage.Name != "" {
			localNames[strings.ToLower(stage.Name)] = true
		}
	}
	for _, stage := range resolved.Plan.Stages {
		if stage.Kind != "from" || stage.Source == "" || strings.EqualFold(stage.Source, "scratch") || localNames[strings.ToLower(stage.Source)] {
			continue
		}
		key := ResolvedBaseKey{Reference: stage.Source, Platform: stage.Platform}
		selected, selectedHere := resolved.SelectedBases[key]
		if !selectedHere {
			return nil, fmt.Errorf("component %s external base %q was not selected during planning", resolved.Identity, stage.Source)
		}
		pinned, found := executor.resolvedBases[key]
		if !found || pinned.ImageID != selected.ImageID || pinned.Selected.Digest != selected.Selected.Digest || !bytes.Equal(pinned.ConfigData, selected.ConfigData) {
			return nil, fmt.Errorf("component %s external base %q changed after planning", resolved.Identity, stage.Source)
		}
	}
	var cacheInputConfig json.RawMessage
	cacheable := executor.componentCache != nil && (len(executor.componentCache.readStores) != 0 || len(executor.componentCache.writeStores) != 0) && buildResultCacheEligible(executor.options.AddHosts) && componentCacheEligible(resolved, executor.options.RunControls)
	if cacheable && executor.instructionRuntime == "" && componentCacheNeedsRuntime(resolved) {
		cacheable = false
	}
	if cacheable {
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
		commitOptions := upstream.CommitOptions{
			PreferredManifestType: builderOptions.Format,
			// This checkpoint is an internal component base, not a source
			// instruction. Do not materialize an empty user-visible layer.
			EmptyLayerIfEmptyDiff: true,
			OmitLayerHistoryEntry: !filesystemChanged,
			SystemContext:         builderOptions.SystemContext,
		}
		timestampPolicyFromOptions(executor.options).apply(&commitOptions)
		callerImageID, _, callerManifest, err = caller.Commit(ctx, nil, commitOptions)
		if err != nil {
			return nil, fmt.Errorf("checkpoint component caller: %w", err)
		}
	}
	if err := adoptCommittedConfig(ctx, executor.store, callerImageID, builderOptions.SystemContext, logical); err != nil {
		return nil, fmt.Errorf("reconcile component caller config: %w", err)
	}
	var componentImageID string
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
				key, keyErr := executor.componentCache.key(executor, identity, resolved, operation.Properties, platform)
				if keyErr == nil {
					candidateKey = &key
					var hit bool
					if !executor.options.NoCache {
						componentImageID, componentConfig, hit, configErr = executor.componentCache.lookup(ctx, executor, key, callerImageID, postCommitConfig, rootBaseline, platform, builderOptions.SystemContext)
						if hit {
							cacheHit = true
							progress.line("--> Using component cache %s", resolved.Identity)
							progress.image(componentImageID)
							componentRoot = rootBaseline
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
			executor.componentCache.stats.Skipped++
		} else if candidateKey == nil {
			executor.componentCache.stats.Skipped++
		}
	}

	if !cacheHit {
		packages := make(map[string]stageState, len(resolved.PackageInputs))
		for stageID := range resolved.PackageInputs {
			imported, err := executor.packageImport.Import(ctx, resolved, stageID, platform)
			if err != nil {
				return nil, fmt.Errorf("import component package stage %s: %w", stageID, err)
			}
			packages[stageID] = stageState{storageImageID: imported.ImageID, manifestDigest: imported.ManifestDigest, config: imported.Config}
		}
		stageTotal := 0
		if resolved.Artifact != nil && resolved.Artifact.Component != nil {
			stageTotal = definitionStageCount(resolved.Artifact.Component.Component.Definition)
		}
		bindings := &graphBindings{
			progressStageTotal: stageTotal,
			progressPrefix:     progress.prefix + "[component " + progressText(operation.Arguments[0]) + "] ",
			caller:             stageState{storageImageID: callerImageID, manifestDigest: callerManifest, config: logical.Clone()},
			packages:           packages,
			cacheScope: CacheMountScope{
				Kind: "component", Source: resolved.Identity, Platform: platform,
				Parameters: maps.Clone(operation.Properties),
			},
			replannedBaseDelta: resolved.replannedBaseDelta,
		}
		observed := map[string]bool{outputID: true}
		_, err = executor.executePlanGraph(ctx, resolved.Plan, resolved.Plan.Stages, "", observed, func(_ storage.Store, stage planner.Stage, imageID string, config *imageconfig.Config, root *PackageRootMetadata) error {
			if stage.ID == outputID {
				componentImageID = imageID
				componentConfig = config.Clone()
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
	if candidateKey != nil && executor.componentCache != nil && !cacheHit {
		// Compaction retains the caller's root metadata. Cache that same root
		// rather than root changes discarded by the image-layer diff.
		executor.componentCache.record(ctx, executor, *candidateKey, componentImageID, componentConfig, rootBaseline, platform, builderOptions.SystemContext)
	}
	if executor.options.Lifecycle.NoLayers {
		nonLayerOptions := *builderOptions
		nonLayerOptions.FromImage = nonLayerBaseID
		nonLayerOptions.PullPolicy = define.PullNever
		nonLayerOptions.Container = executor.builderContainerName("component")
		combined, err := upstream.NewBuilder(ctx, executor.store, nonLayerOptions)
		if err != nil {
			return nil, fmt.Errorf("recreate nonlayered component caller: %w", err)
		}
		combined.FromImage = nonLayerBaseID
		combined.FromImageID = nonLayerBaseID
		keepCombined := false
		defer func() {
			if !keepCombined {
				retErr = errors.Join(retErr, combined.Delete())
			}
		}()
		if err := applyComponentDeltaMutable(ctx, executor.store, nonLayerBaseID, componentImageID, combined, os.TempDir()); err != nil {
			return nil, fmt.Errorf("apply nonlayered component %s: %w", resolved.Identity, err)
		}
		if err := syncBuilderConfig(combined, componentConfig); err != nil {
			return nil, fmt.Errorf("sync nonlayered component %s config: %w", resolved.Identity, err)
		}
		if err := caller.Delete(); err != nil {
			return nil, fmt.Errorf("delete nonlayered component caller: %w", err)
		}
		*logical = *componentConfig.Clone()
		*builderOptions = nonLayerOptions
		keepCombined = true
		return combined, nil
	}

	compactOptions := *builderOptions
	compactBase, err := selectedBuilderBase(ctx, executor.store, callerImageID, callerManifest)
	if err != nil {
		return nil, fmt.Errorf("select component caller manifest %s: %w", callerManifest, err)
	}
	compactOptions.FromImage = compactBase
	compactOptions.PullPolicy = define.PullNever
	compactOptions.Container = executor.builderContainerName("component")
	compacted, err := upstream.NewBuilder(ctx, executor.store, compactOptions)
	if err != nil {
		return nil, fmt.Errorf("create component output builder: %w", err)
	}
	compacted.FromImage = callerImageID
	compacted.FromImageID = callerImageID
	keepCompacted := false
	defer func() {
		if !keepCompacted {
			retErr = errors.Join(retErr, compacted.Delete())
		}
	}()
	if err := ApplyComponentDelta(ctx, executor.store, callerImageID, componentImageID, compacted, os.TempDir()); err != nil {
		return nil, fmt.Errorf("compact component %s: %w", resolved.Identity, err)
	}
	if err := syncBuilderConfig(compacted, componentConfig); err != nil {
		return nil, fmt.Errorf("sync component %s config: %w", resolved.Identity, err)
	}
	if err := caller.Delete(); err != nil {
		return nil, fmt.Errorf("delete component caller builder: %w", err)
	}
	*logical = *componentConfig.Clone()
	*builderOptions = compactOptions
	keepCompacted = true
	return compacted, nil
}

func checkCallerCompatibility(caller *upstream.Builder, logical *imageconfig.Config, plan *planner.Plan) (retErr error) {
	raw, err := logical.MarshalJSON()
	if err != nil {
		return fmt.Errorf("marshal caller config: %w", err)
	}
	rootfs, err := caller.Mount(caller.MountLabel)
	if err != nil {
		return fmt.Errorf("mount caller rootfs: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, caller.Unmount()) }()
	return CheckCompatibility(rootfs, raw, plan.Platform, plan.Stages)
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
