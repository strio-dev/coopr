package buildah

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/bboltcachestorage"
)

// graphCacheKeyStorage opens BuildKit's metadata database only for individual
// operations. Daemonless builds share the outer lock, never a long-lived DB
// handle. All users of this database must follow this locking protocol.
type graphCacheKeyStorage struct {
	ctx  context.Context
	path string
	// Set once before publishing this adapter to a cache manager. The callback
	// checks durable result availability and must not re-enter key storage.
	keepResult func(string) bool
	mu         sync.Mutex
	err        error
}

var _ solver.CacheKeyStorage = (*graphCacheKeyStorage)(nil)

func newGraphCacheKeyStorage(ctx context.Context, path string) (*graphCacheKeyStorage, error) {
	if ctx == nil {
		return nil, errors.New("graph cache requires a context")
	}
	if path == "" {
		return nil, errors.New("graph cache requires a database path")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("graph cache directory %q must be private (0700)", parent)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("graph cache directory %q must belong to the current user", parent)
	}
	// Resolve ancestor aliases so every caller uses the same adjacent lock.
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, err
	}
	s := &graphCacheKeyStorage{ctx: ctx, path: filepath.Join(parent, filepath.Base(path))}
	if err := s.withStore(func(*bboltcachestorage.Store) error { return nil }); err != nil {
		return nil, err
	}
	return s, nil
}

// Err exposes the first storage failure, including failures hidden by BuildKit's
// boolean interface methods. A missing cache entry is not a storage failure.
func (s *graphCacheKeyStorage) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *graphCacheKeyStorage) withStore(fn func(*bboltcachestorage.Store) error) (err error) {
	defer func() {
		if err != nil && !errors.Is(err, solver.ErrNotFound) {
			s.mu.Lock()
			if s.err == nil {
				s.err = err
			}
			s.mu.Unlock()
		}
	}()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	for _, path := range []string{s.path, s.path + ".lock"} {
		if err := graphCacheRegularFile(path); err != nil {
			return err
		}
	}
	lock := flock.New(s.path + ".lock")
	locked, err := lock.TryLockContext(s.ctx, 25*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		return errors.New("graph cache lock not acquired")
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if err := graphCacheRegularFile(s.path); err != nil {
		return err
	}
	db, err := bboltcachestorage.NewStore(s.path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	return fn(db)
}

func graphCacheRegularFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("graph cache path %q must be a regular file", path)
	}
	return nil
}

func (s *graphCacheKeyStorage) Exists(id string) bool {
	var found bool
	err := s.withStore(func(db *bboltcachestorage.Store) error { found = db.Exists(id); return nil })
	return err == nil && found
}

func (s *graphCacheKeyStorage) HasLink(id string, link solver.CacheInfoLink, target string) bool {
	var found bool
	err := s.withStore(func(db *bboltcachestorage.Store) error { found = db.HasLink(id, link, target); return nil })
	return err == nil && found
}

func (s *graphCacheKeyStorage) Load(id, resultID string) (result solver.CacheResult, err error) {
	err = s.withStore(func(db *bboltcachestorage.Store) error {
		var err error
		result, err = db.Load(id, resultID)
		return err
	})
	return result, err
}

func (s *graphCacheKeyStorage) AddResult(id string, result solver.CacheResult) error {
	return s.withStore(func(db *bboltcachestorage.Store) error { return db.AddResult(id, result) })
}

func (s *graphCacheKeyStorage) Release(resultID string) error {
	return s.withStore(func(db *bboltcachestorage.Store) error {
		// BuildKit observes availability before requesting Release. Another
		// process may have refreshed that result in the meantime. Recheck under
		// the key lock so an already-published fresh association survives; a
		// concurrent AddResult after this transaction restores its association.
		if s.keepResult != nil && s.keepResult(resultID) {
			return nil
		}
		return db.Release(resultID)
	})
}

func (s *graphCacheKeyStorage) AddLink(id string, link solver.CacheInfoLink, target string) error {
	return s.withStore(func(db *bboltcachestorage.Store) error { return db.AddLink(id, link, target) })
}

// Walk callbacks run after closing the database and releasing the process lock:
// solver cache cleanup callbacks re-enter this storage to walk and release keys.
func (s *graphCacheKeyStorage) Walk(fn func(string) error) error {
	var rows []string
	if err := s.withStore(func(db *bboltcachestorage.Store) error {
		return db.Walk(func(id string) error { rows = append(rows, id); return nil })
	}); err != nil {
		return err
	}
	for _, row := range rows {
		if err := fn(row); err != nil {
			return err
		}
	}
	return nil
}

func (s *graphCacheKeyStorage) WalkResults(id string, fn func(solver.CacheResult) error) error {
	var rows []solver.CacheResult
	if err := s.withStore(func(db *bboltcachestorage.Store) error {
		return db.WalkResults(id, func(row solver.CacheResult) error { rows = append(rows, row); return nil })
	}); err != nil {
		return err
	}
	for _, row := range rows {
		if err := fn(row); err != nil {
			return err
		}
	}
	return nil
}

func (s *graphCacheKeyStorage) WalkIDsByResult(resultID string, fn func(string) error) error {
	var rows []string
	if err := s.withStore(func(db *bboltcachestorage.Store) error {
		return db.WalkIDsByResult(resultID, func(id string) error { rows = append(rows, id); return nil })
	}); err != nil {
		return err
	}
	for _, row := range rows {
		if err := fn(row); err != nil {
			return err
		}
	}
	return nil
}

func (s *graphCacheKeyStorage) WalkLinks(id string, link solver.CacheInfoLink, fn func(string) error) error {
	var rows []string
	if err := s.withStore(func(db *bboltcachestorage.Store) error {
		return db.WalkLinks(id, link, func(target string) error { rows = append(rows, target); return nil })
	}); err != nil {
		return err
	}
	for _, row := range rows {
		if err := fn(row); err != nil {
			return err
		}
	}
	return nil
}

func (s *graphCacheKeyStorage) WalkBacklinks(id string, fn func(string, solver.CacheInfoLink) error) error {
	type backlink struct {
		id   string
		link solver.CacheInfoLink
	}
	var rows []backlink
	if err := s.withStore(func(db *bboltcachestorage.Store) error {
		return db.WalkBacklinks(id, func(id string, link solver.CacheInfoLink) error { rows = append(rows, backlink{id, link}); return nil })
	}); err != nil {
		return err
	}
	for _, row := range rows {
		if err := fn(row.id, row.link); err != nil {
			return err
		}
	}
	return nil
}

// WalkLinksAll preserves the upstream optional outgoing-link enumeration used
// when exporting a complete graph, including multiple links to the same key.
func (s *graphCacheKeyStorage) WalkLinksAll(id string, fn func(string, solver.CacheInfoLink) error) error {
	type outgoing struct {
		id   string
		link solver.CacheInfoLink
	}
	var rows []outgoing
	if err := s.withStore(func(db *bboltcachestorage.Store) error {
		return db.WalkLinksAll(id, func(id string, link solver.CacheInfoLink) error {
			rows = append(rows, outgoing{id, link})
			return nil
		})
	}); err != nil {
		return err
	}
	for _, row := range rows {
		if err := fn(row.id, row.link); err != nil {
			return err
		}
	}
	return nil
}
