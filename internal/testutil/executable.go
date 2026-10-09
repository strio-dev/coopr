package testutil

import (
	"fmt"
	"os"
	"path/filepath"
)

func ValidateExecutable(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("must be an absolute path to an executable file")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("must be an executable file: %s", path)
	}
	return nil
}
