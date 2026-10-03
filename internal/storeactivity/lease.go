// Package storeactivity coordinates long-lived Coopr store operations with
// destructive maintenance across processes.
package storeactivity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gofrs/flock"
)

const lockFilename = ".coopr-activity.lock"

type Lease struct {
	locks     []*flock.Flock
	paths     []string
	exclusive bool
}

type leaseContextKey struct{}

// ContextWithLease records a live lease so nested Coopr operations can reuse
// the same lock instead of deadlocking while reacquiring or upgrading it.
func ContextWithLease(ctx context.Context, lease *Lease) context.Context {
	if lease == nil || len(lease.locks) == 0 {
		return ctx
	}
	held, _ := ctx.Value(leaseContextKey{}).([]*Lease)
	next := append([]*Lease(nil), held...)
	next = append(next, lease)
	return context.WithValue(ctx, leaseContextKey{}, next)
}

func AcquireShared(ctx context.Context, roots ...string) (*Lease, error) {
	return acquire(ctx, false, roots)
}

func AcquireExclusive(ctx context.Context, roots ...string) (*Lease, error) {
	return acquire(ctx, true, roots)
}

func acquire(ctx context.Context, exclusive bool, roots []string) (*Lease, error) {
	paths := make([]string, 0, len(roots))
	seen := make(map[string]bool, len(roots))
	for _, root := range roots {
		if root == "" {
			continue
		}
		path, err := canonicalRoot(root)
		if err != nil {
			return nil, err
		}
		if !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	for _, lease := range contextLeases(ctx) {
		if lease.covers(paths, exclusive) {
			return &Lease{}, nil
		}
	}
	lease := &Lease{paths: append([]string(nil), paths...), exclusive: exclusive}
	for _, root := range paths {
		lock := flock.New(filepath.Join(root, lockFilename))
		var locked bool
		var err error
		if exclusive {
			locked, err = lock.TryLockContext(ctx, 25*time.Millisecond)
		} else {
			locked, err = lock.TryRLockContext(ctx, 25*time.Millisecond)
		}
		if err != nil || !locked {
			_ = lease.Close()
			if err != nil {
				return nil, err
			}
			return nil, errors.New("store activity lock not acquired")
		}
		lease.locks = append(lease.locks, lock)
	}
	return lease, nil
}

func contextLeases(ctx context.Context) []*Lease {
	if ctx == nil {
		return nil
	}
	leases, _ := ctx.Value(leaseContextKey{}).([]*Lease)
	return leases
}

func (lease *Lease) covers(paths []string, exclusive bool) bool {
	if lease == nil || len(lease.locks) == 0 || exclusive && !lease.exclusive {
		return false
	}
	for _, path := range paths {
		index := sort.SearchStrings(lease.paths, path)
		if index == len(lease.paths) || lease.paths[index] != path {
			return false
		}
	}
	return true
}

func canonicalRoot(root string) (string, error) {
	path, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

func (lease *Lease) Close() error {
	if lease == nil {
		return nil
	}
	var result error
	for index := len(lease.locks) - 1; index >= 0; index-- {
		result = errors.Join(result, lease.locks[index].Unlock())
	}
	lease.locks = nil
	lease.paths = nil
	return result
}
