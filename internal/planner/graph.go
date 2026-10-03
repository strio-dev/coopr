package planner

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"coopr/internal/definition"
)

// RawGraph preserves authored instructions until executor-independent planning.
// Published bindings describe graph edges; expressions remain raw.
type RawGraph struct {
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
	ID           string
	Name         string
	Kind         string
	Head         definition.Instruction
	Body         []definition.Instruction
	Dependencies []string
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
	output := len(projected.Stages) - 1
	if target != "" {
		target = canonicalStageName(target)
		output = -1
		for i, stage := range projected.Stages {
			if canonicalStageName(stage.Name) == target {
				output = i
				break
			}
		}
		if output < 0 {
			return nil, fmt.Errorf("unknown target %q", target)
		}
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
	output := len(projected.Stages) - 1
	if target != "" {
		target = canonicalStageName(target)
		output = -1
		for i, stage := range projected.Stages {
			if canonicalStageName(stage.Name) == target {
				output = i
				break
			}
		}
		if output < 0 {
			return nil, fmt.Errorf("unknown target %q", target)
		}
	}
	if projected.Stages[output].Kind == "package" {
		return nil, fmt.Errorf("selected output cannot be a package stage")
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
	result := &RawGraph{}
	aliases := map[string]bool{}
	for pos, inst := range def.Instructions {
		switch inst.Name {
		case "from", "extend", "package":
			name := inst.Properties["as"]
			if _, named := inst.Properties["as"]; named {
				canonical := canonicalStageName(name)
				if !stageName.MatchString(canonical) || canonical == "scratch" {
					return nil, fmt.Errorf("instruction %d: invalid stage name %q", pos+1, name)
				}
				if aliases[canonical] {
					return nil, fmt.Errorf("duplicate stage name %q", name)
				}
				aliases[canonical] = true
			}
			result.Stages = append(result.Stages, RawStage{ID: strconv.Itoa(len(result.Stages)), Name: name, Kind: inst.Name, Head: cloneInstruction(inst)})
		default:
			if len(result.Stages) == 0 {
				if inst.Name != "arg" {
					return nil, fmt.Errorf("instruction %d: %s requires a stage", pos+1, inst.Name)
				}
				result.Globals = append(result.Globals, cloneInstruction(inst))
			} else {
				i := len(result.Stages) - 1
				result.Stages[i].Body = append(result.Stages[i].Body, cloneInstruction(inst))
			}
		}
	}
	if len(result.Stages) == 0 {
		return nil, fmt.Errorf("no stages: expected from or extend")
	}
	return result, nil
}

func canonicalStageName(name string) string {
	return strings.ToLower(name)
}

func cloneInstruction(inst definition.Instruction) definition.Instruction {
	inst.Arguments = slices.Clone(inst.Arguments)
	inst.Properties = maps.Clone(inst.Properties)
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
