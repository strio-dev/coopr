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
	path, cleanup, err := prepareRunBindFile(source, []string{"rw"})
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

func TestReadOnlyImageFileBindIsolatesMetadataChanges(t *testing.T) {
	for _, option := range []string{"U", "z", "Z"} {
		t.Run(option, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "source")
			if err := os.WriteFile(source, []byte("original"), 0o640); err != nil {
				t.Fatal(err)
			}
			path, cleanup, err := prepareRunBindFile(source, []string{"ro", "rbind", option})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cleanup() }()
			before, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			mounted, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if os.SameFile(before, mounted) {
				t.Fatal("metadata-changing readonly mount aliases its source image file")
			}
			// Runtime mount setup may change metadata before applying readonly.
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			if after.Mode() != before.Mode() {
				t.Fatalf("source metadata changed: %v -> %v", before.Mode(), after.Mode())
			}
			if err := cleanup(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("RUN bind snapshot remains after cleanup: %v", err)
			}
		})
	}
}

func TestReadOnlyImageFileBindWithoutMetadataChangesKeepsSource(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	path, cleanup, err := prepareRunBindFile(source, []string{"ro", "rbind", "noexec", "nosuid"})
	if err != nil {
		t.Fatal(err)
	}
	if path != source {
		t.Fatal("unmodified readonly source was copied unnecessarily")
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("readonly source removed by cleanup: %v", err)
	}
}
