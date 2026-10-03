package buildah

import (
	"path/filepath"
	"testing"
)

func TestValidateFinalizationOutputAcceptsRepeatableFilesystems(t *testing.T) {
	root := t.TempDir()
	if err := validateFinalizationOutput(Output{Filesystems: []FilesystemOutput{{Type: "local", Path: root}, {Type: "tar", Path: filepath.Join(root, "rootfs.tar")}}}); err != nil {
		t.Fatal(err)
	}
}
