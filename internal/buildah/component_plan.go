package buildah

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/strio-dev/coopr/internal/buildcontext"
	"github.com/strio-dev/coopr/internal/imageconfig"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/planner"

	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// ComponentPlanRequest identifies one published component invocation. Resolver
// performs OCI selection and verification before the planner sees metadata.
type ComponentPlanRequest struct {
	Resolver  *oci.Resolver
	Reference string
	// Selection freezes the root, platform manifest and delivery context selected by the coordinator.
	Selection     *oci.ComponentSelection
	DeliveryScope *oci.DeliveryScope
	Parameters    map[string]string
	// CallerEnvironment supplies only the current caller image ENV to EXTEND.
	CallerEnvironment map[string]string
	// LocalParameters permits matching publication-time arguments for a local definition.
	LocalParameters bool
	Platform        v1.Platform
	// ResolveBase selects an invocation-only external FROM in the same image
	// store used by graph execution. The selected image is pinned for this build.
	DeferImageSource   func(string) (bool, error)
	ResolveBase        imageBaseResolver
	ResolveFiles       imageBaseResolver
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

	resolved, err := request.Resolver.ResolveComponent(ctx, request.Reference, request.Platform, request.Selection, request.DeliveryScope)
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

	packageBinds, err := oci.ComponentPackageBinds(*resolved.Component)
	if err != nil {
		return nil, fmt.Errorf("component %q: %w", request.Reference, err)
	}
	selectedBases := newSelectedBaseState()
	resolvedBinds := make(map[ResolvedBaseKey]planner.StageBind)
	plan, err := planner.InstantiateDemandDriven(&resolved.Component.Component, planner.Options{
		DeferImageSource: request.DeferImageSource, Mode: planner.Invoke, Arguments: parameters, Platform: platform,
		CallerEnvironment:     request.CallerEnvironment,
		MapExternalInput:      resolved.Delivery.MapExternalInput,
		AwaitComponentReturns: true,
		TransientRunMounts:    TransientMountInstructions(request.TransientRunMounts),
		BuildContexts:         request.BuildContexts,
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
			} else if source.Kind == planner.FromSourceCopyImage && source.Role == "copy" && request.ResolveFiles != nil {
				base, err = request.ResolveFiles(ctx, source.Source, basePlatform)
			} else {
				base, err = request.ResolveBase(ctx, source.Source, basePlatform)
			}
			if err != nil {
				return planner.StageBind{}, fmt.Errorf("stage %s image %q: %w", source.StageID, source.Source, err)
			}
			selectedBases.record(key, base)
		}
		if err := validateSelectedInput(base, source.Kind == planner.FromSourceCopyImage && source.Role == "copy"); err != nil {
			return planner.StageBind{}, fmt.Errorf("stage %s source %q: %w", source.StageID, source.Source, err)
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

	inputs, err := componentPackageInputs(plan, resolved.Component.Packages)
	if err != nil {
		return nil, fmt.Errorf("component %q: %w", request.Reference, err)
	}
	return &ResolvedComponentPlan{
		Identity: resolved.Selected.Digest, Artifact: resolved, Plan: plan, PackageInputs: inputs, SelectedBases: selectedBases.all,
		replannedBaseDelta: selectedBases.takeDelta,
	}, nil
}

// componentPackageInputs selects verified artifact packages for the current plan. A
// runtime replan can activate packages that were dormant in the initial plan.
func componentPackageInputs(plan *planner.Plan, packages []oci.Package) (map[string]oci.Package, error) {
	packageStages := make(map[string]string)
	for _, stage := range plan.Stages {
		if stage.Kind != "package-input" {
			continue
		}
		name := strings.ToLower(stage.Name)
		if name == "" {
			return nil, fmt.Errorf("package-input stage %s has no name", stage.ID)
		}
		if _, exists := packageStages[name]; exists {
			return nil, fmt.Errorf("duplicate package-input stage %q", stage.Name)
		}
		packageStages[name] = stage.ID
	}
	inputs := make(map[string]oci.Package, len(packageStages))
	for _, pkg := range packages {
		stageID, ok := packageStages[strings.ToLower(pkg.Stage)]
		if !ok {
			continue // Dormant packages are verified but need not be imported.
		}
		if _, exists := inputs[stageID]; exists {
			return nil, fmt.Errorf("package stage %s is bound more than once", stageID)
		}
		inputs[stageID] = cloneComponentPackage(pkg)
	}
	if len(inputs) != len(packageStages) {
		return nil, fmt.Errorf("package inputs do not match invocation stages")
	}
	return inputs, nil
}

func componentPlatformFromString(value string) (v1.Platform, error) {
	if strings.Count(value, "/") < 1 || strings.Count(value, "/") > 2 || strings.HasSuffix(value, "/") {
		return v1.Platform{}, fmt.Errorf("invalid Linux platform %q", value)
	}
	platform, err := platforms.Parse(value)
	if err != nil || platform.OS != "linux" || platform.Architecture == "" || platform.OSVersion != "" || len(platform.OSFeatures) != 0 {
		return v1.Platform{}, fmt.Errorf("invalid Linux platform %q", value)
	}
	return platforms.Normalize(platform), nil
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
	return platforms.Format(platforms.Normalize(platform)), nil
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
