package buildah

import (
	"testing"

	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/docker"
)

func TestEffectiveIdentityLabelFollowsNativeTimestampSemantics(t *testing.T) {
	epoch := int64(123)
	for _, test := range []struct {
		name     string
		controls ImageControls
		policy   timestampPolicy
		want     bool
	}{
		{name: "default", want: true},
		{name: "default timestamp", policy: timestampPolicy{timestamp: &epoch}},
		{name: "explicit false", controls: ImageControls{IdentityLabel: boolPointer(false)}},
		{name: "explicit true timestamp", controls: ImageControls{IdentityLabel: boolPointer(true)}, policy: timestampPolicy{timestamp: &epoch}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			controls := effectiveOutputControls(test.controls, test.policy)
			found := false
			for _, label := range controls.Labels {
				found = found || label == upstream.BuilderIdentityAnnotation+"="+define.Version
			}
			if found != test.want {
				t.Fatalf("identity label found=%t want=%t: %#v", found, test.want, controls)
			}
		})
	}
}

func TestCheckpointLayerLabelsDoNotOverrideInstructionLabels(t *testing.T) {
	builder := &upstream.Builder{}
	builder.Docker.Config = &docker.Config{}
	builder.SetLabel("same", "instruction")
	epoch := int64(123)
	if err := applyBuilderCheckpointControls(builder, ImageControls{LayerLabels: []string{"same=layer", "cache=yes"}}, timestampPolicy{timestamp: &epoch}); err != nil {
		t.Fatal(err)
	}
	labels := builder.Labels()
	if labels["same"] != "instruction" || labels["cache"] != "yes" {
		t.Fatalf("checkpoint labels = %#v", labels)
	}
	if _, found := labels[upstream.BuilderIdentityAnnotation]; found {
		t.Fatalf("default identity label added despite timestamp: %#v", labels)
	}
}

func boolPointer(value bool) *bool { return &value }

func TestFinalCommitCompressionFollowsDisableCompression(t *testing.T) {
	var options upstream.CommitOptions
	applyFinalCommitOptions(&options, Output{DisableCompression: true})
	if options.Compression != define.Uncompressed {
		t.Fatalf("disabled compression = %v", options.Compression)
	}
	applyFinalCommitOptions(&options, Output{DisableCompression: false})
	if options.Compression != define.Gzip {
		t.Fatalf("enabled compression = %v", options.Compression)
	}
}
