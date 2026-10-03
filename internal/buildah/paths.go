package buildah

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"coopr/internal/imagestore"
	"coopr/internal/localstore"
)

// DefaultStoreOptions uses the persistent Coopr image graph for both build
// outputs and image inputs. Only the run root belongs in the runtime/cache
// directory; image bytes must survive cache cleanup.
func DefaultStoreOptions() (StoreOptions, error) {
	imageDir, err := localstore.DefaultImageDir()
	if err != nil {
		return StoreOptions{}, err
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return StoreOptions{}, fmt.Errorf("find user cache directory: %w", err)
	}
	if !filepath.IsAbs(cacheDir) {
		return StoreOptions{}, fmt.Errorf("user cache directory must be absolute: %q", cacheDir)
	}
	root := filepath.Join(cacheDir, "coopr", "buildah")
	runRoot := filepath.Join(root, "run")
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		if !filepath.IsAbs(runtimeDir) {
			return StoreOptions{}, fmt.Errorf("XDG_RUNTIME_DIR must be absolute: %q", runtimeDir)
		}
		runRoot = filepath.Join(runtimeDir, "coopr", "buildah")
	}
	return StoreOptions{GraphRoot: filepath.Join(imageDir, "graph"), RunRoot: runRoot}, nil
}

// PodmanStoreOptions loads the effective host containers/storage
// configuration without caching it across a later user-namespace transition.
func PodmanStoreOptions() (StoreOptions, error) {
	native, err := imagestore.DefaultStoreOptions()
	if err != nil {
		return StoreOptions{}, err
	}
	return StoreOptions{
		RunRoot: native.RunRoot, GraphRoot: native.GraphRoot, ImageStore: native.ImageStore,
		GraphDriverName: native.GraphDriverName, GraphDriverOptions: append([]string(nil), native.GraphDriverOptions...),
		TransientStore: native.TransientStore, Native: native, Shared: true,
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
