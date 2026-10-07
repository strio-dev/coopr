package buildah

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"coopr/internal/imagestore"
)

// DefaultStoreOptions loads the effective containers/storage configuration at
// use time, including after a user-namespace transition. Images share the same
// store as other native clients, so maintenance must preserve unowned records.
func DefaultStoreOptions() (StoreOptions, error) {
	native, err := imagestore.DefaultStoreOptions()
	if err != nil {
		return StoreOptions{}, err
	}
	return StoreOptions{
		RunRoot: native.RunRoot, GraphRoot: native.GraphRoot, ImageStore: native.ImageStore,
		GraphDriverName: native.GraphDriverName, GraphDriverOptions: append([]string(nil), native.GraphDriverOptions...),
		TransientStore: native.TransientStore, Native: native,
	}, nil
}

// canonicalStorePath resolves every existing component, including symlinks,
// while retaining a suffix that has not yet been created by containers/storage.
func canonicalStorePath(path string) (string, error) {
	resolved := string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(path), resolved), resolved)
	for index, part := range parts {
		candidate := filepath.Join(resolved, part)
		if _, err := os.Lstat(candidate); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return filepath.Join(append([]string{resolved}, parts[index:]...)...), nil
			}
			return "", err
		}
		var err error
		resolved, err = filepath.EvalSymlinks(candidate)
		if err != nil {
			return "", err
		}
	}
	return resolved, nil
}

func pathsOverlap(left, right string) bool {
	return pathContains(left, right) || pathContains(right, left)
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
