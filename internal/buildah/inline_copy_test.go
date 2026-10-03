package buildah

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/definition"
	upstream "go.podman.io/buildah"
)

func TestAddInlineDataRejectsUnsafeSourcePath(t *testing.T) {
	for _, path := range []string{"../escape", "/absolute", "."} {
		err := addInlineData(&recordingBuilder{}, "/target", definition.InlineFile{Path: path, Data: "payload"}, upstream.AddAndCopyOptions{})
		if err == nil || !strings.Contains(err.Error(), "is not local") {
			t.Fatalf("addInlineData path %q error = %v", path, err)
		}
	}
}

func TestMaterializeRunInlineFilesUsesDeterministicMetadata(t *testing.T) {
	directory, cleanup, err := materializeRunInlineFiles([]definition.InlineFile{{Path: "SCRIPT", Data: "#!/bin/sh\n"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	info, err := os.Stat(filepath.Join(directory, "SCRIPT"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 || info.ModTime().Unix() != 0 {
		t.Fatalf("RUN inline metadata = mode %o mtime %v", info.Mode().Perm(), info.ModTime())
	}
	rootInfo, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.Mode().Perm() != 0o755 || rootInfo.ModTime().Unix() != 0 {
		t.Fatalf("RUN inline root metadata = mode %o mtime %v", rootInfo.Mode().Perm(), rootInfo.ModTime())
	}
}
