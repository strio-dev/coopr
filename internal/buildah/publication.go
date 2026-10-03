package buildah

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"coopr/internal/cache"
	"coopr/internal/imageconfig"
	"coopr/internal/oci"
	"coopr/internal/planner"

	"github.com/containerd/platforms"
	"go.podman.io/storage"
)

// PublicationOptions supplies execution values and one tar destination for
// every package output. Package paths may be keyed by package stage name or ID.
type PublicationOptions struct {
	PlanOptions
	PackagePaths map[string]string
}

type packageOutput struct {
	key  string
	path string
}

// PublishPlan executes a planner publication graph once and snapshots each
// package output into Coopr's existing OCI component package representation.
func PublishPlan(ctx context.Context, plan *planner.Plan, options PublicationOptions) (_ map[string]oci.Package, retErr error) {
	if options.JobID != "" && !validWorkerJobID(options.JobID) {
		return nil, errors.New("invalid supervised build job ID")
	}
	if _, err := allowedEntitlements(options.Allow); err != nil {
		return nil, err
	}
	stages, outputs, err := validatePublicationGraph(plan, options.PackagePaths)
	if err != nil {
		return nil, err
	}
	options.PlanOptions, err = normalizePlanOptions(options.PlanOptions)
	if err != nil {
		return nil, err
	}
	stages, err = resolveAndValidateBuildNetwork(stages, options.Network, options.RunControls)
	if err != nil {
		return nil, err
	}
	options.SourceDateEpoch = plan.SourceDateEpoch
	if options.SourceDateEpochOverride != nil {
		options.SourceDateEpoch = options.SourceDateEpochOverride
	}
	if err := validateTimestampOptions(options.Timestamp, options.SourceDateEpoch, options.RewriteTimestamp); err != nil {
		return nil, err
	}
	if len(stages) == 0 {
		return map[string]oci.Package{}, nil
	}
	for _, input := range plan.Inputs {
		if input.Kind == "image" && options.Resolver == nil {
			selected := false
			for key, source := range options.ResolvedBases {
				if key.Reference == input.Reference && source.ImageID != "" {
					selected = true
					break
				}
			}
			if !selected {
				return nil, fmt.Errorf("external image %q requires a Coopr image resolver", input.Reference)
			}
		}
	}
	paths := make([]string, 0, len(outputs))
	for _, output := range outputs {
		paths = append(paths, output.path)
	}
	validationOutput := ""
	if len(paths) != 0 {
		validationOutput = paths[0]
	} else {
		temporary, err := os.MkdirTemp("", ".coopr-unused-packages-*")
		if err != nil {
			return nil, fmt.Errorf("create unused package workspace: %w", err)
		}
		defer func() { retErr = errors.Join(retErr, os.RemoveAll(temporary)) }()
		validationOutput = filepath.Join(temporary, "unused.oci")
	}
	if err := validateRequest(Request{
		Store: options.Store, ContextDir: options.ContextDir, Output: Output{Path: validationOutput},
	}); err != nil {
		return nil, err
	}
	activity, err := acquirePlanActivity(ctx, options.PlanOptions)
	if err != nil {
		return nil, fmt.Errorf("acquire component publication store activity lease: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, activity.Close()) }()

	planOptions := options.PlanOptions
	// Package snapshots retain Docker image configuration (including pending
	// ONBUILD) for later FROM stages in the invocation graph.
	planOptions.Output = Output{Path: validationOutput, Format: outputFormatDocker}
	planOptions.ContextArtifacts = append(slices.Clone(planOptions.ContextArtifacts), paths...)
	for _, output := range outputs {
		cacheCheck := planOptions
		cacheCheck.Output.Path = output.path
		if err := validateComponentCachePath(cacheCheck); err != nil {
			return nil, fmt.Errorf("package %q cache location: %w", output.key, err)
		}
	}
	planOptions.ResolvedBases, err = selectPublicationBases(ctx, planOptions, stages)
	if err != nil {
		return nil, err
	}
	packageCache, err := newPackageResultCache(ctx, planOptions)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := packageCache.close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close package cache: %w", closeErr))
		}
	}()
	cacheArtifacts := append(slices.Clone(planOptions.ContextArtifacts), planOptions.Store.RunRoot, planOptions.Store.GraphRoot, planOptions.Output.Path, planOptions.CacheLocalDir)
	var packageKeys map[string]cache.PackageKey
	if packageCache != nil && len(outputs) != 0 {
		var eligible bool
		packageKeys, eligible, err = packageCacheKeys(ctx, planOptions, plan, stages, outputs, cacheArtifacts)
		if err != nil {
			return nil, fmt.Errorf("prepare package cache: %w", err)
		}
		if eligible {
			if !planOptions.NoCache {
				hits, complete, err := packageCache.lookupAll(ctx, packageKeys)
				if err != nil {
					return nil, err
				}
				if complete {
					defer func() {
						for _, hit := range hits {
							_ = os.Remove(hit.path)
						}
					}()
					return installPackageCacheHits(ctx, hits, outputs)
				}
			}
		} else {
			packageKeys = nil
		}
	}
	executor, err := newGraphExecutor(ctx, planOptions)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := executor.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close containers/storage: %w", closeErr))
		}
	}()
	for key, selected := range planOptions.ResolvedBases {
		if selected.Root.Digest == "" {
			continue
		}
		platform, err := platforms.Parse(key.Platform)
		if err != nil {
			return nil, fmt.Errorf("materialize package image %q: %w", key.Reference, err)
		}
		resolver := planOptions.Resolver
		if strings.HasPrefix(key.Reference, graphNamedContextPrefix) {
			// Named contexts are frozen into this same Buildah graph during
			// planning. Verify and reuse that immutable image directly; the
			// synthetic map key is deliberately not an OCI image reference.
			resolver = nil
		}
		materialized, err := materializeImageSource(ctx, resolver, key.Reference, platform, executor.store, platformSystemContext(executor.system, platform), selected)
		if err != nil {
			return nil, fmt.Errorf("materialize package image %q: %w", key.Reference, err)
		}
		executor.resolvedBases[key] = materialized
	}
	packages := make(map[string]oci.Package, len(outputs))
	observed := make(map[string]bool, len(outputs))
	for stageID := range outputs {
		observed[stageID] = true
	}
	_, err = executor.executePlanGraph(ctx, plan, stages, "", observed, func(store storage.Store, stage planner.Stage, imageID string, config *imageconfig.Config, rootMetadata *PackageRootMetadata) error {
		output, ok := outputs[stage.ID]
		if !ok {
			return nil
		}
		rawConfig, err := config.MarshalJSON()
		if err != nil {
			return fmt.Errorf("marshal package %q config: %w", output.key, err)
		}
		platform, err := executionPlatform(stage.Platform)
		if err != nil {
			return fmt.Errorf("package %q platform: %w", output.key, err)
		}
		pkg, err := SnapshotPackage(ctx, store, PackageSnapshotRequest{
			ImageID: imageID, Stage: output.key, TarPath: output.path,
			Platform: platform, SystemContext: platformSystemContext(options.SystemContext, platform),
			Config: rawConfig, RootMetadata: rootMetadata,
			SourceDateEpoch: options.SourceDateEpoch, RewriteTimestamp: options.RewriteTimestamp,
			Timestamp: options.Timestamp,
		})
		if err != nil {
			return err
		}
		packages[output.key] = pkg
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	if packageCache != nil && len(packageKeys) != 0 {
		err = errors.Join(err, packageCache.store(ctx, packageKeys, outputs, packages))
	}
	if executor.componentCache != nil {
		err = errors.Join(err, executor.componentCache.publish(ctx))
	}
	if executor.instructionPortableCache != nil {
		err = errors.Join(err, executor.instructionPortableCache.publish(ctx))
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return packages, nil
}

func selectPublicationBases(ctx context.Context, options PlanOptions, stages []planner.Stage) (map[ResolvedBaseKey]ResolvedImageSource, error) {
	selected := make(map[ResolvedBaseKey]ResolvedImageSource, len(options.ResolvedBases))
	for key, source := range options.ResolvedBases {
		selected[key] = source
	}
	aliases := make(map[string]bool, len(stages))
	resolve := func(reference, platformString string) error {
		if reference == "" || strings.EqualFold(reference, "scratch") || aliases[strings.ToLower(reference)] {
			return nil
		}
		key := ResolvedBaseKey{Reference: reference, Platform: platformString}
		if _, ok := selected[key]; ok {
			return nil
		}
		if options.Resolver == nil {
			return fmt.Errorf("external image %q requires a Coopr image resolver", reference)
		}
		platform, err := platforms.Parse(platformString)
		if err != nil {
			return err
		}
		source, err := SelectImageSource(ctx, options.Resolver, reference, platform)
		if err != nil {
			return err
		}
		selected[key] = source
		return nil
	}
	for _, stage := range stages {
		if stage.SourceContext == "" {
			if err := resolve(stage.Source, stage.Platform); err != nil {
				return nil, fmt.Errorf("select package stage %s base: %w", stage.ID, err)
			}
		}
		for index, operation := range stage.Operations {
			if operation.Name == "copy" || operation.Name == "add" {
				if operation.InputContext == "" {
					if err := resolve(operation.Properties["from"], stage.Platform); err != nil {
						return nil, fmt.Errorf("select package stage %s operation %d image: %w", stage.ID, index+1, err)
					}
				}
			}
			for mountIndex, mount := range operation.Children {
				if mount.Name != "mount" || len(mount.Arguments) != 1 ||
					(mount.Arguments[0] != "bind" && mount.Arguments[0] != "cache") {
					continue
				}
				if operation.MountContexts[mountIndex] == "" {
					if err := resolve(mount.Properties["from"], stage.Platform); err != nil {
						return nil, fmt.Errorf("select package stage %s operation %d RUN mount %d image: %w", stage.ID, index+1, mountIndex+1, err)
					}
				}
			}
		}
		if stage.Name != "" {
			aliases[strings.ToLower(stage.Name)] = true
		}
	}
	return selected, nil
}

func validatePublicationGraph(plan *planner.Plan, paths map[string]string) ([]planner.Stage, map[string]packageOutput, error) {
	if plan == nil {
		return nil, nil, errors.New("plan is nil")
	}
	if plan.Mode != planner.Publish || plan.DefinitionType != "component" || plan.Component == nil {
		return nil, nil, errors.New("direct Buildah publication requires a component publish plan")
	}
	if len(plan.Stages) == 0 && len(plan.Outputs) != 0 {
		return nil, nil, errors.New("direct Buildah publication outputs require package stages")
	}
	if _, err := executionPlatform(plan.Platform); err != nil {
		return nil, nil, fmt.Errorf("publication platform: %w", err)
	}
	externalInputs := make(map[string]bool, len(plan.Inputs))
	namedContexts := make(map[string]bool, len(plan.Contexts))
	for _, input := range plan.Contexts {
		name := strings.ToLower(input.Name)
		if name == "" || namedContexts[name] {
			return nil, nil, fmt.Errorf("plan contains invalid or duplicate named context %q", input.Name)
		}
		namedContexts[name] = true
	}
	for _, input := range plan.Inputs {
		switch input.Kind {
		case "image":
			externalInputs[input.Reference] = true
		case "component":
			// Resolved and verified at its graph position by the shared executor.
		case "context":
			if !namedContexts[strings.ToLower(input.Reference)] {
				return nil, nil, fmt.Errorf("plan context input %q has no materialization specification", input.Reference)
			}
		default:
			return nil, nil, fmt.Errorf("publication input kind %q is not supported", input.Kind)
		}
	}

	knownIDs := make(map[string]planner.Stage, len(plan.Stages))
	stagePlatforms := make(map[string]string, len(plan.Stages))
	aliases := make(map[string]string, len(plan.Stages))
	for _, stage := range plan.Stages {
		if stage.Kind != "from" && stage.Kind != "package" {
			return nil, nil, fmt.Errorf("publication stage %s kind %q is not supported", stage.ID, stage.Kind)
		}
		if stage.Kind == "package" && stage.Source != "" {
			return nil, nil, fmt.Errorf("package stage %s must not have a base source", stage.ID)
		}
		if _, err := executionPlatform(stage.Platform); err != nil {
			return nil, nil, fmt.Errorf("stage %s platform: %w", stage.ID, err)
		}
		if _, exists := knownIDs[stage.ID]; exists {
			return nil, nil, fmt.Errorf("duplicate stage ID %q", stage.ID)
		}
		baseID := ""
		if stage.SourceContext != "" {
			if !namedContexts[strings.ToLower(stage.SourceContext)] {
				return nil, nil, fmt.Errorf("stage %s named context %q is unavailable", stage.ID, stage.SourceContext)
			}
		} else if stage.Kind == "from" && !strings.EqualFold(stage.Source, "scratch") {
			baseID = aliases[strings.ToLower(stage.Source)]
			if baseID == "" && !externalInputs[stage.Source] {
				return nil, nil, fmt.Errorf("stage %s external base %q lacks a declared image input", stage.ID, stage.Source)
			}
			if baseID != "" && stagePlatforms[baseID] != stage.Platform {
				return nil, nil, fmt.Errorf("stage %s platform %q differs from base stage %s platform %q", stage.ID, stage.Platform, baseID, stagePlatforms[baseID])
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
						return nil, nil, fmt.Errorf("stage %s operation %d: %s references unavailable named context %q", stage.ID, operationIndex+1, reference.name, reference.context)
					}
					continue
				}
				sourceID := aliases[strings.ToLower(reference.source)]
				if sourceID == "" {
					if externalInputs[reference.source] {
						continue
					}
					return nil, nil, fmt.Errorf("stage %s operation %d: %s references an unavailable stage %q", stage.ID, operationIndex+1, reference.name, reference.source)
				}
				if !slices.Contains(wantDependencies, sourceID) {
					wantDependencies = append(wantDependencies, sourceID)
				}
			}
		}
		if !slices.Equal(stage.Dependencies, wantDependencies) {
			return nil, nil, fmt.Errorf("stage %s dependencies %v do not match source dependencies %v", stage.ID, stage.Dependencies, wantDependencies)
		}
		knownIDs[stage.ID] = stage
		stagePlatforms[stage.ID] = stage.Platform
		if stage.Name != "" {
			aliases[strings.ToLower(stage.Name)] = stage.ID
		}
	}

	outputs := make(map[string]packageOutput, len(plan.Outputs))
	usedPathKeys := make(map[string]bool, len(plan.Outputs))
	usedPaths := make(map[string]string, len(plan.Outputs))
	for _, outputID := range plan.Outputs {
		stage, ok := knownIDs[outputID]
		if !ok || stage.Kind != "package" {
			return nil, nil, fmt.Errorf("publication output %q does not identify a package stage", outputID)
		}
		if stage.Platform != plan.Platform {
			return nil, nil, fmt.Errorf("package output %q platform %q differs from publication platform %q", outputID, stage.Platform, plan.Platform)
		}
		key := stage.Name
		if key == "" {
			key = stage.ID
		}
		path, byName := paths[key]
		if pathByID, byID := paths[stage.ID]; byID && stage.ID != key {
			if byName {
				return nil, nil, fmt.Errorf("package %q has paths keyed by both name and ID", key)
			}
			path = pathByID
			usedPathKeys[stage.ID] = true
		} else if byName {
			usedPathKeys[key] = true
		}
		if path == "" {
			return nil, nil, fmt.Errorf("package %q output path is required", key)
		}
		if !filepath.IsAbs(path) {
			return nil, nil, fmt.Errorf("package %q output path must be absolute: %q", key, path)
		}
		clean := filepath.Clean(path)
		if previous := usedPaths[clean]; previous != "" {
			return nil, nil, fmt.Errorf("packages %q and %q share output path %q", previous, key, clean)
		}
		usedPaths[clean] = key
		outputs[outputID] = packageOutput{key: key, path: clean}
	}
	for key := range paths {
		if !usedPathKeys[key] {
			return nil, nil, fmt.Errorf("package output path key %q does not identify a required package", key)
		}
	}
	return slices.Clone(plan.Stages), outputs, nil
}
