package buildah

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"
)

// prepareWorkerTemp gives Buildah's RUN cache mounts a stable, private path
// associated with the canonical execution store.
func prepareWorkerTemp(graphRoot string) (string, error) {
	canonicalRoot, err := canonicalStorePath(graphRoot)
	if err != nil {
		return "", fmt.Errorf("resolve Buildah graph root: %w", err)
	}
	directory := canonicalRoot + "-tmp"
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
	base := filepath.Join("/tmp", fmt.Sprintf("coopr-%d", os.Geteuid()))
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
	alias := filepath.Join(base, digest.FromString(directory).Encoded()[:16])
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
