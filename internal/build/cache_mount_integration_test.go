package build

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRunMountOnlyDefinitionWithoutProjectLock(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for live Buildah tests")
	}
	dir := t.TempDir()
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatal("live Buildah tests require the Nix dev shell's static busybox binary")
	}
	binary, err := os.ReadFile(busybox)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "busybox"), binary, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "app.coopr")
	source := "from \"scratch\"\ncopy \"busybox\" \"/bin/sh\" chmod=\"0755\"\nrun \"echo marker > /out\" network=\"none\" { mount \"cache\" target=\"/cache\" }\n"
	if err := os.WriteFile(file, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		output := filepath.Join(dir, []string{"first.oci.tar", "second.oci.tar"}[i])
		if _, err := Run(context.Background(), Options{File: file, Context: dir, Tag: "oci-archive:" + output, Platform: "linux/" + runtime.GOARCH}); err != nil {
			t.Fatal(err)
		}
		if names := archiveLayerNames(t, output); !containsName(names, "out") {
			t.Fatalf("build %d archive has no RUN marker: %v", i, names)
		}
		if config := archiveImageConfig(t, output); config.OS != "linux" || config.Architecture != runtime.GOARCH {
			t.Fatalf("build %d platform = %s/%s, want linux/%s", i, config.OS, config.Architecture, runtime.GOARCH)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "coopr.lock")); !os.IsNotExist(err) {
		t.Fatalf("build created a project lockfile: %v", err)
	}
}
