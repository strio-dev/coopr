package buildah

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// An explicit fixture directory must be usable; only an absent override compiles locally.
func runtimeFixtureBinary(name string) (string, error) {
	root, supplied := os.LookupEnv("COOPR_TEST_FIXTURES")
	if !supplied {
		return "", nil
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("COOPR_TEST_FIXTURES must be an absolute directory")
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("COOPR_TEST_FIXTURES: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("COOPR_TEST_FIXTURES must be a directory: %s", root)
	}
	path := filepath.Join(root, name)
	info, err = os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("COOPR_TEST_FIXTURES: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("COOPR_TEST_FIXTURES must contain an executable file: %s", path)
	}
	return path, nil
}

func TestRuntimeFixtureBinaryOverride(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		t.Setenv("COOPR_TEST_FIXTURES", "")
		if err := os.Unsetenv("COOPR_TEST_FIXTURES"); err != nil {
			t.Fatal(err)
		}
		if path, err := runtimeFixtureBinary("proof"); path != "" || err != nil {
			t.Fatalf("absent override: path=%q error=%v", path, err)
		}
	})
	root := t.TempDir()
	executable := filepath.Join(root, "proof")
	if err := os.WriteFile(executable, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	nonExecutable := filepath.Join(root, "non-executable")
	if err := os.WriteFile(nonExecutable, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, directory, binary string
		wantError               bool
	}{
		{"empty", "", "proof", true},
		{"relative", "fixtures", "proof", true},
		{"missing-directory", filepath.Join(root, "missing"), "proof", true},
		{"file-directory", executable, "proof", true},
		{"missing-binary", root, "missing", true},
		{"non-executable", root, "non-executable", true},
		{"directory-binary", root, ".", true},
		{"executable", root, "proof", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("COOPR_TEST_FIXTURES", test.directory)
			path, err := runtimeFixtureBinary(test.binary)
			if (err != nil) != test.wantError {
				t.Fatalf("path=%q error=%v, want error=%t", path, err, test.wantError)
			}
			if err == nil && path != executable {
				t.Fatalf("path=%q, want %q", path, executable)
			}
		})
	}
}
