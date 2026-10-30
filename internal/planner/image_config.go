package planner

import (
	"encoding/json"
	"fmt"

	"github.com/moby/buildkit/frontend/dockerfile/shell"
	"github.com/strio-dev/coopr/internal/imageconfig"
	"github.com/strio-dev/coopr/internal/onbuildparse"
)

// StageBindFromImageConfig extracts base-image state before planning a child
// FROM stage. Inherited triggers remain unexpanded for the child's ARG and
// stage-reference scope. The source configuration is never consumed or changed.
func StageBindFromImageConfig(logical *imageconfig.Config) (StageBind, error) {
	if logical == nil {
		return StageBind{}, fmt.Errorf("ONBUILD base configuration is nil")
	}

	triggers, err := logical.OnBuild()
	if err != nil {
		return StageBind{}, err
	}
	if err := onbuildparse.ValidateInheritedTriggers(triggers); err != nil {
		return StageBind{}, fmt.Errorf("ONBUILD ingestion: %w", err)
	}
	bind := StageBind{}
	for index, trigger := range triggers {
		parsed, err := onbuildparse.ParseDeferred(trigger)
		if err != nil {
			return StageBind{}, fmt.Errorf("ONBUILD trigger %d: %w", index+1, err)
		}
		bind.Inherited = append(bind.Inherited, parsed...)
	}

	bind.BaseEnvironment, err = ImageEnvironment(logical)
	if err != nil {
		return StageBind{}, fmt.Errorf("ONBUILD base environment: %w", err)
	}
	return bind, nil
}

// ImageEnvironment extracts ENV without parsing or consuming ONBUILD. EXTEND
// invocation uses this boundary because caller triggers belong to a later FROM.
func ImageEnvironment(logical *imageconfig.Config) (map[string]string, error) {
	environment, err := ImageEnvironmentEntries(logical)
	if err != nil {
		return nil, err
	}
	var values map[string]string
	native := shell.EnvsFromSlice(environment)
	for _, name := range native.Keys() {
		value, _ := native.Get(name)
		if values == nil {
			values = make(map[string]string)
		}
		values[name] = value
	}
	return values, nil
}

// ImageEnvironmentEntries returns the image's ordered ENV entries, retaining
// duplicate names and empty values without parsing or consuming ONBUILD.
func ImageEnvironmentEntries(logical *imageconfig.Config) ([]string, error) {
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
