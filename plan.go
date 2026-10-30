package coopr

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/containerd/platforms"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/planner"
	dockerreference "go.podman.io/image/v5/docker/reference"
)

// compile adapts already-typed operations without definition parsing or expansion.
func compile(program Program) (*planner.Plan, error) {
	platform := platforms.DefaultSpec()
	if program.Platform != "" {
		var err error
		platform, err = platforms.Parse(program.Platform)
		if err != nil {
			return nil, fmt.Errorf("platform: %w", err)
		}
	}
	normalized := platforms.Format(platforms.Normalize(platform))
	plan := &planner.Plan{Mode: planner.Build, DefinitionType: "container", Platform: normalized}
	if len(program.States) == 0 {
		return nil, errors.New("program requires states")
	}
	if program.Output == "" || program.States[len(program.States)-1].ID != program.Output {
		return nil, errors.New("output must identify the final state")
	}
	known := map[StateID]string{}
	inputs := map[string]bool{}
	for index, state := range program.States {
		if state.ID == "" {
			return nil, errors.New("state ID is required")
		}
		if _, ok := known[state.ID]; ok {
			return nil, fmt.Errorf("duplicate state %q", state.ID)
		}
		stage := planner.Stage{ID: strconv.Itoa(index), Kind: "from", Platform: normalized, InheritedOnBuildPlanned: true}
		resolve := func(source Source, required bool) (string, error) {
			if source.Image != "" && source.State != "" {
				return "", errors.New("source must select either image or state")
			}
			if source.State != "" {
				id, ok := known[source.State]
				if !ok {
					return "", fmt.Errorf("state %q must refer to an earlier state", source.State)
				}
				if !slices.Contains(stage.Dependencies, id) {
					stage.Dependencies = append(stage.Dependencies, id)
				}
				return id, nil
			}
			if source.Image != "" {
				if _, err := strconv.Atoi(source.Image); err == nil {
					// An explicit default tag distinguishes numeric short image
					// names from private numeric stage aliases without choosing
					// a registry or bypassing configured short-name policy.
					parsed, err := dockerreference.Parse(source.Image)
					if err != nil {
						return "", err
					}
					named, ok := parsed.(dockerreference.Named)
					if !ok {
						return "", fmt.Errorf("invalid image name %q", source.Image)
					}
					source.Image = dockerreference.TagNameOnly(named).String()
				}
				if !strings.EqualFold(source.Image, "scratch") && !inputs[source.Image] {
					plan.Inputs = append(plan.Inputs, planner.Input{Kind: "image", Reference: source.Image})
					inputs[source.Image] = true
				}
				return source.Image, nil
			}
			if required {
				return "", errors.New("state source is required")
			}
			return "", nil
		}
		var err error
		stage.Source, err = resolve(state.From, true)
		if err != nil {
			return nil, fmt.Errorf("state %q: %w", state.ID, err)
		}
		for operationIndex, operation := range state.Operations {
			op := planner.Operation{}
			switch value := operation.(type) {
			case Run:
				if len(value.Args) == 0 || value.Args[0] == "" {
					return nil, fmt.Errorf("state %q: RUN requires argv", state.ID)
				}
				op.Instruction = definition.Instruction{Name: "run", Form: "exec", Arguments: slices.Clone(value.Args)}
				for mountIndex, mount := range value.Mounts {
					if !strings.HasPrefix(mount.Target, "/") {
						return nil, errors.New("mount target must be absolute")
					}
					if mount.Type != "bind" && mount.Type != "cache" && mount.Type != "tmpfs" {
						return nil, fmt.Errorf("unsupported mount type %q", mount.Type)
					}
					if mount.Sharing != "" && mount.Sharing != "shared" && mount.Sharing != "private" && mount.Sharing != "locked" {
						return nil, errors.New("invalid cache sharing mode")
					}
					if mount.Type != "cache" && (mount.ID != "" || mount.Sharing != "") {
						return nil, errors.New("ID and sharing apply only to cache mounts")
					}
					if mount.Type != "bind" && (mount.From != (Source{}) || mount.Source != "") {
						return nil, errors.New("source applies only to bind mounts")
					}
					from, err := resolve(mount.From, false)
					if err != nil {
						return nil, err
					}
					props := map[string]string{"target": mount.Target}
					if mount.Source != "" {
						props["source"] = mount.Source
					}
					if from != "" {
						props["from"] = from
					} else if mount.Type == "bind" {
						if op.MountContexts == nil {
							op.MountContexts = map[int]string{}
						}
						op.MountContexts[mountIndex] = "build"
					}
					if mount.ID != "" {
						props["id"] = mount.ID
					}
					if mount.Sharing != "" {
						props["sharing"] = mount.Sharing
					}
					if mount.ReadOnly || mount.Type == "bind" {
						props["readonly"] = strconv.FormatBool(mount.ReadOnly)
					}
					op.Children = append(op.Children, definition.Instruction{Name: "mount", Arguments: []string{mount.Type}, Properties: props})
				}
			case Copy:
				if len(value.Sources) == 0 || value.Destination == "" {
					return nil, errors.New("COPY requires sources and destination")
				}
				for _, source := range value.Sources {
					if source == "" {
						return nil, errors.New("COPY source must not be empty")
					}
				}
				from, err := resolve(value.From, false)
				if err != nil {
					return nil, err
				}
				op.Instruction = definition.Instruction{Name: "copy", Arguments: append(slices.Clone(value.Sources), value.Destination)}
				op.InputContext = "build"
				if from != "" {
					op.Properties = map[string]string{"from": from}
					op.InputContext = ""
				}
			case Env:
				if value.Name == "" || strings.ContainsAny(value.Name, "=\x00") {
					return nil, errors.New("invalid environment name")
				}
				op.Instruction = definition.Instruction{Name: "env", Arguments: []string{value.Name, value.Value}}
			case Workdir:
				if value.Path == "" {
					return nil, errors.New("workdir requires a path")
				}
				op.Instruction = definition.Instruction{Name: "workdir", Arguments: []string{value.Path}}
			default:
				return nil, fmt.Errorf("state %q operation %d: unsupported operation %T", state.ID, operationIndex+1, operation)
			}
			stage.Operations = append(stage.Operations, op)
		}
		known[state.ID] = stage.ID
		plan.Stages = append(plan.Stages, stage)
	}
	plan.Outputs = []string{known[program.Output]}
	// Retain only sources reachable from the selected output. The backend remains
	// the sole scheduler; this is selection, not a second execution graph.
	selected := map[string]bool{known[program.Output]: true}
	for i := len(plan.Stages) - 1; i >= 0; i-- {
		if selected[plan.Stages[i].ID] {
			for _, id := range plan.Stages[i].Dependencies {
				selected[id] = true
			}
		}
	}
	stages := plan.Stages[:0]
	usedImages := map[string]bool{}
	for _, stage := range plan.Stages {
		if !selected[stage.ID] {
			continue
		}
		stages = append(stages, stage)
		usedImages[stage.Source] = true
		for _, op := range stage.Operations {
			if from := op.Properties["from"]; from != "" {
				usedImages[from] = true
			}
			for _, mount := range op.Children {
				if from := mount.Properties["from"]; from != "" {
					usedImages[from] = true
				}
			}
		}
	}
	plan.Stages = stages
	retainedInputs := plan.Inputs[:0]
	for _, input := range plan.Inputs {
		if usedImages[input.Reference] {
			retainedInputs = append(retainedInputs, input)
		}
	}
	plan.Inputs = retainedInputs
	return plan, nil
}
