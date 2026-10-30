package buildah

import (
	"context"
	"fmt"
	"maps"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/buildcontext"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/imageconfig"
	"github.com/strio-dev/coopr/internal/planner"
)

type imageBaseResolver func(context.Context, string, v1.Platform) (ResolvedImageSource, error)
type namedContextResolver func(context.Context, buildcontext.Spec, v1.Platform) (ResolvedImageSource, error)

type selectedBaseState struct {
	all      map[ResolvedBaseKey]ResolvedImageSource
	pending  map[ResolvedBaseKey]ResolvedImageSource
	tracking bool
}

func newSelectedBaseState() *selectedBaseState {
	return &selectedBaseState{all: make(map[ResolvedBaseKey]ResolvedImageSource), pending: make(map[ResolvedBaseKey]ResolvedImageSource)}
}

func (state *selectedBaseState) record(key ResolvedBaseKey, selected ResolvedImageSource) {
	state.all[key] = selected
	if state.tracking {
		state.pending[key] = selected
	}
}

func (state *selectedBaseState) finishInitialPlan() {
	state.tracking = true
}

func (state *selectedBaseState) takeDelta() map[ResolvedBaseKey]ResolvedImageSource {
	delta := maps.Clone(state.pending)
	clear(state.pending)
	return delta
}

// planDefinitionWithExternalBases binds image metadata when the selected graph
// first reaches each external FROM. The returned selections pin execution to
// the same immutable images whose metadata was used during planning.
func planDefinitionWithExternalBases(ctx context.Context, def *definition.Definition, opts planner.Options, resolve imageBaseResolver, resolveContext ...namedContextResolver) (*planner.Plan, map[ResolvedBaseKey]ResolvedImageSource, error) {
	plan, state, err := planDefinitionWithExternalBaseState(ctx, def, opts, resolve, resolveContext...)
	if err != nil {
		return nil, nil, err
	}
	return plan, state.all, nil
}

func planDefinitionWithExternalBaseState(ctx context.Context, def *definition.Definition, opts planner.Options, resolve imageBaseResolver, resolveContext ...namedContextResolver) (*planner.Plan, *selectedBaseState, error) {
	return planDefinitionWithInitialBindings(ctx, def, opts, false, resolve, resolveContext...)
}

// suppliedRecipe admits already validated caller bindings from a supplied Plan.
// Raw definitions retain the guard against replacing external image metadata.
func planDefinitionWithInitialBindings(ctx context.Context, def *definition.Definition, opts planner.Options, suppliedRecipe bool, resolve imageBaseResolver, resolveContext ...namedContextResolver) (*planner.Plan, *selectedBaseState, error) {
	return planDefinitionWithSourceResolvers(ctx, def, opts, suppliedRecipe, resolve, nil, resolveContext...)
}

func planDefinitionWithSourceResolvers(ctx context.Context, def *definition.Definition, opts planner.Options, suppliedRecipe bool, resolve, resolveFiles imageBaseResolver, resolveContext ...namedContextResolver) (*planner.Plan, *selectedBaseState, error) {
	if opts.Mode != planner.Build && opts.Mode != planner.Publish {
		return nil, nil, fmt.Errorf("external base binding requires build or publish mode, got %q", opts.Mode)
	}
	if resolve == nil {
		return nil, nil, fmt.Errorf("external base resolver is nil")
	}
	if opts.Platform != "" {
		if _, err := executionPlatform(opts.Platform); err != nil {
			return nil, nil, fmt.Errorf("build platform: %w", err)
		}
	}
	if len(opts.StageBinds) != 0 && !suppliedRecipe {
		from, err := planner.ResolveFromSources(def, opts)
		if err != nil {
			return nil, nil, err
		}
		for _, source := range from {
			if _, supplied := opts.StageBinds[source.StageID]; supplied && source.Kind == planner.FromSourceImage {
				return nil, nil, fmt.Errorf("stage %s has both a supplied bind and an external base", source.StageID)
			}
		}
	}
	selected := newSelectedBaseState()
	bindings := make(map[ResolvedBaseKey]planner.StageBind)
	plan, err := planner.CreateDemandDriven(def, opts, func(source planner.FromSource) (planner.StageBind, error) {
		if err := ctx.Err(); err != nil {
			return planner.StageBind{}, err
		}
		key := ResolvedBaseKey{Reference: source.Source, Platform: source.Platform}
		if source.Kind == planner.FromSourceContext {
			key = graphNamedContextKey(source.Source, source.Platform)
		}
		base, found := selected.all[key]
		if !found {
			platform, err := executionPlatform(source.Platform)
			if err != nil {
				return planner.StageBind{}, fmt.Errorf("stage %s platform: %w", source.StageID, err)
			}
			if source.Kind == planner.FromSourceContext {
				if source.Context == nil || len(resolveContext) != 1 || resolveContext[0] == nil {
					return planner.StageBind{}, fmt.Errorf("stage %s named context %q has no resolver", source.StageID, source.Source)
				}
				base, err = resolveContext[0](ctx, *source.Context, platform)
			} else if source.Kind == planner.FromSourceCopyImage && source.Role == "copy" && resolveFiles != nil {
				base, err = resolveFiles(ctx, source.Source, platform)
			} else {
				base, err = resolve(ctx, source.Source, platform)
			}
			if err != nil {
				return planner.StageBind{}, fmt.Errorf("stage %s image %q: %w", source.StageID, source.Source, err)
			}
			selected.record(key, base)
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
		if binding, found := bindings[key]; found {
			return binding, nil
		}
		config, err := imageconfig.Parse(base.ConfigData)
		if err != nil {
			return planner.StageBind{}, fmt.Errorf("stage %s base %q configuration: %w", source.StageID, source.Source, err)
		}
		binding, err := stageBindFromImageConfig(config)
		if err != nil {
			return planner.StageBind{}, fmt.Errorf("stage %s base %q inherited instructions: %w", source.StageID, source.Source, err)
		}
		bindings[key] = binding
		return binding, nil
	})
	if err != nil {
		return nil, nil, err
	}
	selected.finishInitialPlan()
	return plan, selected, nil
}
