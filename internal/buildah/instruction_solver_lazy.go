package buildah

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"
	"github.com/moby/buildkit/solver"
	"github.com/strio-dev/coopr/api"
	"github.com/strio-dev/coopr/internal/cache"
	"oras.land/oras-go/v2/content"
)

// Imported associations are scoped to this invocation until their image is
// materialized. Publishing unresolved IDs in the native database would let an
// independent process prune them, because it does not own these staged layouts.
type instructionGraphKeyOverlay struct {
	*graphCacheKeyStorage
	lazy *instructionLazyResults
}

func (s *instructionGraphKeyOverlay) WalkResults(id string, fn func(solver.CacheResult) error) error {
	records := map[string]solver.CacheResult{}
	if err := s.graphCacheKeyStorage.WalkResults(id, func(r solver.CacheResult) error {
		records[r.ID] = r
		return nil
	}); err != nil {
		return err
	}
	for _, pending := range s.lazy.records(id) {
		if previous, exists := records[pending.ID]; !exists || previous.CreatedAt.Before(pending.CreatedAt) {
			records[pending.ID] = pending
		}
	}
	ids := make([]string, 0, len(records))
	for id := range records {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if err := fn(records[id]); err != nil {
			return err
		}
	}
	return nil
}

func (s *instructionGraphKeyOverlay) Load(id, resultID string) (solver.CacheResult, error) {
	result, err := s.graphCacheKeyStorage.Load(id, resultID)
	if err != nil && !errors.Is(err, solver.ErrNotFound) {
		return result, err
	}
	for _, pending := range s.lazy.records(id) {
		if pending.ID == resultID && (err != nil || result.CreatedAt.Before(pending.CreatedAt)) {
			return pending, nil
		}
	}
	return result, err
}

func (s *instructionGraphKeyOverlay) AddResult(id string, result solver.CacheResult) (err error) {
	lock, err := s.lazy.storage.lockResult(s.lazy.storage.ctx, result.ID)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if native, err := s.lazy.storage.read(result.ID); err == nil && result.CreatedAt.Before(native.CreatedAt) {
		result.CreatedAt = native.CreatedAt
	}
	if previous, err := s.graphCacheKeyStorage.Load(id, result.ID); err == nil && result.CreatedAt.Before(previous.CreatedAt) {
		result.CreatedAt = previous.CreatedAt
	}
	return s.graphCacheKeyStorage.AddResult(id, result)
}

type instructionLazyResults struct {
	mu       sync.RWMutex
	results  map[string]*instructionPortableResult
	bindings map[string]map[string]solver.CacheResult
	keys     *graphCacheKeyStorage
	storage  *instructionResultStorage
}

type instructionPortableResult struct {
	value       instructionSolverResult
	record      cache.GraphResult
	layout      string
	source      *cache.GraphSource
	locations   *instructionPortableLocations
	owner       *instructionGraphPortable
	gate        chan struct{}
	failed      atomic.Bool
	importImage func(context.Context) (string, error)
}

// Equivalent immutable results may be available from several approved mirrors.
// Share locations across timestamp refreshes without changing the selected
// output, its restoration metadata or any source's admission policy.
type instructionPortableLocations struct {
	mu        sync.RWMutex
	locations []instructionPortableLocation
}

type instructionPortableLocation struct {
	source *cache.GraphSource
	layout string
}

func (pending *instructionPortableResult) ensureLayout(ctx context.Context) (string, *cache.GraphSource, error) {
	if pending.locations != nil {
		pending.locations.mu.RLock()
		locations := slices.Clone(pending.locations.locations)
		pending.locations.mu.RUnlock()
		var failures error
		for index, location := range locations {
			if err := ctx.Err(); err != nil {
				return "", nil, err
			}
			path, err := location.ensureLayout(ctx, pending.record.ID)
			if err == nil {
				return path, location.source, nil
			}
			if ctx.Err() != nil {
				return "", nil, ctx.Err()
			}
			failure := fmt.Errorf("approved graph source %d for result %s: %w", index+1, pending.record.ID, err)
			failures = errors.Join(failures, failure)
			packageCacheWarning("restore equivalent graph source", failure)
		}
		return "", nil, fmt.Errorf("no available approved source for graph result %s: %w", pending.record.ID, failures)
	}
	path, err := (instructionPortableLocation{source: pending.source, layout: pending.layout}).ensureLayout(ctx, pending.record.ID)
	return path, pending.source, err
}

