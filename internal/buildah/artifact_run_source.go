package buildah

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"go.podman.io/storage"
)

// extractArtifactRunSource reuses the verified native raw-file view unchanged
// for content/naming. Only this private RUN projection has readable mount-root
// permissions: its parent remains 0700, protecting it from other host users.
// COPY/ADD retain their own extraction modes and semantics.
func extractArtifactRunSource(ctx context.Context, store storage.Store, selected ResolvedImageSource, parents ...string) (_ string, _ func() error, retErr error) {
	parent, cleanup, err := extractArtifactSource(ctx, store, selected, parents...)
	if err != nil {
		return "", nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, cleanup())
		}
	}()
	entries, err := os.ReadDir(parent)
	if err != nil {
		return "", nil, err
	}
	view, err := os.MkdirTemp(parent, ".run-view-")
	if err != nil {
		return "", nil, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		if err := os.Rename(filepath.Join(parent, entry.Name()), filepath.Join(view, entry.Name())); err != nil {
			return "", nil, err
		}
	}
	if err := filepath.WalkDir(view, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("artifact RUN view contains a non-regular file")
		}
		return os.Chmod(path, 0444)
	}); err != nil {
		return "", nil, err
	}
	return view, cleanup, nil
}
