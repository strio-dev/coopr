package buildah

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"coopr/internal/buildcontext"
	"coopr/internal/imageconfig"
	"coopr/internal/oci"
	"coopr/internal/planner"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// ComponentPlanRequest identifies one published component invocation. Resolver
// performs OCI selection and verification before the planner sees metadata.
type ComponentPlanRequest struct {
	Resolver   *oci.Resolver
	Reference  string
	Parameters map[string]string
	// LocalParameters permits matching publication-time arguments for a local definition.
	LocalParameters bool
	Platform        v1.Platform
	// ResolveBase selects an invocation-only external FROM in the same image
	// store used by graph execution. The selected image is pinned for this build.
	DeferImageSource   func(string) (bool, error)
	ResolveBase        imageBaseResolver
	TransientRunMounts []RunMount
	BuildContexts      []buildcontext.Spec
	ResolveContext     namedContextResolver
}

// ResolvedComponentPlan contains the immutable artifact identity, its verified
// resolution for package downloads, the instantiated invocation graph, and
// package inputs indexed by the graph's package-input stage ID.
type ResolvedComponentPlan struct {
	Identity           digest.Digest
	Artifact           *oci.Resolved
	Plan               *planner.Plan
	PackageInputs      map[string]oci.Package
	SelectedBases      map[ResolvedBaseKey]ResolvedImageSource
	replannedBaseDelta func() map[ResolvedBaseKey]ResolvedImageSource
	// localSourceIdentity is frozen session metadata, not OCI/cache identity.
	localSourceIdentity string
}

