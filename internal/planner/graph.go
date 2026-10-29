package planner

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/strio-dev/coopr/internal/definition"
)

// RawGraph preserves authored instructions until executor-independent planning.
// Published bindings describe graph edges; expressions remain raw.
// Aliases includes EXTEND names redirected to their selected nested result.
// Roots and Results preserve authored scope independently of flattened IDs.
type RawGraph struct {
	aliasPositions     map[string]int
	Aliases            map[string]string
	Roots              []string
	Results            map[string]string
	Globals            []definition.Instruction
	Stages             []RawStage
	Output             string
	PackageLeaves      []string
	FromBindings       map[string]FromBinding
	StageReferences    []StageReferenceBinding
	ReservedStageNames []string
	PackageArguments   map[string]string
	Platform           string
}

type RawStage struct {
	headPosition       int
	endPosition        int
	operationPositions []int
	AuthoredChildren   []definition.Instruction
	Owner              string
	ID                 string
	Name               string
	Kind               string
	Head               definition.Instruction
	Body               []definition.Instruction
	Dependencies       []string
}

// ProjectStandalone splits a container definition without evaluating ARG or
// resolving FROM, COPY, ADD, and mount expressions. The planner binds them later.
func ProjectStandalone(def *definition.Definition, target string) (*RawGraph, error) {
	projected, err := projectRaw(def)
	if err != nil {
		return nil, err
	}
	for _, stage := range projected.Stages {
		if stage.Kind != "from" {
			return nil, fmt.Errorf("standalone build cannot contain %s stage", stage.Kind)
		}
	}
	output, err := projected.selectedTarget(target, false)
	if err != nil {
		return nil, err
	}
	projected.Output = strconv.Itoa(output)
	return projected, nil
}

// ProjectPublicationRaw retains the whole authored component graph so the
// planner can resolve structural bindings before closure pruning.
// It does not evaluate ARG values or execute package producers.
func ProjectPublicationRaw(def *definition.Definition, target string) (*RawGraph, error) {
	projected, err := projectRaw(def)
	if err != nil {
		return nil, err
	}
	component := false
	for _, stage := range projected.Stages {
		component = component || stage.Kind == "extend"
	}
	if !component {
		return nil, fmt.Errorf("component publication requires extend")
	}
	output, err := projected.selectedTarget(target, true)
	if err != nil {
		return nil, err
	}
	projected.Output = strconv.Itoa(output)
	return projected, nil
}

// ProjectPublished validates immutable publication metadata, then exposes its
// already-pruned stage graph without re-evaluating authored instructions.
func ProjectPublished(component *PublishedComponent) (*RawGraph, error) {
	packages, err := ValidatePublished(component)
	if err != nil {
		return nil, err
	}
	if component.Platform == "" {
		return nil, fmt.Errorf("published platform is empty")
	}
	if err := validatePlatform(component.Platform); err != nil {
		return nil, err
	}
	projected, err := projectRaw(component.Definition)
	if err != nil {
		return nil, err
	}
	projected.Output = component.Output
	projected.FromBindings = maps.Clone(component.FromBindings)
	projected.StageReferences = slices.Clone(component.StageReferences)
	projected.ReservedStageNames = slices.Clone(component.ReservedStageNames)
	projected.PackageArguments = maps.Clone(component.PackageArguments)
	projected.Platform = component.Platform

	byName := make(map[string]string, len(projected.Stages))
	for _, stage := range projected.Stages {
		if stage.Name != "" {
			byName[canonicalStageName(stage.Name)] = stage.ID
		}
	}
	for _, name := range packages {
		projected.PackageLeaves = append(projected.PackageLeaves, byName[canonicalStageName(name)])
	}
	for i := range projected.Stages {
		stage := &projected.Stages[i]
		if binding := projected.FromBindings[stage.ID]; binding.AfterStage != "" {
			stage.Dependencies = appendUnique(stage.Dependencies, binding.AfterStage)
		}
		if binding, ok := projected.FromBindings[stage.ID]; ok && binding.Kind == "stage" {
			stage.Dependencies = appendUnique(stage.Dependencies, binding.Stage)
		}
		for _, ref := range projected.StageReferences {
			if ref.Stage == stage.ID && ref.Kind != "image" {
				stage.Dependencies = appendUnique(stage.Dependencies, ref.Target)
			}
		}
	}
	return projected, nil
}

