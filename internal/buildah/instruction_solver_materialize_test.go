package buildah

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/buildkit/solver"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/cache"
)

func eagerInstructionFixture(t *testing.T, ctx context.Context, root string, created time.Time, physical ...string) (*instructionGraphPortable, instructionGraphMetadata, *cache.GraphRecord, []*instructionSolverResult) {
	t.Helper()
	engine, err := newInstructionSolver(ctx, solverTestStore{root: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.close(); err != nil {
			t.Error(err)
		}
	})
	source := solver.NewCacheKey(digest.FromString("eager source"), "", 0).ID
	metadata := instructionGraphMetadata{Roots: []string{"eager node"}, Nodes: []instructionGraphNode{
		{ID: source},
		{ID: "eager node", Inputs: []instructionGraphInput{{Source: source, Link: solver.CacheInfoLink{Digest: digest.FromString("eager operation")}}}},
	}}
	record := &cache.GraphRecord{}
	values := make([]*instructionSolverResult, len(physical))
	for i, image := range physical {
		value := &instructionSolverResult{Entry: instructionCacheEntry{ManifestDigest: digest.FromString(image)}, CreatedAt: created}
		values[i] = value
		record.Results = append(record.Results, cache.GraphResult{ID: value.ID(), Image: v1.Descriptor{Digest: value.Entry.ManifestDigest}})
		metadata.Nodes[1].Results = append(metadata.Nodes[1].Results, solver.CacheResult{ID: value.ID(), CreatedAt: created})
	}
	return &instructionGraphPortable{engine: engine, cache: &portableInstructionCache{}}, metadata, record, values
}

func assertNoEagerAssociations(t *testing.T, p *instructionGraphPortable, metadata instructionGraphMetadata) {
	t.Helper()
	for _, node := range metadata.Nodes {
		count := 0
		if err := p.engine.keys.graphCacheKeyStorage.WalkResults(node.ID, func(solver.CacheResult) error { count++; return nil }); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("uncompleted eager restoration published %d results for %q", count, node.ID)
		}
		for _, input := range node.Inputs {
			if p.engine.keys.HasLink(input.Source, input.Link, node.ID) {
				t.Fatal("uncompleted eager restoration published a dependency link")
			}
		}
	}
}

