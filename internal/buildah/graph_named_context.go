package buildah

import (
	"context"
	"fmt"
	"strings"

	"coopr/internal/planner"
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
	stagePlatforms := make(map[string]string, len(plan.Stages))
	for _, stage := range plan.Stages {
		stagePlatforms[stage.ID] = stage.Platform
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
			platformValue := stagePlatforms[binding.StageID]
			if platformValue == "" {
				restore()
				return nil, fmt.Errorf("named context %q binding references unavailable stage %q", input.Name, binding.StageID)
			}
			platformValues[platformValue] = true
		}
		if len(platformValues) == 0 {
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
			resolved, err := MaterializeNamedContext(ctx, input.Spec, platform, executor.store, platformSystemContext(executor.system, platform), executor.options.Resolver, executor.options.Secrets, executor.options.SSH, executor.options.ContextArtifacts...)
			if err != nil {
				restore()
				return nil, fmt.Errorf("materialize named context %q for %s: %w", input.Name, platformValue, err)
			}
			executor.resolvedBases[key] = resolved
		}
	}
	return restore, nil
}
