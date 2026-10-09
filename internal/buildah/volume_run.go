package buildah

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"go.podman.io/buildah/copier"
	"go.podman.io/storage/pkg/chrootarchive"
)

// preserveRunVolumes follows imagebuildah's VFS save/restore strategy. Saving
// the current directory before each RUN naturally includes COPY/ADD changes,
// even when the stage uses a single mutable layer.
func (b nativeBuilder) preserveRunVolumes() (restore func() error, retErr error) {
	root, err := b.Mount(b.MountLabel)
	if err != nil {
		return nil, err
	}
	type savedVolume struct {
		path, archive string
		info          os.FileInfo
	}
	var saved []savedVolume
	temporary, err := os.MkdirTemp("", "coopr-volume-")
	if err != nil {
		return nil, errors.Join(err, b.Unmount())
	}
	cleanup := func() error { return errors.Join(os.RemoveAll(temporary), b.Unmount()) }
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, cleanup())
		}
	}()
	volumes := b.Volumes()
	slices.Sort(volumes)
	for _, volume := range volumes {
		path, err := copier.Eval(root, filepath.Join(root, volume), copier.EvalOptions{})
		if err != nil {
			return nil, err
		}
		covered := false
		for _, prior := range saved {
			if path == prior.path || strings.HasPrefix(path, prior.path+string(os.PathSeparator)) {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		archive := filepath.Join(temporary, fmt.Sprintf("%d.tar", len(saved)))
		file, err := os.Create(archive)
		if err != nil {
			return nil, err
		}
		reader, err := chrootarchive.Tar(path, nil, root)
		if err != nil {
			return nil, errors.Join(err, file.Close())
		}
		_, copyErr := io.Copy(file, reader)
		if err := errors.Join(copyErr, reader.Close(), file.Close()); err != nil {
			return nil, err
		}
		saved = append(saved, savedVolume{path: path, archive: archive, info: info})
	}
	return func() (retErr error) {
		defer func() { retErr = errors.Join(retErr, cleanup()) }()
		for _, volume := range saved {
			// RUN can replace a parent directory with a symlink. Re-evaluate
			// inside the container root immediately before restoring, matching
			// imagebuildah's volumeCacheRestoreVFS confinement.
			path, err := copier.Eval(root, volume.path, copier.EvalOptions{})
			if err != nil {
				return fmt.Errorf("evaluate volume restore path: %w", err)
			}
			file, err := os.Open(volume.archive)
			if err != nil {
				return err
			}
			err = copier.Remove(root, path, copier.RemoveOptions{All: true})
			if err == nil {
				err = chrootarchive.Untar(file, path, nil)
			}
			err = errors.Join(err, file.Close())
			if err != nil {
				return err
			}
			if err := os.Chmod(path, volume.info.Mode()); err != nil {
				return err
			}
			if stat, ok := volume.info.Sys().(*syscall.Stat_t); ok {
				if err := os.Chown(path, int(stat.Uid), int(stat.Gid)); err != nil {
					return err
				}
			}
			if err := os.Chtimes(path, volume.info.ModTime(), volume.info.ModTime()); err != nil {
				return err
			}
		}
		return nil
	}, nil
}
