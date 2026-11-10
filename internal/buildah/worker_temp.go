package buildah

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"
	"go.podman.io/storage/pkg/unshare"
	"golang.org/x/sys/unix"
)

// prepareWorkerTemp gives Buildah's RUN cache mounts a stable, private path
// associated with the canonical execution store.
func prepareWorkerTemp(graphRoot string, mountDirs ...string) (string, error) {
	canonicalRoot, err := canonicalStorePath(graphRoot)
	if err != nil {
		return "", fmt.Errorf("resolve Buildah graph root: %w", err)
	}
	directory := canonicalRoot + "-tmp"
	if len(mountDirs) > 1 {
		return "", errors.New("multiple mount cache parents supplied")
	}
	if len(mountDirs) == 1 && mountDirs[0] != "" {
		if !filepath.IsAbs(mountDirs[0]) {
			return "", errors.New("mount cache parent must be absolute")
		}
		directory, err = canonicalStorePath(mountDirs[0])
		if err != nil {
			return "", fmt.Errorf("resolve mount cache parent: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(directory), 0o700); err != nil {
		return "", fmt.Errorf("create Buildah cache parent: %w", err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("create Buildah cache directory: %w", err)
	}
	fd, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("open Buildah cache directory: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return "", fmt.Errorf("inspect Buildah cache directory: %w", err)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("buildah cache directory %q is owned by UID %d, expected %d", directory, stat.Uid, os.Geteuid())
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		return "", fmt.Errorf("secure Buildah cache directory: %w", err)
	}
	return directory, nil
}

// Buildah creates SSH forwarding sockets below TMPDIR. A long graph-root path
// can exceed Linux's Unix-socket path limit, so give that same persistent
// directory a short, private alias when needed.
func socketSafeWorkerTemp(directory string) (string, error) {
	const maxTempPrefix = 50 // Leave room for Buildah's socket subdirectory and name.
	if len(directory) <= maxTempPrefix {
		return directory, nil
	}
	tempDir, err := filepath.Abs(os.TempDir())
	if err != nil {
		return "", fmt.Errorf("resolve Buildah temp alias parent: %w", err)
	}
	name := digest.FromString(directory).Encoded()[:16]
	base := filepath.Join(tempDir, fmt.Sprintf("coopr-%d", os.Geteuid()))
	if len(filepath.Join(base, name)) > maxTempPrefix {
		base = filepath.Join("/tmp", fmt.Sprintf("coopr-%d", os.Geteuid()))
	}
	if err := os.Mkdir(base, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("create short Buildah temp alias directory: %w", err)
	}
	fd, err := unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("open short Buildah temp alias directory: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return "", fmt.Errorf("inspect short Buildah temp alias directory: %w", err)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("short Buildah temp alias directory is owned by UID %d, expected %d", stat.Uid, os.Geteuid())
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		return "", fmt.Errorf("secure short Buildah temp alias directory: %w", err)
	}
	alias := filepath.Join(base, name)
	if err := os.Symlink(directory, alias); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("create short Buildah temp alias: %w", err)
	}
	target, err := os.Readlink(alias)
	if err != nil {
		return "", fmt.Errorf("read short Buildah temp alias: %w", err)
	}
	if target != directory {
		return "", fmt.Errorf("short Buildah temp alias points to %q instead of %q", target, directory)
	}
	return alias, nil
}

// validateCacheMountParent keeps a persistent acceleration root separate from
// authoritative inputs, native storage, portable results and caller outputs.
func validateCacheMountParent(directory string, resources ...string) (string, error) {
	if directory == "" {
		return "", nil
	}
	if !filepath.IsAbs(directory) {
		return "", errors.New("mount cache parent must be absolute")
	}
	canonical, err := canonicalStorePath(directory)
	if err != nil {
		return "", fmt.Errorf("mount cache parent: %w", err)
	}
	for _, resource := range resources {
		if resource == "" {
			continue
		}
		other, err := canonicalStorePath(resource)
		if err != nil {
			return "", err
		}
		within := func(parent, child string) bool {
			rel, err := filepath.Rel(parent, child)
			return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
		}
		if within(canonical, other) || within(other, canonical) {
			return "", errors.New("mount cache parent overlaps context, native storage, result cache or output")
		}
	}
	return canonical, nil
}

// preparePersistentCacheMounts relocates only Buildah's existing cache subtree.
// All other temporary content retains the ordinary execution-store lifetime.
// The native directory/lock naming and cache implementation remain upstream.
func preparePersistentCacheMounts(workerTemp, mountDir string) error {
	parent, err := prepareWorkerTemp(workerTemp, mountDir)
	if err != nil {
		return err
	}
	name := "buildah-cache-" + strconv.Itoa(unshare.GetRootlessUID())
	target := filepath.Join(parent, name)
	if err := os.Mkdir(target, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("prepare native persistent cache: %w", err)
	}
	info, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("native persistent cache root must be a directory")
	}
	if _, err := prepareWorkerTemp(workerTemp, target); err != nil {
		return err
	}
	link := filepath.Join(workerTemp, name)
	if err := os.Symlink(target, link); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("link native persistent cache: %w", err)
	}
	actual, err := os.Readlink(link)
	if err != nil {
		return fmt.Errorf("native cache location already exists without the requested persistent binding: %w", err)
	}
	if actual != target {
		return fmt.Errorf("native cache location points to %q instead of %q", actual, target)
	}
	return nil
}
