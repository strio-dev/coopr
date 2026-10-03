package build

import (
	"path/filepath"
	"testing"

	"coopr/internal/buildah"
)

func TestValidateFinalizationAcceptsRepeatableFilesystemOutputs(t *testing.T) {
	root := t.TempDir()
	opts := Options{Outputs: []buildah.FilesystemOutput{
		{Type: "local", Path: filepath.Join(root, "rootfs")},
		{Type: "tar", Path: filepath.Join(root, "rootfs.tar")},
	}}
	if err := validateFinalization(&opts, []string{"linux/amd64"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(opts.Outputs) != 2 {
		t.Fatalf("outputs = %#v", opts.Outputs)
	}
}

func TestValidateFinalizationRejectsMultipleStdoutOutputs(t *testing.T) {
	opts := Options{Outputs: []buildah.FilesystemOutput{{Type: "tar", Path: "-"}, {Type: "tar", Path: "-"}}}
	if err := validateFinalization(&opts, []string{"linux/amd64"}, nil); err == nil {
		t.Fatal("expected multiple stdout outputs to fail")
	}
}
