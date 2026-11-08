package buildah

import (
	"fmt"
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
	tempDir, err := filepath.Abs(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tempDir)
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
	wantParent := tempDir
	if len(filepath.Join(tempDir, fmt.Sprintf("coopr-%d", os.Geteuid()), strings.Repeat("x", 16))) > 50 {
		wantParent = "/tmp"
	}
	if got := filepath.Dir(filepath.Dir(first)); got != wantParent {
		t.Fatalf("alias parent = %q; want %q for TMPDIR %q", got, wantParent, tempDir)
	}
	if first != second || len(first) > 50 {
		t.Fatalf("short Buildah temp aliases = %q, %q", first, second)
	}
	target, err := os.Readlink(first)
	if err != nil || target != longRoot {
		t.Fatalf("alias target = %q, %v; want %q", target, err, longRoot)
	}
}

func TestExplicitMountCacheParentSurvivesGraphRootChanges(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "mounts")
	first, err := prepareWorkerTemp(filepath.Join(root, "first-graph"), parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "volume"), []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := prepareWorkerTemp(filepath.Join(root, "second-graph"), parent)
	if err != nil || second != first {
		t.Fatalf("persistent parent changed: %q/%q %v", first, second, err)
	}
	data, err := os.ReadFile(filepath.Join(second, "volume"))
	if err != nil || string(data) != "retained" {
		t.Fatalf("native cache content lost: %q %v", data, err)
	}
	for _, resource := range []string{parent, filepath.Join(parent, "child"), root} {
		if _, err := validateCacheMountParent(parent, resource); err == nil {
			t.Fatalf("overlapping cache parent accepted: %q", resource)
		}
	}
	if _, err := prepareWorkerTemp(filepath.Join(root, "graph"), "relative"); err == nil {
		t.Fatal("relative mount cache parent accepted")
	}
}

func TestPersistentCacheBridgeRejectsExistingConflictingLocation(t *testing.T) {
	root := t.TempDir()
	worker, err := prepareWorkerTemp(filepath.Join(root, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "mounts")
	if err := preparePersistentCacheMounts(worker, parent); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(worker)
	if err != nil || len(entries) != 1 {
		t.Fatalf("native cache bridge: %v %v", entries, err)
	}
	link := filepath.Join(worker, entries[0].Name())
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(link, 0700); err != nil {
		t.Fatal(err)
	}
	if err := preparePersistentCacheMounts(worker, parent); err == nil {
		t.Fatal("existing native cache directory was silently migrated")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := preparePersistentCacheMounts(worker, parent); err == nil {
		t.Fatal("conflicting native cache link was replaced")
	}
}
