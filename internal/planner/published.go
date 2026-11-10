package planner

import (
	"fmt"
	"strings"

	"github.com/containerd/platforms"
)

// ValidatePublished admits only the new original-namespace residual format.
// It never reconstructs topology without invocation arguments. Package bodies
// are opaque snapshot leaves and cannot contain executable producers.
func ValidatePublished(component *PublishedComponent) ([]string, error) {
	projected, err := ProjectPublished(component)
	if err != nil {
		return nil, err
	}
	packages := make([]string, 0, len(projected.PackageLeaves))
	for _, id := range projected.PackageLeaves {
		packages = append(packages, projected.Stages[mustStageIndex(id)].Name)
	}
	return packages, nil
}

func validatePublished(component *PublishedComponent, projected *RawGraph) error {
	validOutput := false
	for _, root := range projected.Roots {
		if projected.Stages[mustStageIndex(root)].Kind == "extend" && projected.Results[root] == component.Output {
			validOutput = true
		}
	}
	if !validOutput || projected.Stages[mustStageIndex(component.Output)].Head.PrunedFrom {
		return fmt.Errorf("invalid published output %q", component.Output)
	}

	for _, stage := range projected.Stages {
		if stage.Kind != "package" {
			if stage.Head.UnavailablePackage {
				return fmt.Errorf("unavailable marker requires PACKAGE")
			}
			continue
		}
		if stage.Head.UnavailablePackage {
			if len(stage.Body) != 0 {
				return fmt.Errorf("unavailable package contains producer instructions")
			}
			continue
		}
		seen := map[string]bool{}
		for _, inst := range stage.Body {
			if inst.Name != "arg" || len(inst.Arguments) != 2 {
				return fmt.Errorf("package %s contains producer instructions", stage.ID)
			}
			name := inst.Arguments[0]
			value, ok := component.PackageArguments[name]
			if !ok || seen[name] || value != inst.Arguments[1] {
				return fmt.Errorf("invalid package argument %q", name)
			}
			seen[name] = true
		}
		if len(seen) != len(component.PackageArguments) {
			return fmt.Errorf("package argument declarations missing")
		}
		projected.PackageLeaves = append(projected.PackageLeaves, stage.ID)
	}
	if len(component.PackageArguments) != 0 && len(projected.PackageLeaves) == 0 {
		return fmt.Errorf("fixed package arguments require a captured package")
	}
	return nil
}

func samePlatform(a, b string) bool {
	left, leftErr := platforms.Parse(a)
	right, rightErr := platforms.Parse(b)
	return leftErr == nil && rightErr == nil && platforms.FormatAll(platforms.Normalize(left)) == platforms.FormatAll(platforms.Normalize(right))
}

// Reuse the selected closure; a raw substitution or returned configuration keeps
// the full program. Unknown FROM configuration can only reference earlier stages,
// so keep that lexical prefix too (including caller named-context overrides).
func (g *graph) prunableFromBodies(reachable []int) map[int]bool {
	selected := make(map[int]bool, len(reachable))
	latestFrom := 0
	for _, id := range reachable {
		selected[id] = true
		raw := g.raw[id]
		if raw.head.Name == "package" {
			continue // Snapshot producers are not invocation instructions.
		}
		if strings.Contains(raw.head.Properties["after"], "$") || raw.head.Name == "from" && strings.Contains(raw.head.Arguments[0], "$") {
			return nil
		}
		if raw.head.Name == "from" {
			latestFrom = max(latestFrom, g.projection.Stages[id].headPosition)
		}
		for _, inst := range raw.body {
			if inst.Name == "onbuild" || inst.Name == "component" || strings.Contains(inst.Properties["from"], "$") {
				return nil
			}
			for _, child := range inst.Children {
				if strings.Contains(child.Properties["from"], "$") || child.Name == "mount" && len(child.Arguments) == 1 && strings.Contains(child.Arguments[0], "$") {
					return nil
				}
			}
		}
	}
	pruned := map[int]bool{}
	for id, stage := range g.projection.Stages {
		if stage.Kind != "from" || selected[id] || stage.endPosition < latestFrom {
			continue
		}
		// SOURCE_DATE_EPOCH can inspect an otherwise unreachable ADD recipe by
		// name, independently of graph edges and with different invocation ARGs.
		hasAdd := false
		for _, inst := range stage.Body {
			hasAdd = hasAdd || inst.Name == "add"
		}
		if !hasAdd {
			pruned[id] = true
		}
	}
	return pruned
}
