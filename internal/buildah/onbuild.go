package buildah

import (
	"encoding/json"
	"fmt"
	"strings"

	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"coopr/internal/onbuildparse"
	"coopr/internal/planner"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
)

// stageBindFromImageConfig extracts the base-image state that must be known
// before planning a child FROM stage. Inherited triggers remain unexpanded so
// ARG defaults and stage references resolve in the child's planning scope.
// The source configuration is read-only: consuming and clearing OnBuild stays
// an execution concern until callers are fully moved to pre-execution binding.
func stageBindFromImageConfig(logical *imageconfig.Config) (planner.StageBind, error) {
	if logical == nil {
		return planner.StageBind{}, fmt.Errorf("ONBUILD base configuration is nil")
	}

	triggers, err := logical.OnBuild()
	if err != nil {
		return planner.StageBind{}, err
	}
	bind := planner.StageBind{}
	for index, trigger := range triggers {
		parsed, err := parseOnBuildTrigger(trigger)
		if err != nil {
			return planner.StageBind{}, fmt.Errorf("ONBUILD trigger %d: %w", index+1, err)
		}
		bind.Inherited = append(bind.Inherited, parsed...)
	}

	environment, err := onBuildEnvironment(logical)
	if err != nil {
		return planner.StageBind{}, fmt.Errorf("ONBUILD base environment: %w", err)
	}
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		if !found || name == "" {
			return planner.StageBind{}, fmt.Errorf("ONBUILD base environment entry %q must be NAME=value", entry)
		}
		if bind.BaseEnvironment == nil {
			bind.BaseEnvironment = make(map[string]string)
		}
		bind.BaseEnvironment[name] = value
	}
	// Keep the zero value stable for images without environment entries.
	if len(bind.BaseEnvironment) == 0 {
		bind.BaseEnvironment = nil
	}
	return bind, nil
}

// consumeOnBuild plans inherited triggers at the start of a FROM stage. A
// component's extend stage is not a FROM, so its caller's triggers remain
// untouched. Reject stage references until the graph scheduler can add their
// dependencies before selecting concurrent waves.
func consumeOnBuild(logical *imageconfig.Config) ([]planner.Operation, error) {
	if logical == nil {
		return nil, fmt.Errorf("ONBUILD base configuration is nil")
	}
	triggers, err := logical.OnBuild()
	if err != nil {
		return nil, err
	}
	var planned []planner.Operation
	working := logical.Clone()
	for index, trigger := range triggers {
		environment, err := onBuildEnvironment(working)
		if err != nil {
			return nil, fmt.Errorf("ONBUILD trigger %d environment: %w", index+1, err)
		}
		lex := shell.NewLex('\\')
		rawLex := shell.NewLex('\\')
		rawLex.SkipProcessQuotes = true
		instructions, err := parseOnBuildTriggerWithExpanders(trigger, func(word string) (string, error) {
			result, _, err := lex.ProcessWord(word, shell.EnvsFromSlice(environment))
			return result, err
		}, func(word string) (string, error) {
			result, _, err := rawLex.ProcessWord(word, shell.EnvsFromSlice(environment))
			return result, err
		})
		if err != nil {
			return nil, fmt.Errorf("ONBUILD trigger %d: %w", index+1, err)
		}
		for _, instruction := range instructions {
			if instruction.Name == "arg" {
				return nil, fmt.Errorf("ONBUILD trigger %d: ARG requires child-stage planning before execution", index+1)
			}
			_, networkExplicit := instruction.Properties["network"]
			operation := planner.Operation{
				Instruction:     instruction,
				NetworkExplicit: instruction.Name == "run" && networkExplicit,
			}
			if len(operationStageReferences(operation)) != 0 {
				return nil, fmt.Errorf("ONBUILD trigger %d: --from stage references are not yet supported", index+1)
			}
			if instruction.Name == "copy" || instruction.Name == "add" {
				operation.InputContext = "build"
			}
			for mountIndex, mount := range instruction.Children {
				if mount.Name == "mount" && len(mount.Arguments) == 1 && mount.Arguments[0] == "bind" {
					if operation.MountContexts == nil {
						operation.MountContexts = make(map[int]string)
					}
					operation.MountContexts[mountIndex] = "build"
				}
			}
			planned = append(planned, operation)
			switch instruction.Name {
			case "env", "label", "workdir", "user", "cmd", "entrypoint", "shell", "expose", "volume", "stopsignal", "healthcheck":
				if err := working.Apply(instruction); err != nil {
					return nil, fmt.Errorf("ONBUILD trigger %d configuration: %w", index+1, err)
				}
			}
		}
	}
	if err := logical.ClearOnBuild(); err != nil {
		return nil, fmt.Errorf("clear consumed ONBUILD triggers: %w", err)
	}
	return planned, nil
}

func onBuildEnvironment(logical *imageconfig.Config) ([]string, error) {
	raw, err := logical.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var document struct {
		Config struct {
			Env []string `json:"Env"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	return document.Config.Env, nil
}

// parseOnBuildTrigger reads authored Dockerfile metadata in memory.
func parseOnBuildTrigger(raw string) ([]definition.Instruction, error) {
	return onbuildparse.ParseDeferred(raw)
}

func parseOnBuildTriggerWithExpander(raw string, expand func(string) (string, error)) ([]definition.Instruction, error) {
	return onbuildparse.ParseExpanded(raw, expand)
}

func parseOnBuildTriggerWithExpanders(raw string, expand, expandRaw func(string) (string, error)) ([]definition.Instruction, error) {
	return onbuildparse.ParseExpandedRaw(raw, expand, expandRaw)
}
