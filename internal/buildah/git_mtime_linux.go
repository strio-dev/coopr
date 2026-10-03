//go:build linux

package buildah

import (
	"os"
	"path/filepath"
	"slices"
	"time"

	"golang.org/x/sys/unix"
)

func resetGitCheckoutMTimes(root string, timestamp time.Time) error {
	directories := make([]string, 0)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			times := []unix.Timespec{unix.NsecToTimespec(timestamp.UnixNano()), unix.NsecToTimespec(timestamp.UnixNano())}
			return unix.UtimesNanoAt(unix.AT_FDCWD, path, times, unix.AT_SYMLINK_NOFOLLOW)
		}
		return os.Chtimes(path, timestamp, timestamp)
	})
	if err != nil {
		return err
	}
	for _, directory := range slices.Backward(directories) {
		if err := os.Chtimes(directory, timestamp, timestamp); err != nil {
			return err
		}
	}
	return nil
}
