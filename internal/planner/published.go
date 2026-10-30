package planner

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/containerd/platforms"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/onbuildparse"
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
	projected, err := projectRaw(component.Definition)
	if err != nil {
		return nil, err
	}
	type stage struct {
		head definition.Instruction
		body []definition.Instruction
	}
	stages := make([]stage, len(projected.Stages))
	aliases := map[string]int{}
	for name, id := range projected.Aliases {
		if !canonicalReservations[name] {
			return nil, fmt.Errorf("invalid or unreserved stage name %q", name)
		}
		aliases[name] = mustStageIndex(id)
	}
	for id, s := range projected.Stages {
		stages[id] = stage{head: s.Head, body: s.Body}
	}
	if component.SourceStageCount < len(stages) || len(component.SourceStageIDs) != len(stages) {
		return nil, fmt.Errorf("invalid original stage identity mapping")
	}
	mappedIDs := map[string]bool{}
	originalIDs := make([]int, 0, len(stages))
	for original, mapped := range component.SourceStageIDs {
		index, err := strconv.Atoi(original)
		target, targetErr := strconv.Atoi(mapped)
		if err != nil || index < 0 || index >= component.SourceStageCount || strconv.Itoa(index) != original || targetErr != nil || target < 0 || target >= len(stages) || strconv.Itoa(target) != mapped || mappedIDs[mapped] {
			return nil, fmt.Errorf("invalid original stage identity mapping")
		}
		mappedIDs[mapped] = true
		originalIDs = append(originalIDs, index)
	}
	slices.Sort(originalIDs)
	for index, original := range originalIDs {
		if component.SourceStageIDs[strconv.Itoa(original)] != strconv.Itoa(index) {
			return nil, fmt.Errorf("original stage identity mapping changes source order")
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
			after, hasAfter := s.head.Properties["after"]
			if hasAfter {
				target, err := strconv.Atoi(binding.AfterStage)
				if err != nil || target < 0 || target >= id || strconv.Itoa(target) != binding.AfterStage {
					return nil, fmt.Errorf("invalid FROM after binding for stage %d", id)
				}
				if binding.AfterIndex != "" {
					if component.SourceStageIDs[binding.AfterIndex] != binding.AfterStage {
						return nil, fmt.Errorf("FROM after binding conflicts with original stage identity")
					}
					if !canonicalSourceIndex(binding.AfterIndex) || !strings.Contains(after, "$") && after != binding.AfterIndex {
						return nil, fmt.Errorf("FROM after binding conflicts with numeric source %q", after)
					}
				} else if !strings.Contains(after, "$") && aliases[canonicalStageName(after)] != target || !strings.Contains(after, "$") && projected.Aliases[canonicalStageName(after)] == "" {
					return nil, fmt.Errorf("FROM after binding conflicts with source %q", after)
				}
				if err := projected.requireEarlier(id, target, -1, after); err != nil {
					return nil, err
				}
				deps[id] = append(deps[id], target)
			} else if binding.AfterStage != "" || binding.AfterIndex != "" {
				return nil, fmt.Errorf("unexpected FROM after binding for stage %d", id)
			}
			source := s.head.Arguments[0]
			switch binding.Kind {
			case "stage":
				target, parseErr := strconv.Atoi(binding.Stage)
				if parseErr != nil || target < 0 || target >= len(stages) || strconv.Itoa(target) != binding.Stage {
					return nil, fmt.Errorf("invalid FROM stage binding %d", id)
				}
				if binding.SourceIndex != "" {
					if component.SourceStageIDs[binding.SourceIndex] != binding.Stage {
						return nil, fmt.Errorf("FROM stage binding conflicts with original stage identity")
					}
					if !canonicalSourceIndex(binding.SourceIndex) || !strings.Contains(source, "$") && source != binding.SourceIndex {
						return nil, fmt.Errorf("FROM stage binding %d conflicts with numeric source %q", id, source)
					}
				} else if !strings.Contains(source, "$") && aliases[canonicalStageName(source)] != target || !strings.Contains(source, "$") && projected.Aliases[canonicalStageName(source)] == "" {
					return nil, fmt.Errorf("FROM stage binding %d conflicts with source %q", id, source)
				}
				if err := projected.requireEarlier(id, target, -1, source); err != nil {
					return nil, err
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
				if err := checkPublishedReference(component.StageReferences, &deps[id], aliases, component.ReservedStageNames, projected, id, operation, -1, inst.Properties["from"], operation); err != nil {
					return nil, err
				}
			}
			for child, mount := range inst.Children {
				if mount.Properties["from"] != "" {
					referenceCount++
					if err := checkPublishedReference(component.StageReferences, &deps[id], aliases, component.ReservedStageNames, projected, id, operation, child, mount.Properties["from"], operation); err != nil {
						return nil, err
					}
				}
			}
			operation++
		}
	}
	// A local FROM's authored ONBUILD is part of the retained source. Unlike
	// external triggers, its reference positions can be checked before execution.
	scratchLineage := func(id int) bool {
		for steps := 0; steps < len(stages); steps++ {
			if stages[id].head.Name != "from" {
				return false
			}
			for _, inst := range stages[id].body {
				if inst.Name == "component" {
					return false
				}
			}
			binding := component.FromBindings[strconv.Itoa(id)]
			if binding.Kind == "scratch" {
				return true
			}
			if binding.Kind != "stage" {
				return false
			}
			id = mustStageIndex(binding.Stage)
		}
		return false
	}
	localTriggers := map[int][]definition.Instruction{}
	for id := range stages {
		binding, ok := component.FromBindings[strconv.Itoa(id)]
		if !ok || binding.Kind != "stage" {
			continue
		}
		base := mustStageIndex(binding.Stage)
		if stages[base].head.Name != "from" {
			continue
		}
		claimed := slices.ContainsFunc(component.StageReferences, func(ref StageReferenceBinding) bool {
			return ref.Origin == "local-onbuild" && ref.Stage == strconv.Itoa(id)
		})
		if !claimed && !scratchLineage(base) {
			continue
		}
		if slices.ContainsFunc(stages[base].body, func(inst definition.Instruction) bool { return inst.Name == "component" }) {
			continue // A component can replace the image's final ONBUILD configuration.
		}
		for _, inst := range stages[base].body {
			if inst.Name != "onbuild" {
				continue
			}
			parsed, err := onbuildparse.ParseDeferred(inst.Arguments[0])
			if err != nil {
				return nil, fmt.Errorf("local ONBUILD at stage %d: %w", base, err)
			}
			for _, trigger := range parsed {
				if trigger.Name != "arg" {
					localTriggers[id] = append(localTriggers[id], trigger)
				}
			}
		}
		for operation, trigger := range localTriggers[id] {
			check := func(source string, mount int) error {
				if source == "" {
					return nil
				}
				for _, ref := range component.StageReferences {
					if ref.Origin == "local-onbuild" && ref.Stage == strconv.Itoa(id) && ref.Operation == operation && ref.MountIndex == mount {
						return nil
					}
				}
				return fmt.Errorf("published local ONBUILD reference missing for stage %d operation %d", id, operation)
			}
			if trigger.Name == "copy" || trigger.Name == "add" {
				if err := check(trigger.Properties["from"], -1); err != nil {
					return nil, err
				}
			}
			for child, mount := range trigger.Children {
				if err := check(mount.Properties["from"], child); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, ref := range component.StageReferences {
		if ref.Origin != "onbuild" && ref.Origin != "local-onbuild" {
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
		triggers := localTriggers[stageID]
		if ref.Origin == "local-onbuild" {
			if ref.Operation >= len(triggers) {
				return nil, fmt.Errorf("invalid local ONBUILD operation at stage %s", ref.Stage)
			}
			trigger := triggers[ref.Operation]
			source := ""
			if ref.MountIndex == -1 && (trigger.Name == "copy" || trigger.Name == "add") {
				source = trigger.Properties["from"]
			} else if ref.MountIndex >= 0 && ref.MountIndex < len(trigger.Children) {
				source = trigger.Children[ref.MountIndex].Properties["from"]
			}
			if source == "" {
				return nil, fmt.Errorf("invalid local ONBUILD reference at stage %s", ref.Stage)
			}
			authored := ref
			authored.Origin = "authored"
			if err := checkPublishedReference([]StageReferenceBinding{authored}, &deps[stageID], aliases, component.ReservedStageNames, projected, stageID, ref.Operation, ref.MountIndex, source, -1); err != nil {
				return nil, fmt.Errorf("local ONBUILD: %w", err)
			}
			continue
		}
		if !packageBase {
			return nil, fmt.Errorf("ONBUILD stage reference %s is not inherited from a package or authored local FROM", ref.Stage)
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
		aliased := false
		for _, id := range aliases {
			aliased = aliased || id == target
		}
		if err != nil || target < 0 || target >= len(stages) || strconv.Itoa(target) != ref.Target || ref.SourceIndex == "" && !aliased {
			return nil, fmt.Errorf("invalid ONBUILD stage reference target %q", ref.Target)
		}
		if err := projected.requireEarlier(stageID, target, -1, ref.Target); err != nil {
			return nil, err
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
		if ref.SourceIndex != "" && component.SourceStageIDs[ref.SourceIndex] != ref.Target {
			return nil, fmt.Errorf("stage reference conflicts with original stage identity")
		}
		stageID, stageErr := strconv.Atoi(ref.Stage)
		origin := -1
		switch ref.Origin {
		case "onbuild", "local-onbuild":
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
		// An unfinished enclosing EXTEND may be retained as structural ownership
		// for a selected nested result. Other roots need a visible FROM consumer.
		eligible := stages[id].head.Name == "extend" && slices.ContainsFunc(projected.Stages, func(stage RawStage) bool {
			nested := mustStageIndex(stage.ID)
			return nested != id && stage.Owner == strconv.Itoa(id) && (selected[nested] || slices.Contains(dormant, nested))
		})
		for source := range stages {
			if !selected[source] {
				continue
			}
			if projected.requireEarlier(source, id, -1, strconv.Itoa(id)) != nil {
				continue
			}
			binding, ok := component.FromBindings[strconv.Itoa(source)]
			if !ok {
				continue
			}
			if binding.Kind == "image" {
				eligible = true
				break
			}
			// Component-produced configuration can introduce ONBUILD dependencies
			// only when a selected FROM consumes that configuration.
			for steps := 0; binding.Kind == "stage" && steps < len(stages); steps++ {
				base := mustStageIndex(binding.Stage)
				if stages[base].head.Name == "package" {
					break
				}
				if slices.ContainsFunc(stages[base].body, func(inst definition.Instruction) bool { return inst.Name == "component" }) {
					eligible = true
					break
				}
				binding = component.FromBindings[strconv.Itoa(base)]
			}
			if eligible {
				break
			}
		}
		if !eligible {
			return nil, fmt.Errorf("dormant root %d has no selected external FROM or component-produced base", id)
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
	callable := false
	for _, root := range projected.Roots {
		if stages[mustStageIndex(root)].head.Name == "extend" && projected.Results[root] == component.Output {
			callable = true
		}
	}
	if !callable {
		return nil, fmt.Errorf("published component output must belong to an extend target")
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

func checkPublishedReference(refs []StageReferenceBinding, deps *[]int, aliases map[string]int, reserved []string, projected *RawGraph, stage, operation, mount int, source string, referencePosition int) error {
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
		if err != nil || target < 0 || target >= len(projected.Stages) || strconv.Itoa(target) != ref.Target {
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
		if err := projected.requireEarlier(stage, target, referencePosition, source); err != nil {
			return err
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
	left, leftErr := platforms.Parse(a)
	right, rightErr := platforms.Parse(b)
	return leftErr == nil && rightErr == nil && platforms.FormatAll(platforms.Normalize(left)) == platforms.FormatAll(platforms.Normalize(right))
}

func canonicalSourceIndex(value string) bool {
	index, err := strconv.Atoi(value)
	return err == nil && index >= 0 && strconv.Itoa(index) == value
}