func projectRaw(def *definition.Definition) (*RawGraph, error) {
	if err := definition.Validate(def); err != nil {
		return nil, err
	}
	result := &RawGraph{Aliases: map[string]string{}, Results: map[string]string{}}
	names := map[string]bool{}
	add := func(inst definition.Instruction, owner string) (int, error) {
		name := inst.Properties["as"]
		if _, named := inst.Properties["as"]; named {
			canonical := canonicalStageName(name)
			if !stageName.MatchString(canonical) || canonical == "scratch" {
				return 0, fmt.Errorf("invalid stage name %q", name)
			}
			if names[canonical] {
				return 0, fmt.Errorf("duplicate stage name %q", name)
			}
			names[canonical] = true
		}
		id := len(result.Stages)
		authoredChildren := cloneInstruction(inst).Children
		body := definition.FlattenLayers(inst.Children)
		inst.Children = nil
		stage := RawStage{ID: strconv.Itoa(id), Name: name, Kind: inst.Name, Head: cloneInstruction(inst), Owner: owner, AuthoredChildren: authoredChildren}
		for _, child := range body {
			stage.Body = append(stage.Body, cloneInstruction(child))
		}
		result.Stages = append(result.Stages, stage)
		if name != "" {
			result.Aliases[canonicalStageName(name)] = stage.ID
		}
		return id, nil
	}
	for _, inst := range def.Instructions {
		if inst.Name == "arg" {
			result.Globals = append(result.Globals, cloneInstruction(inst))
			continue
		}
		children := cloneInstruction(inst).Children
		if inst.Name == "extend" {
			inst.Children = nil
			for _, child := range children {
				if child.Name != "from" {
					inst.Children = append(inst.Children, child)
				}
			}
		}
		id, err := add(inst, "")
		if err != nil {
			return nil, err
		}
		root := strconv.Itoa(id)
		result.Roots = append(result.Roots, root)
		selected := root
		if inst.Name == "extend" {
			result.Stages[id].Owner = root
			result.Stages[id].AuthoredChildren = children
			for _, child := range children {
				if child.Name == "from" {
					nested, err := add(child, root)
					if err != nil {
						return nil, err
					}
					selected = strconv.Itoa(nested)
				} else if child.Name != "arg" {
					selected = root
				}
			}
			if inst.Properties["as"] != "" {
				result.Aliases[canonicalStageName(inst.Properties["as"])] = selected
				if selected != root && result.Stages[mustStageIndex(selected)].Name == "" {
					result.Stages[mustStageIndex(selected)].Name = inst.Properties["as"]
				}
			}
		}
		result.Results[root] = selected
	}
	if len(result.Stages) == 0 {
		return nil, fmt.Errorf("no stages: expected from or extend")
	}
	result.indexStageOrder()
	return result, nil
}

// Stage IDs identify graph nodes, not lexical positions: an EXTEND's direct
// instructions can follow a nested FROM whose ID is greater than its own.
func (g *RawGraph) indexStageOrder() {
	g.aliasPositions = map[string]int{}
	position, next := 0, 0
	var visit func(int)
	visit = func(id int) {
		stage := &g.Stages[id]
		position++
		stage.headPosition = position
		for _, inst := range definition.FlattenLayers(stage.AuthoredChildren) {
			if inst.Name == "from" {
				nested := next
				next++
				visit(nested)
				continue
			}
			position++
			if inst.Name != "arg" {
				stage.operationPositions = append(stage.operationPositions, position)
			}
		}
		position++
		stage.endPosition = position
		if name := stage.Head.Properties["as"]; name != "" {
			g.aliasPositions[canonicalStageName(name)] = position
		}
	}
	for _, root := range g.Roots {
		id := mustStageIndex(root)
		next = id + 1
		visit(id)
	}
}

// operation is the authored non-ARG ordinal, or -1 for the stage head and
// inherited ONBUILD, which executes before the authored stage body.
func (g *RawGraph) requireEarlier(stage, target, operation int, source string) error {
	consumer := g.Stages[stage]
	position := consumer.headPosition
	if operation >= 0 {
		if operation >= len(consumer.operationPositions) {
			return fmt.Errorf("stage %d has invalid reference operation %d", stage, operation)
		}
		position = consumer.operationPositions[operation]
	}
	available := g.Stages[target].endPosition
	if alias, ok := g.aliasPositions[canonicalStageName(source)]; ok {
		available = alias
	}
	if available >= position {
		return fmt.Errorf("stage %d references %q before it is available: requires an earlier stage", stage, source)
	}
	return nil
}

func (g *RawGraph) selectedTarget(target string, component bool) (int, error) {
	root := g.Roots[len(g.Roots)-1]
	if target != "" {
		output, ok := g.Aliases[canonicalStageName(target)]
		if !ok {
			return 0, fmt.Errorf("unknown target %q", target)
		}
		if component {
			found := false
			for _, id := range g.Roots {
				stage := g.Stages[mustStageIndex(id)]
				if stage.Kind == "extend" && canonicalStageName(stage.Name) == canonicalStageName(target) {
					root = id
					found = true
					break
				}
			}
			if !found {
				return 0, fmt.Errorf("selected component output must be an extend target")
			}
		} else {
			return mustStageIndex(output), nil
		}
	}
	if component && target == "" {
		for i := len(g.Roots) - 1; i >= 0; i-- {
			if g.Stages[mustStageIndex(g.Roots[i])].Kind == "extend" {
				root = g.Roots[i]
				break
			}
		}
	}
	return mustStageIndex(g.Results[root]), nil
}

func mustStageIndex(id string) int { value, _ := strconv.Atoi(id); return value }

func canonicalStageName(name string) string {
	return strings.ToLower(name)
}

func cloneInstruction(inst definition.Instruction) definition.Instruction {
	inst.Arguments = slices.Clone(inst.Arguments)
	inst.Commands = cloneCommands(inst.Commands)
	inst.Properties = maps.Clone(inst.Properties)
	inst.PropertyOrder = slices.Clone(inst.PropertyOrder)
	inst.InlineFiles = slices.Clone(inst.InlineFiles)
	if inst.Children != nil {
		children := make([]definition.Instruction, len(inst.Children))
		for i, child := range inst.Children {
			children[i] = cloneInstruction(child)
		}
		inst.Children = children
	}
	return inst
}

func appendUnique(values []string, value string) []string {
	if !slices.Contains(values, value) {
		return append(values, value)
	}
	return values
}

func cloneCommands(commands []definition.Command) []definition.Command {
	if commands == nil {
		return nil
	}
	out := make([]definition.Command, len(commands))
	for i, command := range commands {
		out[i] = definition.Command{Form: command.Form, Arguments: slices.Clone(command.Arguments)}
	}
	return out
}
