package buildah

import (
	"fmt"
	"maps"
	"strings"

	"github.com/strio-dev/coopr/internal/planner"
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

// stageAliases preserves the source-order scope even if a later independent
// stage completes before an earlier dependency chain.
func stageAliases(stages []planner.Stage) []map[string]string {
	aliases := make([]map[string]string, len(stages))
	prior := make(map[string]string, len(stages))
	for index, stage := range stages {
		aliases[index] = maps.Clone(prior)
		prior[stage.ID] = stage.ID
		if stage.Name != "" {
			prior[strings.ToLower(stage.Name)] = stage.ID
		}
	}
	return aliases
}
