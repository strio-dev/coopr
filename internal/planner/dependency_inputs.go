package planner

import (
	"fmt"
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
// Literal references come from the default selected closure, never unrelated
// residual branches. Default-prefix references use an empty caller environment.
// Runtime-selected branches and unavailable external configuration are incomplete.
func InspectPublishedDependencies(component *PublishedComponent, packageBinds map[string]StageBind) (DependencyInspection, error) {
	graph, err := ProjectPublished(component)
	if err != nil {
		return DependencyInspection{}, err
	}
	if err := validatePackageStageBinds(graph, packageBinds); err != nil {
		return DependencyInspection{}, err
	}
	raw := make([]rawStage, len(graph.Stages))
	for index, stage := range graph.Stages {
		raw[index].head = stage.Head
	}
	packageBinds, err = validateAndCloneBinds(raw, packageBinds, "package")
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
				if child.Name == "mount" && len(child.Arguments) == 1 && strings.Contains(child.Arguments[0], "$") {
					inspection.Incomplete = true
				}
				if child.Name == "mount" && len(child.Arguments) == 1 && (child.Arguments[0] == "bind" || child.Arguments[0] == "cache") {
					image(child.Properties["from"], child.ProcessQuotes)
				}
			}
		}
	}
	// The residual retains unrelated source for future argument selections, but
	// dependency publication must not capture it. Reuse normal planning for the
	// default selected closure; only the real prefix contributes expanded inputs.
	options := Options{packageBinds: packageBinds, AwaitComponentReturns: true, DeferImageSource: func(reference string) (bool, error) {
		return reference != "" && !strings.EqualFold(reference, "scratch"), nil
	}}
	resolve := func(source FromSource) (StageBind, error) {
		// COPY/mount sources need no image configuration to inventory their names.
		if source.Kind == FromSourceCopyImage {
			return StageBind{}, nil
		}
		return StageBind{}, fmt.Errorf("offline dependency inspection cannot resolve image configuration %q", source.Source)
	}
	plan, planErr := instantiate(component, options, resolve, graph)
	closure := plan
	if plan != nil {
		for _, stage := range plan.Stages {
			if stage.AwaitingComponent == nil {
				continue
			}
			// Only component-return boundaries require a second symbolic pass.
			// No executor, registry resolver or filesystem callback is installed.
			options.AwaitComponentReturns = false
			var closureErr error
			closure, closureErr = instantiate(component, options, resolve, graph)
			if closureErr != nil {
				inspection.Incomplete = true
				closure = plan
			}
			break
		}
	}
	selected := map[string]bool{component.Output: true}
	if closure != nil {
		for _, stage := range closure.Stages {
			selected[stage.ID] = true
			if stage.Kind == "from" && stage.InheritedOnBuildPlanned {
				// Inspect only package triggers actually consumed by a FROM;
				// COPY of a snapshot does not activate its ONBUILD metadata.
				id := stage.Source
				if named, ok := graph.Aliases[canonicalStageName(id)]; ok {
					id = named
				} else if index, err := strconv.Atoi(id); err == nil {
					id = strconv.Itoa(index)
				}
				instructions(packageBinds[id].Inherited)
			}
		}
	}
	for _, stage := range graph.Stages {
		if stage.Kind == "package" || !selected[stage.ID] {
			continue
		}
		if strings.Contains(stage.Head.Properties["after"], "$") {
			inspection.Incomplete = true
		}
		if stage.Kind == "from" {
			source := stage.Head.Arguments[0]
			_, local := graph.Aliases[canonicalStageName(source)]
			_, numeric := strconv.Atoi(source)
			if !local && numeric != nil && !strings.EqualFold(source, "scratch") {
				image(source, stage.Head.ProcessQuotes)
				// External ENV and ONBUILD are deliberately not fetched here.
				inspection.Incomplete = true
			}
		}
		instructions(stage.Body)
	}
	if planErr != nil {
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
