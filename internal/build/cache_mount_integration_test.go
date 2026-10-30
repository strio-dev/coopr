package build

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/strio-dev/coopr/internal/buildah"
)

func TestRunCacheMountPersistsAcrossBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native build integration")
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
	source := `from "scratch" {
    copy "busybox" "/bin/sh" chmod="0755"
    run "if test -f /cache/proof; then printf hit >/out; else printf miss >/out; fi; printf marker >/cache/proof" network="none" { mount "cache" target="/cache" }
}
`
	if err := os.WriteFile(file, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	store := nativeBuildTestStore(filepath.Join(t.TempDir(), "store"))
	for i, want := range []string{"miss", "hit"} {
		output := filepath.Join(dir, want+".oci.tar")
		rootfs := filepath.Join(t.TempDir(), "rootfs")
		if _, err := buildArchive(t, output, context.Background(), Options{
			File: file, Context: dir, Platform: "linux/" + runtime.GOARCH,
			BuildStore: store, NoCache: i > 0,
			Output: buildah.FilesystemOutput{Type: "local", Path: rootfs},
		}); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(rootfs, "out")); err != nil || string(got) != want {
			t.Fatalf("build %d cache result = %q, %v; want %q", i, got, err, want)
		}
		if names := archiveLayerNames(t, output); !containsName(names, "out") || containsName(names, "cache/proof") {
			t.Fatalf("build %d archive must retain the result without cache contents: %v", i, names)
		}
		if config := archiveImageConfig(t, output); config.OS != "linux" || config.Architecture != runtime.GOARCH {
			t.Fatalf("build %d platform = %s/%s, want linux/%s", i, config.OS, config.Architecture, runtime.GOARCH)
		}
	}
}
