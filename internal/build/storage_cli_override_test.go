package build

import (
	"os"
	"path/filepath"
	"testing"

	"coopr/internal/testutil"
)

func TestStorageTestCLIRejectsInvalidExecutable(t *testing.T) {
	root := t.TempDir()
	nonExecutable := filepath.Join(root, "coopr")
	if err := os.WriteFile(nonExecutable, []byte("not executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"empty": "", "relative": "coopr", "missing": filepath.Join(root, "missing"),
		"directory": root, "non-executable": nonExecutable,
	} {
		t.Run(name, func(t *testing.T) {
			if err := testutil.ValidateExecutable(path); err == nil {
				t.Fatalf("accepted invalid executable %q", path)
			}
		})
	}
}
