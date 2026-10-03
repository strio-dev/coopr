package buildah

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"golang.org/x/sys/unix"
)

type runCacheLocks struct {
	files []*os.File
}

func (locks *runCacheLocks) close() error {
	if locks == nil {
		return nil
	}
	var result error
	for index := len(locks.files) - 1; index >= 0; index-- {
		file := locks.files[index]
		result = errors.Join(result, unix.Flock(int(file.Fd()), unix.LOCK_UN), file.Close())
	}
	locks.files = nil
	return result
}

func lockRunCacheMounts(ctx context.Context, graphRoot string, mounts []RunMount) (*runCacheLocks, error) {
	ids := lockedRunCacheMountIDs(mounts)
	if len(ids) == 0 {
		return &runCacheLocks{}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if graphRoot == "" {
		return nil, errors.New("locked RUN cache mount requires a containers/storage graph root")
	}
	lockDir := filepath.Join(graphRoot, "coopr-cache-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("create cache lock directory: %w", err)
	}
	locks := &runCacheLocks{}
	for _, id := range ids {
		name := fmt.Sprintf("%x.lock", sha256.Sum256([]byte(id)))
		file, err := os.OpenFile(filepath.Join(lockDir, name), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("open cache mount lock: %w", err), locks.close())
		}
		for {
			if err := ctx.Err(); err != nil {
				_ = file.Close()
				return nil, errors.Join(err, locks.close())
			}
			err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
			if err == nil {
				if err := ctx.Err(); err != nil {
					_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
					_ = file.Close()
					return nil, errors.Join(err, locks.close())
				}
				break
			}
			if !errors.Is(err, unix.EWOULDBLOCK) {
				_ = file.Close()
				return nil, errors.Join(fmt.Errorf("lock cache mount: %w", err), locks.close())
			}
			timer := time.NewTimer(25 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				_ = file.Close()
				return nil, errors.Join(ctx.Err(), locks.close())
			case <-timer.C:
			}
		}
		locks.files = append(locks.files, file)
	}
	return locks, nil
}

func lockedRunCacheMountIDs(mounts []RunMount) []string {
	unique := make(map[string]bool)
	for _, mount := range mounts {
		if mount.Type == "cache" && mount.Properties["sharing"] == "locked" && mount.Properties["id"] != "" {
			unique[mount.Properties["id"]] = true
		}
	}
	ids := make([]string, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
