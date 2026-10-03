package buildah

import (
	"fmt"
	"strings"

	"coopr/internal/imageconfig"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
)

// ImageControls is kept executor-neutral by internal/imageconfig and threaded
// through Buildah requests as one value.
type ImageControls = imageconfig.OutputControls

func prepareBuilderImageControls(builder *upstream.Builder, controls ImageControls) error {
	if err := controls.Validate(); err != nil {
		return err
	}
	if controls.DropInheritedLabels {
		builder.ClearLabels()
	}
	if controls.DropInheritedAnnotations {
		builder.ClearAnnotations()
	}
	for _, item := range controls.Env {
		key, value, _ := strings.Cut(item, "=")
		builder.SetEnv(key, value)
	}
	return nil
}

func applyBuilderOutputControls(builder *upstream.Builder, controls ImageControls, policy timestampPolicy) error {
	if err := controls.Validate(); err != nil {
		return err
	}
	for _, item := range controls.Labels {
		key, value, _ := strings.Cut(item, "=")
		builder.SetLabel(key, value)
	}
	applyIdentityLabel(builder, controls, policy)
	for _, key := range controls.UnsetEnv {
		builder.UnsetEnv(key)
	}
	for _, key := range controls.UnsetLabels {
		builder.UnsetLabel(key)
	}
	for _, item := range controls.Annotations {
		key, value, found := strings.Cut(item, "=")
		if !found {
			return fmt.Errorf("invalid annotation %q", item)
		}
		builder.SetAnnotation(key, value)
	}
	for _, key := range controls.UnsetAnnotations {
		builder.UnsetAnnotation(key)
	}
	if controls.OSVersion != "" {
		builder.SetOSVersion(controls.OSVersion)
	}
	for _, spec := range controls.OSFeatures {
		if strings.HasSuffix(spec, "-") {
			builder.UnsetOSFeature(strings.TrimSuffix(spec, "-"))
		} else {
			builder.SetOSFeature(spec)
		}
	}
	return nil
}

func applyBuilderCheckpointControls(builder *upstream.Builder, controls ImageControls, policy timestampPolicy) error {
	if err := controls.Validate(); err != nil {
		return err
	}
	labels := builder.Labels()
	for _, item := range controls.LayerLabels {
		key, value, _ := strings.Cut(item, "=")
		if _, exists := labels[key]; !exists {
			builder.SetLabel(key, value)
		}
	}
	applyIdentityLabel(builder, controls, policy)
	for _, key := range controls.UnsetLabels {
		builder.UnsetLabel(key)
	}
	return nil
}

func applyIdentityLabel(builder *upstream.Builder, controls ImageControls, policy timestampPolicy) {
	if controls.IdentityLabel != nil {
		if *controls.IdentityLabel {
			builder.SetLabel(upstream.BuilderIdentityAnnotation, define.Version)
		}
		return
	}
	if policy.createdEpoch() == nil {
		builder.SetLabel(upstream.BuilderIdentityAnnotation, define.Version)
	}
}

func effectiveOutputControls(controls ImageControls, policy timestampPolicy) ImageControls {
	add := controls.IdentityLabel != nil && *controls.IdentityLabel || controls.IdentityLabel == nil && policy.createdEpoch() == nil
	if add {
		controls.Labels = append(append([]string(nil), controls.Labels...), upstream.BuilderIdentityAnnotation+"="+define.Version)
	}
	return controls
}
