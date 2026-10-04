package buildah

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"coopr/internal/imageconfig"

	upstream "go.podman.io/buildah"
	"go.podman.io/image/v5/pkg/strslice"
)

type builderConfigDocument struct {
	Author string               `json:"author"`
	Config builderRuntimeConfig `json:"config"`
}

// builderRuntimeConfig includes OCI ImageConfig plus Shell, which Coopr keeps
// in raw image configuration for Dockerfile-compatible subsequent RUNs.
type builderRuntimeConfig struct {
	User         string              `json:"User"`
	Hostname     string              `json:"Hostname"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts"`
	Env          []string            `json:"Env"`
	Entrypoint   []string            `json:"Entrypoint"`
	Cmd          []string            `json:"Cmd"`
	Volumes      map[string]struct{} `json:"Volumes"`
	WorkingDir   string              `json:"WorkingDir"`
	Labels       map[string]string   `json:"Labels"`
	StopSignal   string              `json:"StopSignal"`
	ArgsEscaped  bool                `json:"ArgsEscaped"`
	Shell        []string            `json:"Shell"`
}

// syncBuilderConfig replaces the Buildah builder's standard runtime image
// configuration with the executor-neutral logical configuration. The logical
// sidecar remains authoritative and unmodified, retaining unknown fields which
// Buildah's typed configuration cannot represent.
func syncBuilderConfig(builder *upstream.Builder, logical *imageconfig.Config) error {
	if builder == nil {
		return errors.New("config sync builder is nil")
	}
	if logical == nil {
		return errors.New("config sync logical configuration is nil")
	}
	raw, err := logical.MarshalJSON()
	if err != nil {
		return fmt.Errorf("marshal logical image configuration: %w", err)
	}
	var document builderConfigDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return fmt.Errorf("decode logical image configuration: %w", err)
	}

	builder.SetMaintainer(document.Author)
	builder.SetUser(document.Config.User)
	// Generated builder hostnames are session inputs. Preserve only an
	// explicitly inherited hostname as part of the logical execution config.
	if document.Config.Hostname != "" {
		builder.SetHostname(document.Config.Hostname)
	}
	builder.SetWorkDir(document.Config.WorkingDir)
	builder.SetCmd(document.Config.Cmd)
	builder.SetEntrypoint(document.Config.Entrypoint)
	builder.SetStopSignal(document.Config.StopSignal)
	// Buildah's SetShell warning assumes an ordinary OCI commit will discard
	// Shell. Coopr rewrites the authoritative raw config after commit, so copy
	// the value used by subsequent RUNs without emitting that false warning.
	builder.Docker.Config.Shell = strslice.StrSlice(slices.Clone(document.Config.Shell))
	builder.OCIv1.Config.ArgsEscaped = document.Config.ArgsEscaped //nolint:staticcheck // Preserve the validated legacy OCI field exactly.
	builder.Docker.Config.ArgsEscaped = document.Config.ArgsEscaped
	healthcheck, err := logical.Healthcheck()
	if err != nil {
		return fmt.Errorf("read logical healthcheck: %w", err)
	}
	builder.SetHealthcheck(dockerHealthcheck(healthcheck))
	onBuild, err := logical.OnBuild()
	if err != nil {
		return fmt.Errorf("read logical OnBuild triggers: %w", err)
	}
	builder.ClearOnBuild()
	for _, trigger := range onBuild {
		builder.SetOnBuild(trigger)
	}

	builder.ClearEnv()
	for _, spec := range document.Config.Env {
		name, value, set := strings.Cut(spec, "=")
		if set {
			builder.SetEnv(name, value)
		} else {
			// Buildah treats a key without '=' as an explicit removal.
			builder.UnsetEnv(name)
		}
	}

	builder.ClearLabels()
	for _, name := range sortedMapKeys(document.Config.Labels) {
		builder.SetLabel(name, document.Config.Labels[name])
	}
	builder.ClearPorts()
	for _, port := range sortedSetKeys(document.Config.ExposedPorts) {
		builder.SetPort(port)
	}
	builder.ClearVolumes()
	for _, volume := range sortedSetKeys(document.Config.Volumes) {
		builder.AddVolume(volume)
	}
	return nil
}

func sortedMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedSetKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
