package planner

import (
	"fmt"
	"strconv"
	"strings"

	"coopr/internal/definition"
)

func (g *graph) mapExternalInput(kind, source string, packagePhase bool) (string, error) {
	if g.opts.MapExternalInput == nil {
		return source, nil
	}
	mapped, err := g.opts.MapExternalInput(kind, source)
	if err != nil {
		return "", fmt.Errorf("map external %s %q: %w", kind, source, err)
	}
	if mapped == source {
		return source, nil
	}
	if mapped == "" || strings.ContainsAny(mapped, " \t\r\n$\x00") || definition.IsLocalComponentReference(mapped) {
		return "", fmt.Errorf("external %s %q mapped to invalid external reference %q", kind, source, mapped)
	}
	if kind == "image" {
		_, stage := g.resolveStageReference(mapped)
		_, context := g.contextForSource(mapped, g.contextsAvailable(packagePhase))
		_, numericErr := strconv.Atoi(mapped)
		if stage || context || numericErr == nil || strings.EqualFold(mapped, "scratch") || g.reservedStageNames[canonicalStageName(mapped)] {
			return "", fmt.Errorf("external image %q mapped to local source %q", source, mapped)
		}
	}
	return mapped, nil
}
