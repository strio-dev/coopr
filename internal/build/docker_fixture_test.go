package build

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func dockerEngineWorkspace(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	// Committed storage layers can retain read-only root-directory modes.
	// Restore owner permissions before testing.TempDir removes this owned graph.
	t.Cleanup(func() {
		if err := filepath.WalkDir(work, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.Chmod(path, 0o700)
			}
			return nil
		}); err != nil {
			t.Errorf("prepare test graph cleanup: %v", err)
		}
	})
	return work
}
