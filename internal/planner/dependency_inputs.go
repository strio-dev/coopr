package planner

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/onbuildparse"
)

// DependencyInspection contains retained literal reference candidates and
// references observed in a default invocation prefix. Incomplete conservatively
// reports when caller values, runtime configuration or conditional instructions
// may change that inventory. Consumers classify registry references and choose
// publication policy.
type DependencyInspection struct {
	Inputs     []Input
	Incomplete bool
}

// InspectPublishedDependencies inspects publication metadata without fetching
// image configuration, invoking components or resolving registry selectors.
// Literal references include retained dormant stages. Default-prefix references
// describe an empty caller environment, not every possible invocation.
func InspectPublishedDependencies(component *PublishedComponent, packageBinds map[string]StageBind) (DependencyInspection, error) {
	graph, err := ProjectPublished(component)
	if err != nil {
		return DependencyInspection{}, err
	}
	if err := validatePackageStageBinds(component, packageBinds); err != nil {
		return DependencyInspection{}, err
	}
	raw := make([]rawStage, len(graph.Stages))
	for index, stage := range graph.Stages {
		raw[index].head = stage.Head
	}
	packageBinds, err = validateAndCloneStageBinds(raw, packageBinds)
	if err != nil {
		return DependencyInspection{}, err
	}
	inspection := DependencyInspection{}
	seen := map[Input]bool{}
	add := func(kind, value string) {
		if value == "" {
			return
		}
		if strings.Contains(value, "$") {
			inspection.Incomplete = true
			return
		}
		if kind == "image" && strings.EqualFold(value, "scratch") {
			return
		}
		if definition.IsLocalComponentReference(value) {
			inspection.Incomplete = true
			return
		}
		input := Input{Kind: kind, Reference: value}
		if !seen[input] {
			inspection.Inputs = append(inspection.Inputs, input)
			seen[input] = true
		}
	}
	image := func(value string, processQuotes bool) {
		if processQuotes && !strings.Contains(value, "$") {
			expanded, err := expandDockerWord(value, nil)
			if err != nil {
				inspection.Incomplete = true
				return
			}
			value = expanded
		}
		if _, local := graph.Aliases[canonicalStageName(value)]; local {
			return
		}
		if _, err := strconv.Atoi(value); err == nil {
			return
		}
		add("image", value)
	}
	var instructions func([]definition.Instruction)
	instructions = func(body []definition.Instruction) {
		for _, inst := range body {
			switch inst.Name {
			case "component":
				add("component", inst.Arguments[0])
			case "copy", "add":
				image(inst.Properties["from"], inst.ProcessQuotes)
			case "onbuild":
				// The trigger can be consumed by a retained local stage or remain
				// deferred in the resulting image's configuration.
				inspection.Incomplete = true
				parsed, err := onbuildparse.Parse(inst.Arguments[0])
				if err == nil {
					instructions(parsed)
				}
			}
			for _, child := range inst.Children {
				if child.Name == "mount" && len(child.Arguments) == 1 && (child.Arguments[0] == "bind" || child.Arguments[0] == "cache") {
					image(child.Properties["from"], child.ProcessQuotes)
				}
			}
		}
	}
	for _, stage := range graph.Stages {
		if stage.Kind == "package" {
			continue
		}
		if binding := graph.FromBindings[stage.ID]; binding.Kind == "image" {
			add("image", stage.Head.Arguments[0])
			// External ENV and ONBUILD are deliberately not fetched here.
			inspection.Incomplete = true
		}
		instructions(stage.Body)
	}
	if len(component.DormantRoots) != 0 {
		inspection.Incomplete = true
	}
	keys := make([]string, 0, len(packageBinds))
	for key := range packageBinds {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		instructions(packageBinds[key].Inherited)
	}
	plan, err := InstantiateDemandDriven(component, Options{
		AwaitComponentReturns: true,
		DeferImageSource: func(reference string) (bool, error) {
			return reference != "" && !strings.EqualFold(reference, "scratch"), nil
		},
	}, packageBinds, func(source FromSource) (StageBind, error) {
		return StageBind{}, fmt.Errorf("offline dependency inspection cannot resolve image configuration %q", source.Source)
	})
	if err != nil {
		inspection.Incomplete = true
		return inspection, nil
	}
	for _, input := range plan.Inputs {
		switch input.Kind {
		case "component", "image":
			add(input.Kind, input.Reference)
		case "context":
			inspection.Incomplete = true
		}
	}
	for _, stage := range plan.Stages {
		if stage.DeferredImageSource || stage.DynamicBaseStage != "" {
			inspection.Incomplete = true
		}
	}
	return inspection, nil
}
