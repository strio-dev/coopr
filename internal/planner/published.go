package planner

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"coopr/internal/definition"
)

// ValidatePublished checks the graph that can be checked without caller
// arguments. Instantiate checks the remaining expanded bindings at call time.
// It returns the exact package stage names required by the selected output.
func ValidatePublished(component *PublishedComponent) ([]string, error) {
	if component == nil || component.Definition == nil {
		return nil, fmt.Errorf("incomplete published component")
	}
	if err := definition.Validate(component.Definition); err != nil {
		return nil, err
	}
	if _, err := NormalizeParameters(component.PackageArguments); err != nil {
		return nil, err
	}
	if !slices.IsSorted(component.ReservedStageNames) || len(component.ReservedStageNames) != len(slices.Compact(slices.Clone(component.ReservedStageNames))) {
		return nil, fmt.Errorf("invalid published stage name reservations")
	}
	canonicalReservations := make(map[string]bool, len(component.ReservedStageNames))
	for _, name := range component.ReservedStageNames {
		canonical := canonicalStageName(name)
		if canonicalReservations[canonical] {
			return nil, fmt.Errorf("invalid published stage name reservations")
		}
		canonicalReservations[canonical] = true
	}
	type stage struct {
		head definition.Instruction
		body []definition.Instruction
	}
	var stages []stage
	aliases := map[string]int{}
	for _, inst := range component.Definition.Instructions {
		switch inst.Name {
		case "from", "extend", "package":
			if name := inst.Properties["as"]; name != "" {
				canonical := canonicalStageName(name)
				if _, exists := aliases[canonical]; exists || !canonicalReservations[canonical] {
					return nil, fmt.Errorf("invalid or unreserved stage name %q", name)
				}
				aliases[canonical] = len(stages)
			}
			stages = append(stages, stage{head: inst})
		default:
			if len(stages) == 0 {
				if inst.Name != "arg" {
					return nil, fmt.Errorf("instruction %q precedes first stage", inst.Name)
				}
				continue
			}
			stages[len(stages)-1].body = append(stages[len(stages)-1].body, inst)
		}
	}
	output, err := strconv.Atoi(component.Output)
	if err != nil || output < 0 || output >= len(stages) || stages[output].head.Name == "package" {
		return nil, fmt.Errorf("invalid published output %q", component.Output)
	}
	deps := make([][]int, len(stages))
	fromCount := 0
	referenceCount := 0
	for id, s := range stages {
		if s.head.Name == "from" {
			fromCount++
			binding, ok := component.FromBindings[strconv.Itoa(id)]
			if !ok {
				return nil, fmt.Errorf("published FROM binding missing for stage %d", id)
			}
			source := s.head.Arguments[0]
			switch binding.Kind {
			case "stage":
				target, parseErr := strconv.Atoi(binding.Stage)
				if parseErr != nil || target < 0 || target >= len(stages) || strconv.Itoa(target) != binding.Stage {
					return nil, fmt.Errorf("invalid FROM stage binding %d", id)
				}
				if binding.SourceIndex != "" {
					if !canonicalSourceIndex(binding.SourceIndex) || !strings.Contains(source, "$") && source != binding.SourceIndex {
						return nil, fmt.Errorf("FROM stage binding %d conflicts with numeric source %q", id, source)
					}
				} else if name := stages[target].head.Properties["as"]; name == "" || !strings.Contains(source, "$") && canonicalStageName(source) != canonicalStageName(name) {
					return nil, fmt.Errorf("FROM stage binding %d conflicts with source %q", id, source)
				}
				deps[id] = append(deps[id], target)
			case "scratch":
				if binding.Stage != "" || binding.SourceIndex != "" || !strings.Contains(source, "$") && !strings.EqualFold(source, "scratch") {
					return nil, fmt.Errorf("invalid scratch binding for stage %d", id)
				}
			case "image":
				if binding.Stage != "" || binding.SourceIndex != "" || !strings.Contains(source, "$") && strings.EqualFold(source, "scratch") {
					return nil, fmt.Errorf("invalid image binding for stage %d", id)
				}
				_, alias := aliases[canonicalStageName(source)]
				if !strings.Contains(source, "$") && (alias || canonicalReservations[canonicalStageName(source)]) {
					return nil, fmt.Errorf("image binding reuses stage name %q", source)
				}
			default:
				return nil, fmt.Errorf("invalid FROM binding kind %q", binding.Kind)
			}
		}
		operation := 0
		for _, inst := range s.body {
			if inst.Name == "arg" {
				continue
			}
			if (inst.Name == "copy" || inst.Name == "add") && inst.Properties["from"] != "" {
				referenceCount++
				if err := checkPublishedReference(component.StageReferences, &deps[id], aliases, component.ReservedStageNames, len(stages), id, operation, -1, inst.Properties["from"]); err != nil {
					return nil, err
				}
			}
			for child, mount := range inst.Children {
				if mount.Properties["from"] != "" {
					referenceCount++
					if err := checkPublishedReference(component.StageReferences, &deps[id], aliases, component.ReservedStageNames, len(stages), id, operation, child, mount.Properties["from"]); err != nil {
						return nil, err
					}
				}
			}
			operation++
		}
	}
	for _, ref := range component.StageReferences {
		if ref.Origin != "onbuild" {
			continue
		}
		referenceCount++
		stageID, err := strconv.Atoi(ref.Stage)
		if err != nil || stageID < 0 || stageID >= len(stages) || stages[stageID].head.Name != "from" {
			return nil, fmt.Errorf("ONBUILD stage reference has invalid source %q", ref.Stage)
		}
		if ref.Operation < 0 || ref.MountIndex < -1 || ref.SourceIndex != "" && !canonicalSourceIndex(ref.SourceIndex) {
			return nil, fmt.Errorf("invalid ONBUILD stage reference at stage %s operation %d", ref.Stage, ref.Operation)
		}
		if ref.Kind == "image" {
			if ref.Target != "" || ref.SourceIndex != "" {
				return nil, fmt.Errorf("invalid ONBUILD image reference target %q", ref.Target)
			}
			continue
		}
		if ref.Kind != "stage" {
			return nil, fmt.Errorf("invalid ONBUILD stage reference at stage %s operation %d", ref.Stage, ref.Operation)
		}
		target, err := strconv.Atoi(ref.Target)
		if err != nil || target < 0 || target >= len(stages) || strconv.Itoa(target) != ref.Target || ref.SourceIndex == "" && stages[target].head.Properties["as"] == "" {
			return nil, fmt.Errorf("invalid ONBUILD stage reference target %q", ref.Target)
		}
		packageBase := false
		ancestor := stageID
		for steps := 0; steps < len(stages) && stages[ancestor].head.Name == "from"; steps++ {
			binding := component.FromBindings[strconv.Itoa(ancestor)]
			if binding.Kind != "stage" {
				break
			}
			ancestor, _ = strconv.Atoi(binding.Stage)
			if stages[ancestor].head.Name == "package" {
				packageBase = true
				break
			}
		}
		if !packageBase {
			return nil, fmt.Errorf("ONBUILD stage reference %s is not inherited from a package", ref.Stage)
		}
		deps[stageID] = append(deps[stageID], target)
	}
	if len(component.FromBindings) != fromCount {
		return nil, fmt.Errorf("published FROM bindings do not match retained stages")
	}
	if len(component.StageReferences) != referenceCount {
		return nil, fmt.Errorf("published stage references do not match retained operations")
	}
	// StageReferences are checked by key in checkPublishedReference. Reject any
	// extra, duplicate, or out-of-order records too.
	lastStage, lastOrigin, lastOperation, lastMount := -1, -1, -1, -2
	for _, ref := range component.StageReferences {
		stageID, stageErr := strconv.Atoi(ref.Stage)
		origin := -1
		switch ref.Origin {
		case "onbuild":
			origin = 0
		case "authored":
			origin = 1
		}
		if stageErr != nil || stageID < 0 || strconv.Itoa(stageID) != ref.Stage || origin < 0 ||
			stageID < lastStage || stageID == lastStage &&
			(origin < lastOrigin || origin == lastOrigin &&
				(ref.Operation < lastOperation || ref.Operation == lastOperation && ref.MountIndex <= lastMount)) {
			return nil, fmt.Errorf("invalid stage reference order")
		}
		lastStage, lastOrigin, lastOperation, lastMount = stageID, origin, ref.Operation, ref.MountIndex
	}
	dormant := make([]int, 0, len(component.DormantRoots))
	lastDormant := -1
	for _, raw := range component.DormantRoots {
		id, parseErr := strconv.Atoi(raw)
		if parseErr != nil || id < 0 || id >= len(stages) || strconv.Itoa(id) != raw || id <= lastDormant || id == output {
			return nil, fmt.Errorf("invalid dormant root %q", raw)
		}
		dormant = append(dormant, id)
		lastDormant = id
	}
	seen := make([]int, len(stages))
	var visit func(int) error
	visit = func(id int) error {
		if seen[id] == 1 {
			return fmt.Errorf("cycle in published component graph")
		}
		if seen[id] == 2 {
			return nil
		}
		seen[id] = 1
		for _, dep := range deps[id] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		seen[id] = 2
		return nil
	}
	if err := visit(output); err != nil {
		return nil, err
	}
	selected := make([]bool, len(stages))
	for id := range stages {
		selected[id] = seen[id] == 2
	}
	for _, id := range dormant {
		if selected[id] {
			return nil, fmt.Errorf("dormant root %d is already in selected output closure", id)
		}
		eligible := false
		for source := range stages {
			binding, ok := component.FromBindings[strconv.Itoa(source)]
			if selected[source] && ok && binding.Kind == "image" {
				eligible = true
				break
			}
		}
		if !eligible {
			return nil, fmt.Errorf("dormant root %d has no selected external FROM", id)
		}
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	for id := range stages {
		if seen[id] != 2 {
			return nil, fmt.Errorf("retained stage %d is outside selected output closure", id)
		}
	}
	ancestor := output
	for stages[ancestor].head.Name == "from" {
		binding := component.FromBindings[strconv.Itoa(ancestor)]
		if binding.Kind != "stage" {
			break
		}
		ancestor, _ = strconv.Atoi(binding.Stage)
	}
	if stages[ancestor].head.Name != "extend" {
		return nil, fmt.Errorf("published output does not descend from extend")
	}
	var packages []string
	for id, s := range stages {
		if s.head.Name != "package" {
			continue
		}
		var packageDeps func(int, map[int]bool) error
		packageDeps = func(current int, visited map[int]bool) error {
			if stages[current].head.Name == "extend" {
				return fmt.Errorf("package depends on extend")
			}
			if visited[current] {
				return nil
			}
			visited[current] = true
			for _, dep := range deps[current] {
				if err := packageDeps(dep, visited); err != nil {
					return err
				}
			}
			return nil
		}
		if err := packageDeps(id, map[int]bool{}); err != nil {
			return nil, err
		}
		packages = append(packages, s.head.Properties["as"])
	}
	return packages, nil
}

func checkPublishedReference(refs []StageReferenceBinding, deps *[]int, aliases map[string]int, reserved []string, stageCount, stage, operation, mount int, source string) error {
	for _, ref := range refs {
		if ref.Origin != "authored" || ref.Stage != strconv.Itoa(stage) || ref.Operation != operation || ref.MountIndex != mount {
			continue
		}
		if ref.Kind == "image" {
			if ref.Target != "" || ref.SourceIndex != "" {
				return fmt.Errorf("invalid image stage reference metadata")
			}
			if !strings.Contains(source, "$") {
				if strings.EqualFold(source, "scratch") || containsStageName(reserved, source) {
					return fmt.Errorf("image stage reference %q changed from publication", source)
				}
				if _, err := strconv.Atoi(source); err == nil {
					return fmt.Errorf("numeric stage reference %q cannot be an image", source)
				}
			}
			return nil
		}
		if ref.Kind != "" && ref.Kind != "stage" {
			return fmt.Errorf("invalid stage reference kind %q", ref.Kind)
		}
		target, err := strconv.Atoi(ref.Target)
		if err != nil || target < 0 || target >= stageCount || strconv.Itoa(target) != ref.Target {
			return fmt.Errorf("invalid stage reference target %q", ref.Target)
		}
		aliased := false
		for _, id := range aliases {
			aliased = aliased || id == target
		}
		if !aliased && ref.SourceIndex == "" {
			return fmt.Errorf("stage reference target %q has no stage name", ref.Target)
		}
		if ref.SourceIndex != "" {
			if !canonicalSourceIndex(ref.SourceIndex) || !strings.Contains(source, "$") && source != ref.SourceIndex {
				return fmt.Errorf("stage reference %q changed from published numeric index", source)
			}
		} else if !strings.Contains(source, "$") {
			actual, ok := aliases[canonicalStageName(source)]
			if !ok || actual != target {
				return fmt.Errorf("stage reference %q changed from publication", source)
			}
		}
		*deps = append(*deps, target)
		return nil
	}
	return fmt.Errorf("published stage reference missing for stage %d operation %d", stage, operation)
}

func containsStageName(names []string, target string) bool {
	target = canonicalStageName(target)
	return slices.ContainsFunc(names, func(name string) bool {
		return canonicalStageName(name) == target
	})
}

func samePlatform(a, b string) bool {
	if a == b {
		return true
	}
	aParts, bParts := strings.Split(a, "/"), strings.Split(b, "/")
	if len(aParts) < 2 || len(aParts) > 3 || len(bParts) < 2 || len(bParts) > 3 {
		return false
	}
	if aParts[0] != bParts[0] || aParts[1] != bParts[1] || aParts[1] != "arm64" {
		return false
	}
	variant := func(parts []string) string {
		if len(parts) == 2 || parts[2] == "v8" {
			return ""
		}
		return parts[2]
	}
	return variant(aParts) == variant(bParts)
}

func canonicalSourceIndex(value string) bool {
	index, err := strconv.Atoi(value)
	return err == nil && index >= 0 && strconv.Itoa(index) == value
}