func TestInstructionEagerIndependentEnginesShareMaterialization(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	created := time.Now().UTC()
	first, firstMetadata, firstRecord, firstValues := eagerInstructionFixture(t, ctx, root, created, "eager shared")
	second, secondMetadata, secondRecord, secondValues := eagerInstructionFixture(t, ctx, root, created.Add(time.Hour), "eager shared")
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var imports atomic.Int32
	load := func(ctx context.Context, _ cache.GraphResult) (string, error) {
		if imports.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return "eager shared", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	a, b := make(chan error, 1), make(chan error, 1)
	go func() { a <- first.restoreEagerImages(ctx, firstMetadata, firstRecord, firstValues, load) }()
	<-started
	go func() { b <- second.restoreEagerImages(ctx, secondMetadata, secondRecord, secondValues, load) }()
	releaseOnce.Do(func() { close(release) })
	if err := <-a; err != nil {
		t.Fatal(err)
	}
	if err := <-b; err != nil {
		t.Fatal(err)
	}
	if imports.Load() != 1 {
		t.Fatalf("independent eager engines imported the same result %d times", imports.Load())
	}
	saved, err := second.engine.results.read(firstRecord.Results[0].ID)
	if err != nil || !saved.CreatedAt.Equal(created.Add(time.Hour)) {
		t.Fatalf("shared materialization lost latest freshness: %+v %v", saved, err)
	}
	row, err := second.engine.keys.Load("eager node", saved.ID())
	if err != nil || !row.CreatedAt.Equal(saved.CreatedAt) {
		t.Fatalf("eager association differs from materialized freshness: %+v %v", row, err)
	}
}

func TestInstructionEagerAndLazyShareMaterialization(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	created := time.Now().UTC()
	eager, metadata, record, values := eagerInstructionFixture(t, ctx, root, created, "mixed shared")
	lazy, _, _, _ := eagerInstructionFixture(t, ctx, root, created, "mixed shared")
	started, secondImport, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var imports atomic.Int32
	load := func(ctx context.Context) (string, error) {
		switch imports.Add(1) {
		case 1:
			close(started)
		case 2:
			close(secondImport)
		}
		select {
		case <-release:
			return "mixed shared", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	pending := addLazyInstructionFixture(t, lazy.engine, "lazy node", "mixed shared", created.Add(time.Hour), load)
	a, b := make(chan error, 1), make(chan error, 1)
	go func() {
		a <- eager.restoreEagerImages(ctx, metadata, record, values, func(ctx context.Context, _ cache.GraphResult) (string, error) { return load(ctx) })
	}()
	<-started
	go func() { _, err := lazy.engine.results.Load(ctx, solver.CacheResult{ID: pending.value.ID()}); b <- err }()
	// While eager import is held, the other engine must wait for its result
	// lock rather than enter a second import. Give a broken path time to enter.
	select {
	case <-secondImport:
	case <-time.After(100 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-a; err != nil {
		t.Fatal(err)
	}
	if err := <-b; err != nil {
		t.Fatal(err)
	}
	if imports.Load() != 1 {
		t.Fatalf("eager/lazy engines imported the same result %d times", imports.Load())
	}
}

func TestInstructionEagerRestorationRetainsNewerAssociations(t *testing.T) {
	created := time.Now().UTC()
	p, metadata, record, values := eagerInstructionFixture(t, t.Context(), t.TempDir(), created.Add(-time.Hour), "eager fresh")
	value := *values[0]
	value.Entry.ImageID = "eager fresh"
	fresh, err := p.engine.results.Save(&value, created)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.engine.keys.AddResult("eager node", fresh); err != nil {
		t.Fatal(err)
	}
	if err := p.restoreEagerImages(t.Context(), metadata, record, values, func(context.Context, cache.GraphResult) (string, error) { return "eager fresh", nil }); err != nil {
		t.Fatal(err)
	}
	saved, err := p.engine.results.read(fresh.ID)
	if err != nil || !saved.CreatedAt.Equal(created) {
		t.Fatalf("older eager metadata downgraded native freshness: %+v %v", saved, err)
	}
	row, err := p.engine.keys.Load("eager node", fresh.ID)
	if err != nil || !row.CreatedAt.Equal(created) {
		t.Fatalf("older eager metadata downgraded association freshness: %+v %v", row, err)
	}
}

func TestInstructionEagerFailureDoesNotPublishPartialGraph(t *testing.T) {
	p, metadata, record, values := eagerInstructionFixture(t, t.Context(), t.TempDir(), time.Now().UTC(), "eager first", "eager failure")
	failure := errors.New("second image failed verification")
	var imports int
	err := p.restoreEagerImages(t.Context(), metadata, record, values, func(context.Context, cache.GraphResult) (string, error) {
		imports++
		if imports == 2 {
			return "", failure
		}
		return "eager first", nil
	})
	if !errors.Is(err, failure) || imports != 2 {
		t.Fatalf("did not reach second-image failure: imports=%d err=%v", imports, err)
	}
	assertNoEagerAssociations(t, p, metadata)
	if _, err := p.engine.results.read(record.Results[0].ID); err != nil {
		t.Fatalf("already verified immutable first result became unavailable: %v", err)
	}
}

func TestInstructionEagerCanceledLockWaiterDoesNotImportOrPublish(t *testing.T) {
	p, metadata, record, values := eagerInstructionFixture(t, t.Context(), t.TempDir(), time.Now().UTC(), "eager canceled")
	lock, err := p.engine.results.lockResult(t.Context(), record.Results[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	var imports int
	err = p.restoreEagerImages(ctx, metadata, record, values, func(context.Context, cache.GraphResult) (string, error) {
		imports++
		<-ctx.Done()
		return "", ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) || imports != 0 {
		t.Fatalf("canceled eager lock waiter imported or lost cancellation: imports=%d err=%v", imports, err)
	}
	assertNoEagerAssociations(t, p, metadata)
}
