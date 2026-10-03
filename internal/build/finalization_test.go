package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/transfer"
	"go.podman.io/buildah/define"
)

func TestFinalizationValidatesPlatformsAndSigning(t *testing.T) {
	for _, test := range []struct {
		name      string
		options   Options
		platforms []string
		want      string
	}{
		{"local signing", Options{Signing: transfer.SigningOptions{SigstorePrivateKeyFile: "key"}}, []string{"linux/amd64"}, "registry destination"},
		{"password without key", Options{Signing: transfer.SigningOptions{PassphraseFile: "pass"}}, []string{"linux/amd64"}, "requires"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateFinalization(&test.options, test.platforms, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want %s", err, test.want)
			}
		})
	}
	options := Options{Output: buildah.FilesystemOutput{Type: "local", Path: t.TempDir()}, SBOM: []define.SBOMScanOptions{{Image: "scanner", Commands: []string{"scan"}, ImageSBOMOutput: "/sbom.json"}}, Signing: transfer.SigningOptions{SigstorePrivateKeyFile: "key"}}
	if err := validateFinalization(&options, []string{"linux/amd64", "linux/arm64"}, []transfer.Destination{{Transport: "local", Name: "app"}, {Transport: "registry", Name: "registry.test/app:latest"}}); err != nil {
		t.Fatal(err)
	}
	wantBase := options.Outputs[0]
	if got := platformFilesystemOutput(wantBase, "linux/arm64/v8", 2); got.Path != filepath.Join(wantBase.Path, "linux_arm64_v8") {
		t.Fatalf("platform output=%+v", got)
	}
}

func TestFinalizationAcceptsSharedMultiPlatformOutputs(t *testing.T) {
	for _, opts := range []Options{
		{Output: buildah.FilesystemOutput{Type: "tar", Path: "-"}},
		{Output: buildah.FilesystemOutput{Type: "tar", Path: filepath.Join(t.TempDir(), "rootfs.tar")}},
		{SBOM: []define.SBOMScanOptions{{Image: "scanner", Commands: []string{"scan"}, SBOMOutput: "sbom", PURLOutput: "purl"}}},
	} {
		if err := validateFinalization(&opts, []string{"linux/amd64", "linux/arm64"}, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFinalizationRejectsOutputOverlaps(t *testing.T) {
	root := t.TempDir()
	definition := filepath.Join(root, "image.coopr")
	if err := os.WriteFile(definition, []byte("from scratch"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, options := range []Options{
		{File: definition, Output: buildah.FilesystemOutput{Type: "local", Path: root}},
		{File: definition, Output: buildah.FilesystemOutput{Type: "local", Path: filepath.Join(root, "out")}, MetadataFile: filepath.Join(root, "out", "result.json")},
		{File: definition, SBOM: []define.SBOMScanOptions{{SBOMOutput: definition}}},
		{File: definition, Output: buildah.FilesystemOutput{Type: "tar", Path: filepath.Join(root, "cosign.key")}, Signing: transfer.SigningOptions{SigstorePrivateKeyFile: filepath.Join(root, "cosign.key")}},
	} {
		artifacts := []string{}
		if options.MetadataFile != "" {
			artifacts = append(artifacts, options.MetadataFile)
		}
		if _, err := finalizationArtifacts(options, artifacts); err == nil {
			t.Fatalf("overlap accepted: %+v", options)
		}
	}
	store := filepath.Join(root, "store")
	for _, output := range []string{store, root, filepath.Join(store, "nested")} {
		if err := preflightFilesystemOutput(buildah.FilesystemOutput{Type: "local", Path: output}, definition, store); err == nil {
			t.Fatalf("store overlap %s accepted", output)
		}
	}
}
