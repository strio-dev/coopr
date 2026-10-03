package buildah

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestResetGitCheckoutMTimesIncludesFilesSymlinksAndDirectories(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("symlink mtime normalization is Linux-specific")
	}
	root := t.TempDir()
	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "file")
	if err := os.WriteFile(file, []byte("value"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	outsideTimestamp := time.Unix(123456789, 0)
	if err := os.Chtimes(outside, outsideTimestamp, outsideTimestamp); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	timestamp := time.Unix(946684800, 0)
	if err := resetGitCheckoutMTimes(root, timestamp); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, directory, file, link} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(timestamp) {
			t.Errorf("mtime for %q = %s, want %s", path, info.ModTime(), timestamp)
		}
	}
	outsideInfo, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if !outsideInfo.ModTime().Equal(outsideTimestamp) {
		t.Errorf("outside symlink target mtime = %s, want %s", outsideInfo.ModTime(), outsideTimestamp)
	}
}