func (location instructionPortableLocation) ensureLayout(ctx context.Context, result string) (string, error) {
	if location.source != nil {
		return location.source.EnsureResult(ctx, result)
	}
	if location.layout == "" {
		return "", errors.New("portable result has no owned source")
	}
	return location.layout, nil
}

func equivalentInstructionPortableResult(left, right *instructionPortableResult) bool {
	first, second := left.value, right.value
	first.CreatedAt, second.CreatedAt = time.Time{}, time.Time{}
	if left.record.ID != right.record.ID || !content.Equal(left.record.Image, right.record.Image) || !reflect.DeepEqual(first, second) {
		return false
	}
	if left.record.BaseImage == nil || right.record.BaseImage == nil {
		return left.record.BaseImage == nil && right.record.BaseImage == nil
	}
	return content.Equal(*left.record.BaseImage, *right.record.BaseImage)
}

func newInstructionLazyResults(keys *graphCacheKeyStorage) *instructionLazyResults {
	return &instructionLazyResults{results: map[string]*instructionPortableResult{}, bindings: map[string]map[string]solver.CacheResult{}, keys: keys}
}

func (s *instructionLazyResults) get(id string) *instructionPortableResult {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.results[id]
}

func (s *instructionLazyResults) records(key string) []solver.CacheResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var results []solver.CacheResult
	for _, result := range s.bindings[key] {
		results = append(results, result)
	}
	return results
}

func (s *instructionLazyResults) add(results []*instructionPortableResult, nodes []instructionGraphNode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Reject conflicting claims before mutating the invocation overlay. A
	// matching result ID alone is not permission to substitute another view.
	for _, result := range results {
		if previous := s.results[result.value.ID()]; previous != nil && previous.locations != nil && (result.source != nil || result.layout != "") && !equivalentInstructionPortableResult(previous, result) {
			return errors.New("same graph result ID has conflicting immutable source metadata")
		}
	}
	for _, result := range results {
		id := result.value.ID()
		previous := s.results[id]
		addedLocation := false
		if result.source != nil || result.layout != "" {
			if previous != nil {
				result.locations = previous.locations
			}
			if result.locations == nil {
				result.locations = &instructionPortableLocations{}
			}
			location := instructionPortableLocation{source: result.source, layout: result.layout}
			result.locations.mu.Lock()
			if !slices.Contains(result.locations.locations, location) {
				result.locations.locations = append(result.locations.locations, location)
				addedLocation = true
			}
			result.locations.mu.Unlock()
		}
		if previous != nil && addedLocation && !previous.value.CreatedAt.Before(result.value.CreatedAt) {
			// A newly available mirror gets a fresh attempt even when an older
			// pending handle failed. Its payload cannot downgrade the newest
			// admitted restoration metadata or timestamp.
			result.value, result.record = previous.value, previous.record
		}
		if previous == nil || addedLocation || previous.value.CreatedAt.Before(result.value.CreatedAt) {
			s.results[id] = result
		}
	}
	for _, node := range nodes {
		for _, result := range node.Results {
			if s.bindings[node.ID] == nil {
				s.bindings[node.ID] = map[string]solver.CacheResult{}
			}
			previous, exists := s.bindings[node.ID][result.ID]
			if !exists || previous.CreatedAt.Before(result.CreatedAt) {
				s.bindings[node.ID][result.ID] = result
			}
		}
	}
	return nil
}

func (s *instructionLazyResults) persist(id string, created time.Time) error {
	s.mu.RLock()
	bindings := map[string]solver.CacheResult{}
	for node, results := range s.bindings {
		if result, ok := results[id]; ok {
			if result.CreatedAt.Before(created) {
				result.CreatedAt = created
			}
			bindings[node] = result
		}
	}
	s.mu.RUnlock()
	for node, result := range bindings {
		if previous, err := s.keys.Load(node, id); err == nil && result.CreatedAt.Before(previous.CreatedAt) {
			result.CreatedAt = previous.CreatedAt
		}
		if err := s.keys.AddResult(node, result); err != nil {
			return err
		}
	}
	return nil
}