// ResolveComponentPlan resolves and verifies one selected OCI component
// manifest, instantiates its frozen publication metadata with caller
// parameters, and binds each verified package descriptor to its invocation
// stage. It does not download, import, or execute package contents.
func ResolveComponentPlan(ctx context.Context, request ComponentPlanRequest) (*ResolvedComponentPlan, error) {
	if ctx == nil {
		return nil, errors.New("component plan context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.Resolver == nil {
		return nil, errors.New("component resolver is required")
	}
	if strings.TrimSpace(request.Reference) == "" {
		return nil, errors.New("component reference is required")
	}
	platform, err := componentPlatformString(request.Platform)
	if err != nil {
		return nil, err
	}

	resolved, err := request.Resolver.Resolve(ctx, request.Reference, request.Platform, oci.Component)
	if err != nil {
		return nil, fmt.Errorf("resolve component %q: %w", request.Reference, err)
	}
	if resolved.Component == nil {
		return nil, fmt.Errorf("component %q has no verified publication metadata", request.Reference)
	}
	if resolved.Selected.Digest.Algorithm() != digest.SHA256 || resolved.Selected.Digest.Validate() != nil {
		return nil, fmt.Errorf("component %q selected an invalid artifact digest", request.Reference)
	}

	parameters, err := componentInvocationParameters(resolved.Component.Component.PackageArguments, request.Parameters, request.LocalParameters)
	if err != nil {
		return nil, fmt.Errorf("component %q: %w", request.Reference, err)
	}

	projection, err := planner.ProjectPublished(&resolved.Component.Component)
	if err != nil {
		return nil, fmt.Errorf("inspect component %q invocation graph: %w", request.Reference, err)
	}
	packagesByName := make(map[string]oci.Package, len(resolved.Component.Packages))
	for _, pkg := range resolved.Component.Packages {
		packagesByName[strings.ToLower(pkg.Stage)] = pkg
	}
	packageBinds := make(map[string]planner.StageBind)
	for _, stage := range projection.Stages {
		if stage.Kind != "from" {
			continue
		}
		binding := projection.FromBindings[stage.ID]
		if binding.Kind != "stage" {
			continue
		}
		baseID, err := strconv.Atoi(binding.Stage)
		if err != nil || baseID < 0 || baseID >= len(projection.Stages) {
			return nil, fmt.Errorf("component %q has invalid base stage %q", request.Reference, binding.Stage)
		}
		base := projection.Stages[baseID]
		if base.Kind != "package" {
			continue
		}
		pkg, ok := packagesByName[strings.ToLower(base.Name)]
		if !ok {
			return nil, fmt.Errorf("component %q has no package for stage %q", request.Reference, base.Name)
		}
		config, err := imageconfig.Parse(pkg.Config)
		if err != nil {
			return nil, fmt.Errorf("component %q package %q configuration: %w", request.Reference, base.Name, err)
		}
		bind, err := stageBindFromImageConfig(config)
		if err != nil {
			return nil, fmt.Errorf("component %q package %q inherited instructions: %w", request.Reference, base.Name, err)
		}
		packageBinds[stage.ID] = bind
	}
	selectedBases := newSelectedBaseState()
	resolvedBinds := make(map[ResolvedBaseKey]planner.StageBind)
	plan, err := planner.InstantiateDemandDriven(&resolved.Component.Component, planner.Options{
		DeferImageSource: request.DeferImageSource, Mode: planner.Invoke, Arguments: parameters, Platform: platform,
		TransientRunMounts: TransientMountInstructions(request.TransientRunMounts),
		BuildContexts:      request.BuildContexts,
	}, packageBinds, func(source planner.FromSource) (planner.StageBind, error) {
		if request.ResolveBase == nil {
			return planner.StageBind{}, fmt.Errorf("external invocation base %q requires an image resolver", source.Source)
		}
		key := ResolvedBaseKey{Reference: source.Source, Platform: source.Platform}
		if source.Kind == planner.FromSourceContext {
			key = graphNamedContextKey(source.Source, source.Platform)
		}
		base, found := selectedBases.all[key]
		if !found {
			basePlatform, err := componentPlatformFromString(source.Platform)
			if err != nil {
				return planner.StageBind{}, fmt.Errorf("stage %s platform: %w", source.StageID, err)
			}
			if source.Kind == planner.FromSourceContext {
				if request.ResolveContext == nil || source.Context == nil {
					return planner.StageBind{}, fmt.Errorf("named context %q requires a resolver", source.Source)
				}
				base, err = request.ResolveContext(ctx, *source.Context, basePlatform)
			} else {
				base, err = request.ResolveBase(ctx, source.Source, basePlatform)
			}
			if err != nil {
				return planner.StageBind{}, fmt.Errorf("stage %s image %q: %w", source.StageID, source.Source, err)
			}
			if base.ImageID == "" {
				return planner.StageBind{}, fmt.Errorf("stage %s image %q has no selected image", source.StageID, source.Source)
			}
			selectedBases.record(key, base)
		}
		if source.Kind == planner.FromSourceCopyImage {
			return planner.StageBind{}, nil
		}
		if len(base.ConfigData) == 0 {
			return planner.StageBind{}, fmt.Errorf("stage %s base %q has no selected configuration", source.StageID, source.Source)
		}
		if bind, found := resolvedBinds[key]; found {
			return bind, nil
		}
		config, err := imageconfig.Parse(base.ConfigData)
		if err != nil {
			return planner.StageBind{}, fmt.Errorf("stage %s base %q configuration: %w", source.StageID, source.Source, err)
		}
		bind, err := stageBindFromImageConfig(config)
		if err != nil {
			return planner.StageBind{}, fmt.Errorf("stage %s base %q inherited instructions: %w", source.StageID, source.Source, err)
		}
		resolvedBinds[key] = bind
		return bind, nil
	})
	if err != nil {
		return nil, fmt.Errorf("instantiate component %q: %w", request.Reference, err)
	}
	selectedBases.finishInitialPlan()

	packageStages := make(map[string]string)
	for _, stage := range plan.Stages {
		if stage.Kind != "package-input" {
			continue
		}
		name := strings.ToLower(stage.Name)
		if name == "" {
			return nil, fmt.Errorf("component %q package-input stage %s has no name", request.Reference, stage.ID)
		}
		if _, exists := packageStages[name]; exists {
			return nil, fmt.Errorf("component %q has duplicate package-input stage %q", request.Reference, stage.Name)
		}
		packageStages[name] = stage.ID
	}
	inputs := make(map[string]oci.Package, len(packageStages))
	for _, pkg := range resolved.Component.Packages {
		stageID, ok := packageStages[strings.ToLower(pkg.Stage)]
		if !ok {
			continue // Dormant packages are verified but need not be imported.
		}
		if _, exists := inputs[stageID]; exists {
			return nil, fmt.Errorf("component %q package stage %s is bound more than once", request.Reference, stageID)
		}
		inputs[stageID] = cloneComponentPackage(pkg)
	}
	if len(inputs) != len(packageStages) {
		return nil, fmt.Errorf("component %q package inputs do not match invocation stages", request.Reference)
	}

	return &ResolvedComponentPlan{
		Identity: resolved.Selected.Digest, Artifact: resolved, Plan: plan, PackageInputs: inputs, SelectedBases: selectedBases.all,
		replannedBaseDelta: selectedBases.takeDelta,
	}, nil
}

func componentPlatformFromString(value string) (v1.Platform, error) {
	parts := strings.Split(value, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "linux" || parts[1] == "" {
		return v1.Platform{}, fmt.Errorf("invalid Linux platform %q", value)
	}
	platform := v1.Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) == 3 {
		platform.Variant = parts[2]
	}
	return platform, nil
}

func componentPlatformString(platform v1.Platform) (string, error) {
	if platform.OS != "linux" || platform.Architecture == "" {
		return "", errors.New("component platform must specify Linux and architecture")
	}
	if platform.OSVersion != "" || len(platform.OSFeatures) != 0 {
		return "", errors.New("component planner does not support OS version or feature platform fields")
	}
	for _, field := range []string{platform.OS, platform.Architecture, platform.Variant} {
		if strings.TrimSpace(field) != field || strings.ContainsAny(field, "/\x00") {
			return "", errors.New("component platform fields must not contain whitespace, slash, or NUL")
		}
	}
	result := platform.OS + "/" + platform.Architecture
	if platform.Variant != "" {
		result += "/" + platform.Variant
	}
	return result, nil
}

func cloneComponentPackage(pkg oci.Package) oci.Package {
	result := pkg
	result.Config = bytes.Clone(pkg.Config)
	result.Descriptor.URLs = slices.Clone(pkg.Descriptor.URLs)
	result.Descriptor.Annotations = maps.Clone(pkg.Descriptor.Annotations)
	result.Descriptor.Data = bytes.Clone(pkg.Descriptor.Data)
	if pkg.Descriptor.Platform != nil {
		platform := *pkg.Descriptor.Platform
		platform.OSFeatures = slices.Clone(pkg.Descriptor.Platform.OSFeatures)
		result.Descriptor.Platform = &platform
	}
	return result
}

// Local definitions are published with invocation properties. Fixed package
// arguments must match that publication and are omitted from invocation-only
// overrides, exactly as for a separately built component.
func componentInvocationParameters(fixed, supplied map[string]string, local bool) (map[string]string, error) {
	parameters := maps.Clone(supplied)
	if !local {
		return parameters, nil
	}
	for name, value := range fixed {
		if override, found := parameters[name]; found {
			if override != value {
				return nil, fmt.Errorf("local component package argument %q differs from its frozen value", name)
			}
			delete(parameters, name)
		}
	}
	return parameters, nil
}
