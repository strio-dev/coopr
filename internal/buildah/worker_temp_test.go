package buildah

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareWorkerTempUsesCanonicalGraphRootAndPrivatePermissions(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, "actual")
	if err := os.Mkdir(actual, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(actual, alias); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(actual, "graph-tmp")
	got, err := prepareWorkerTemp(filepath.Join(alias, "graph"))
	if err != nil || got != want {
		t.Fatalf("worker temp = %q, %v; want %q", got, err, want)
	}
	if err := os.Chmod(want, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareWorkerTemp(filepath.Join(actual, "graph")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("worker temp mode = %v; want 0700", info.Mode())
	}
}

func TestPrepareWorkerTempRejectsNonDirectories(t *testing.T) {
	root := t.TempDir()
	graphRoot := filepath.Join(root, "graph")
	temp := graphRoot + "-tmp"
	innocent := filepath.Join(root, "innocent")
	if err := os.Mkdir(innocent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(innocent, temp); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareWorkerTemp(graphRoot); err == nil {
		t.Fatal("accepted symlinked worker temp")
	}
	if err := os.Remove(temp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temp, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareWorkerTemp(graphRoot); err == nil {
		t.Fatal("accepted regular file as worker temp")
	}
}

func TestSocketSafeWorkerTempUsesShortStableAlias(t *testing.T) {
	longRoot := filepath.Join(t.TempDir(), strings.Repeat("nested", 15))
	if err := os.MkdirAll(longRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := socketSafeWorkerTemp(longRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(first) })
	second, err := socketSafeWorkerTemp(longRoot)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) > 50 {
		t.Fatalf("short Buildah temp aliases = %q, %q", first, second)
	}
	target, err := os.Readlink(first)
	if err != nil || target != longRoot {
		t.Fatalf("alias target = %q, %v; want %q", target, err, longRoot)
	}
}