func (s *instructionResultStorage) describe(id string) (*instructionSolverResult, error) {
	result, err := s.read(id)
	if pending := s.lazy.get(id); pending != nil && !pending.failed.Load() && (err != nil || result.CreatedAt.Before(pending.value.CreatedAt)) {
		value := pending.value
		return &value, nil
	}
	return result, err
}

func (s *instructionResultStorage) loadPortable(ctx context.Context, id string) (result solver.Result, err error) {
	pending := s.lazy.get(id)
	if pending == nil || pending.failed.Load() {
		return nil, solver.ErrNotFound
	}
	select {
	case <-pending.gate:
		defer func() { pending.gate <- struct{}{} }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// The metadata directory is shared by independent daemonless workers.
	// Recheck native availability after its per-result lock: another worker may
	// have materialized the same image while this request was waiting.
	lock, err := s.lockResult(ctx, id)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if pending.failed.Load() {
		if _, err := s.read(id); err != nil {
			return nil, solver.ErrNotFound
		}
	}
	imported := false
	materialized, err := s.materializePortableLocked(ctx, &pending.value, func(ctx context.Context) (string, error) {
		imported = true
		return pending.importImage(ctx)
	})
	if err == nil {
		err = s.lazy.persist(id, materialized.CreatedAt)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Reading or refreshing an already materialized result did not attempt
		// a portable import; preserve its existing storage-error behavior.
		if !imported {
			return nil, err
		}
		if pending.failed.CompareAndSwap(false, true) {
			pending.owner.mu.Lock()
			pending.owner.cache.stats.Errors++
			pending.owner.mu.Unlock()
			packageCacheWarning("restore selected graph result", err)
		}
		return nil, err
	}
	return s.read(id)
}

// materializePortable serializes independent eager and lazy workers before
// checking native availability or importing the same immutable result. It never
// publishes key associations; eager callers retain their all-results barrier.
func (s *instructionResultStorage) materializePortable(ctx context.Context, value *instructionSolverResult, importImage func(context.Context) (string, error)) (result *instructionSolverResult, err error) {
	lock, err := s.lockResult(ctx, value.ID())
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	return s.materializePortableLocked(ctx, value, importImage)
}

// materializePortableLocked requires the per-result lock. Lazy callers keep
// that lock through association persistence; eager callers publish only after
// every result has passed this same import/verification protocol.
func (s *instructionResultStorage) materializePortableLocked(ctx context.Context, value *instructionSolverResult, importImage func(context.Context) (string, error)) (*instructionSolverResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := value.ID()
	if result, err := s.read(id); err == nil {
		if !result.CreatedAt.Before(value.CreatedAt) {
			return result, nil
		}
		if _, err := s.writeResult(result, value.CreatedAt); err != nil {
			return nil, err
		}
		return s.read(id)
	}
	// BuildKit supplies its own load context. Keep its cancellation while
	// restoring this invocation's observer when that context omits caller data.
	importCtx := ctx
	if api.ProgressObserver(importCtx) == nil {
		importCtx = withOperationProgress(importCtx, api.ProgressObserver(s.ctx))
	}
	importCtx, complete := ObserveOperationPhase(importCtx, OperationProgress{Phase: "restore", Instruction: "cache"})
	image, err := importImage(importCtx)
	complete("portable", err)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := *value
	result.Entry.ImageID = image
	if _, err := s.writeResult(&result, result.CreatedAt); err != nil {
		return nil, err
	}
	return s.read(id)
}

func (s *instructionResultStorage) lockResult(ctx context.Context, id string) (*flock.Flock, error) {
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	if err := graphCacheRegularFile(path + ".import.lock"); err != nil {
		return nil, err
	}
	lock := flock.New(path + ".import.lock")
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.New("graph result lock not acquired")
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	return lock, nil
}
