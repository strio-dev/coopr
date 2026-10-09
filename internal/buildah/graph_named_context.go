package buildah

import (
	"context"
	"fmt"
	"strings"

	"coopr/internal/buildcontext"
	"coopr/internal/planner"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const graphNamedContextPrefix = "\x00coopr-named-context:"

func graphNamedContextKey(name, platform string) ResolvedBaseKey {
	return ResolvedBaseKey{Reference: graphNamedContextPrefix + strings.ToLower(name), Platform: platform}
}

func (executor *graphExecutor) namedContext(name, platform string) (ResolvedImageSource, bool) {
	resolved, ok := executor.resolvedBases[graphNamedContextKey(name, platform)]
	return resolved, ok && resolved.ImageID != ""
}

// materializePlanContexts installs graph-local named contexts in the existing
// selected-image transport. The synthetic key is restored after this graph so
// recursive component execution cannot leak a same-named context outward.
func (executor *graphExecutor) materializePlanContexts(ctx context.Context, plan *planner.Plan) (func(), error) {
	if plan == nil || len(plan.Contexts) == 0 {
		return func() {}, nil
	}
	deferred := make(map[string]bool, len(plan.Stages))
	stagePlatforms := make(map[string]string, len(plan.Stages))
	for _, stage := range plan.Stages {
		stagePlatforms[stage.ID] = stage.Platform
		deferred[stage.ID] = stage.DeferredImageSource
	}
	type previousValue struct {
		key   ResolvedBaseKey
		value ResolvedImageSource
		found bool
	}
	previous := make([]previousValue, 0, len(plan.Contexts))
	restore := func() {
		for index := len(previous) - 1; index >= 0; index-- {
			item := previous[index]
			if item.found {
				executor.resolvedBases[item.key] = item.value
			} else {
				delete(executor.resolvedBases, item.key)
			}
		}
	}
	seen := make(map[string]bool, len(plan.Contexts))
	for _, input := range plan.Contexts {
		name := strings.ToLower(input.Name)
		if name == "" {
			restore()
			return nil, fmt.Errorf("plan contains a named context without a name")
		}
		if seen[name] {
			restore()
			return nil, fmt.Errorf("plan contains duplicate named context %q", input.Name)
		}
		seen[name] = true
		platformValues := map[string]bool{}
		for _, binding := range input.Bindings {
			if binding.Role == "from" && deferred[binding.StageID] {
				continue
			}
			platformValue := stagePlatforms[binding.StageID]
			if platformValue == "" {
				restore()
				return nil, fmt.Errorf("named context %q binding references unavailable stage %q", input.Name, binding.StageID)
			}
			platformValues[platformValue] = true
		}
		if len(platformValues) == 0 {
			if len(input.Bindings) > 0 {
				continue
			}
			platformValues[plan.Platform] = true
		}
		for platformValue := range platformValues {
			platform, err := executionPlatform(platformValue)
			if err != nil {
				restore()
				return nil, fmt.Errorf("named context %q platform: %w", input.Name, err)
			}
			key := graphNamedContextKey(name, platformValue)
			old, found := executor.resolvedBases[key]
			previous = append(previous, previousValue{key: key, value: old, found: found})
			if found && old.ImageID != "" {
				continue
			}
			resolved, err := executor.materializeNamedContext(ctx, input.Spec, platform)
			if err != nil {
				restore()
				return nil, fmt.Errorf("materialize named context %q for %s: %w", input.Name, platformValue, err)
			}
			executor.resolvedBases[key] = resolved
		}
	}
	return restore, nil
}

func (executor *graphExecutor) materializeNamedContext(ctx context.Context, spec buildcontext.Spec, platform v1.Platform) (ResolvedImageSource, error) {
	if spec.Kind == buildcontext.DockerImage {
		return resolveBaseSourceWithPolicy(ctx, executor.options.Resolver, spec.Reference, platform, executor.store, platformSystemContext(executor.system, platform), executor.options.ContextDir, executor.sourcePolicy)
	}
	return MaterializeNamedContext(ctx, spec, platform, executor.store, platformSystemContext(executor.system, platform), executor.options.Resolver, executor.options.Secrets, executor.options.SSH, executor.options.ContextArtifacts...)
}
