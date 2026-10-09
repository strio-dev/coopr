package buildah

import (
	"os"
	"path/filepath"
	"testing"

	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/docker"
	commonconfig "go.podman.io/common/pkg/config"
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
	if err := applyFinalCommitOptions(&options, Output{DisableCompression: true}); err != nil {
		t.Fatal(err)
	}
	if options.Compression != define.Uncompressed {
		t.Fatalf("disabled compression = %v", options.Compression)
	}
	if err := applyFinalCommitOptions(&options, Output{DisableCompression: false}); err != nil {
		t.Fatal(err)
	}
	if options.Compression != define.Gzip {
		t.Fatalf("enabled compression = %v", options.Compression)
	}
}

func TestFinalCommitCompressionAlgorithmOverridesDisable(t *testing.T) {
	level := 5
	output := Output{DisableCompression: true, CompressionFormat: "zstd", CompressionLevel: &level}
	var options upstream.CommitOptions
	if err := applyFinalCommitOptions(&options, output); err != nil {
		t.Fatal(err)
	}
	if options.Compression != define.Gzip || options.CompressionFormat == nil || options.CompressionFormat.Name() != "zstd" || options.CompressionLevel == nil || *options.CompressionLevel != 5 || !options.ForceCompressionFormat {
		t.Fatalf("compression options: %#v", options)
	}
	output.ForceCompression = boolPointer(false)
	if err := applyFinalCommitOptions(&options, output); err != nil {
		t.Fatal(err)
	}
	if options.ForceCompressionFormat {
		t.Fatal("explicit false was ignored")
	}
	if err := applyFinalCommitOptions(&options, Output{CompressionFormat: "unknown"}); err == nil {
		t.Fatal("invalid algorithm accepted")
	}
}

func TestCompressionDefaultsUseRequestConfigModules(t *testing.T) {
	module := filepath.Join(t.TempDir(), "compression.conf")
	if err := os.WriteFile(module, []byte("[engine]\ncompression_format=\"zstd\"\ncompression_level=3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	options, err := normalizePlanOptions(PlanOptions{Output: Output{DisableCompression: true}, RunControls: RunControls{ConfigModules: []string{module}}})
	if err != nil {
		t.Fatal(err)
	}
	if options.Output.DisableCompression || options.Output.CompressionFormat != "zstd" || options.Output.CompressionLevel == nil || *options.Output.CompressionLevel != 3 || options.Output.ForceCompression == nil || !*options.Output.ForceCompression {
		t.Fatalf("request module defaults ignored: %#v", options.Output)
	}
	options, err = normalizePlanOptions(PlanOptions{Output: Output{CompressionFormat: "gzip", ForceCompression: boolPointer(false)}, RunControls: RunControls{ConfigModules: []string{module}}})
	if err != nil {
		t.Fatal(err)
	}
	if options.Output.CompressionFormat != "gzip" || *options.Output.ForceCompression {
		t.Fatalf("explicit compression overrides ignored: %#v", options.Output)
	}
}

func TestResolvedCompressionDoesNotReadAmbientDefaultsAgain(t *testing.T) {
	dir := t.TempDir()
	ambient, selected := filepath.Join(dir, "ambient.conf"), filepath.Join(dir, "selected.conf")
	for path, format := range map[string]string{ambient: "zstd", selected: "gzip"} {
		if err := os.WriteFile(path, []byte("[engine]\ncompression_format=\""+format+"\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if _, err := commonconfig.New(&commonconfig.Options{SetDefault: true}); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("CONTAINERS_CONF", ambient)
	if _, err := commonconfig.New(&commonconfig.Options{SetDefault: true}); err != nil {
		t.Fatal(err)
	}
	for _, output := range []Output{{DisableCompression: true}, {ForceCompression: boolPointer(false)}} {
		normalized, err := normalizeOutputCompressionForControls(output, RunControls{ConfigModules: []string{selected}})
		if err != nil {
			t.Fatal(err)
		}
		again, err := normalizeOutputCompression(normalized)
		if err != nil {
			t.Fatal(err)
		}
		var options upstream.CommitOptions
		if err := applyFinalCommitOptions(&options, again); err != nil {
			t.Fatal(err)
		}
		if options.CompressionFormat != nil {
			t.Fatalf("ambient compression changed selected gzip: %#v", options)
		}
		want := define.Gzip
		if output.DisableCompression {
			want = define.Uncompressed
		}
		if options.Compression != want || options.ForceCompressionFormat {
			t.Fatalf("selected defaults changed: %#v", options)
		}
		if (again.ForceCompression == nil) != (output.ForceCompression == nil) {
			t.Fatal("unspecified force became explicit")
		}
	}
}
