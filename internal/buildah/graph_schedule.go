package buildah

import (
	"fmt"
	"maps"
	"strings"

	"coopr/internal/planner"
)

func graphParallelism(jobs, stages int) int {
	if jobs > 0 {
		return jobs
	}
	return max(1, stages)
}

func graphStageLabel(stage planner.Stage) string {
	label := stage.ID
	if stage.Name != "" {
		label = fmt.Sprintf("%s (%s)", stage.Name, stage.ID)
	}
	if stage.Platform != "" {
		label += " [" + stage.Platform + "]"
	}
	return label
}

// readyGraphStages returns dependency-ready unscheduled stages in planner
// order. Running stages do not satisfy dependencies; downstream work becomes
// ready immediately after each dependency completion event.
func readyGraphStages(stages []planner.Stage, completed, running map[string]bool, limit int) ([]int, error) {
	if limit < 1 {
		return nil, nil
	}
	known := make(map[string]bool, len(stages))
	for _, stage := range stages {
		if known[stage.ID] {
			return nil, fmt.Errorf("duplicate stage ID %q", stage.ID)
		}
		known[stage.ID] = true
	}
	ready := make([]int, 0, limit)
	for index, stage := range stages {
		if completed[stage.ID] || running[stage.ID] {
			continue
		}
		eligible := true
		for _, dependency := range stage.Dependencies {
			if !known[dependency] {
				return nil, fmt.Errorf("stage %s has unresolved dependency %s", stage.ID, dependency)
			}
			if !completed[dependency] {
				eligible = false
			}
		}
		if eligible {
			ready = append(ready, index)
			if len(ready) == limit {
				break
			}
		}
	}
	return ready, nil
}

// stageAliases preserves the source-order scope even if a later independent
// stage completes before an earlier dependency chain.
func stageAliases(stages []planner.Stage) []map[string]string {
	aliases := make([]map[string]string, len(stages))
	prior := make(map[string]string, len(stages))
	for index, stage := range stages {
		aliases[index] = maps.Clone(prior)
		if stage.Name != "" {
			prior[strings.ToLower(stage.Name)] = stage.ID
		}
	}
	return aliases
}
