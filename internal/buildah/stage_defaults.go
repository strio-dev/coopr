package buildah

import (
	"encoding/json"

	"github.com/moby/buildkit/util/system"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/imageconfig"
)

// seedStagePath runs only when entering FROM or a package root. An explicit
// empty PATH is inherited, and a component return never passes this boundary.
func seedStagePath(logical *imageconfig.Config) error {
	environment, err := imageEnvironment(logical)
	if err != nil {
		return err
	}
	if _, present := environment["PATH"]; present {
		return nil
	}
	return logical.Apply(definition.Instruction{Name: "env", Properties: map[string]string{"PATH": system.DefaultPathEnvUnix}})
}

// inheritScratchWorkDir retains the native builder's scratch initialization.
// It does not replace a working directory already present in the logical config.
func inheritScratchWorkDir(logical *imageconfig.Config, native string) error {
	raw, err := logical.MarshalJSON()
	if err != nil {
		return err
	}
	var document struct{ Config map[string]json.RawMessage }
	if err := json.Unmarshal(raw, &document); err != nil {
		return err
	}
	if _, present := document.Config["WorkingDir"]; present || native == "" {
		return nil
	}
	return logical.Apply(definition.Instruction{Name: "workdir", Arguments: []string{native}})
}
