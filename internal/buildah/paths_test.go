package buildah

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultStoreOptionsUseCooprImageDataAndRuntimeNamespace(t *testing.T) {
	cache := t.TempDir()
	data := t.TempDir()
	runtimeDir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	got, err := DefaultStoreOptions()
	if err != nil {
		t.Fatal(err)
	}
	if got.GraphRoot != filepath.Join(data, "coopr", "images", "graph") || got.RunRoot != filepath.Join(runtimeDir, "coopr", "buildah") {
		t.Fatalf("default Buildah store paths = %+v", got)
	}
	if _, err := os.Stat(got.GraphRoot); !os.IsNotExist(err) {
		t.Fatalf("finding default paths created a store: %v", err)
	}
}

func TestDefaultStoreOptionsRejectsRelativeRuntimeDir(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", "relative")
	if _, err := DefaultStoreOptions(); err == nil {
		t.Fatal("relative runtime directory accepted")
	}
}

func TestCanonicalStorePathDetectsSymlinkedOverlap(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	runRoot, err := canonicalStorePath(filepath.Join(alias, "run"))
	if err != nil {
		t.Fatal(err)
	}
	if runRoot != filepath.Join(real, "run") {
		t.Fatalf("canonical path = %q, want %q", runRoot, filepath.Join(real, "run"))
	}
	graphRoot, err := canonicalStorePath(filepath.Join(real, "run", "graph"))
	if err != nil {
		t.Fatal(err)
	}
	if !pathsOverlap(runRoot, graphRoot) {
		t.Fatalf("symlinked store roots do not overlap: %q, %q", runRoot, graphRoot)
	}
	if pathsOverlap(filepath.Join(real, "run"), filepath.Join(real, "graph")) {
		t.Fatal("sibling store roots overlap")
	}
}
