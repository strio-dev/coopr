package buildah

import (
	"fmt"

	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"coopr/internal/onbuildparse"
	"coopr/internal/planner"
)

func stageBindFromImageConfig(logical *imageconfig.Config) (planner.StageBind, error) {
	return planner.StageBindFromImageConfig(logical)
}

func imageEnvironment(logical *imageconfig.Config) (map[string]string, error) {
	return planner.ImageEnvironment(logical)
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
	if err := onbuildparse.ValidateInheritedTriggers(triggers); err != nil {
		return nil, fmt.Errorf("ONBUILD ingestion: %w", err)
	}
	var planned []planner.Operation
	working := logical.Clone()
	for index, trigger := range triggers {
		environment, err := onBuildEnvironment(working)
		if err != nil {
			return nil, fmt.Errorf("ONBUILD trigger %d environment: %w", index+1, err)
		}
		instructions, err := onbuildparse.ParseExpandedEnvironment(trigger, environment)
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
	return planner.ImageEnvironmentEntries(logical)
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
