package buildah

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSnapshotWritableBindFilePreservesMetadataAndDiscardsWrites(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	path, cleanup, err := snapshotWritableBindFile(source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	if path == source {
		t.Fatal("RUN bind snapshot aliases its source")
	}
	before, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode() != before.Mode() {
		t.Fatalf("snapshot mode = %v, want %v", after.Mode(), before.Mode())
	}
	beforeOwner := before.Sys().(*syscall.Stat_t)
	afterOwner := after.Sys().(*syscall.Stat_t)
	if afterOwner.Uid != beforeOwner.Uid || afterOwner.Gid != beforeOwner.Gid {
		t.Fatalf("snapshot owner = %d:%d, want %d:%d", afterOwner.Uid, afterOwner.Gid, beforeOwner.Uid, beforeOwner.Gid)
	}
	if err := os.WriteFile(path, []byte("changed"), 0o640); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(original) != "original" {
		t.Fatalf("source was mutated by RUN bind write: %q", original)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("RUN bind snapshot remains after cleanup: %v", err)
	}
}
