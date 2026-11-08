package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/buildkit/solver"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/cache"
	"github.com/strio-dev/coopr/internal/oci"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func newLazyInstructionSolver(t *testing.T, ctx context.Context, root string, ttl *time.Duration) *instructionSolver {
	t.Helper()
	engine, err := newInstructionSolver(ctx, solverTestStore{root: root}, ttl)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.close(); err != nil {
			t.Error(err)
		}
	})
	return engine
}

func addLazyInstructionFixture(t *testing.T, engine *instructionSolver, node, physical string, created time.Time, load func(context.Context) (string, error)) *instructionPortableResult {
	t.Helper()
	pending := &instructionPortableResult{
		value: instructionSolverResult{Entry: instructionCacheEntry{ManifestDigest: digest.FromString(physical)}, CreatedAt: created},
		owner: &instructionGraphPortable{cache: &portableInstructionCache{}},
		gate:  make(chan struct{}, 1), importImage: load,
	}
	pending.gate <- struct{}{}
	if err := engine.results.lazy.add([]*instructionPortableResult{pending}, []instructionGraphNode{{ID: node, Results: []solver.CacheResult{{ID: pending.value.ID(), CreatedAt: created}}}}); err != nil {
		t.Fatal(err)
	}
	return pending
}

func TestInstructionLazyResultsStayLocalUntilMaterialized(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	engine := newLazyInstructionSolver(t, ctx, root, nil)
	const node = "imported-operation"
	var imports atomic.Int32
	pending := addLazyInstructionFixture(t, engine, node, "selected", time.Now().UTC(), func(context.Context) (string, error) {
		imports.Add(1)
		return "selected", nil
	})
	if !engine.results.Exists(ctx, pending.value.ID()) || imports.Load() != 0 {
		t.Fatal("metadata admission imported its image")
	}
	count := 0
	if err := engine.keys.graphCacheKeyStorage.WalkResults(node, func(solver.CacheResult) error { count++; return nil }); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("unresolved result leaked into the persistent native graph")
	}
	// An independent cache-manager startup must not delete this invocation's
	// still-unmaterialized result or require its temporary layout.
	other, err := newInstructionSolver(ctx, solverTestStore{root: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.close(); err != nil {
		t.Fatal(err)
	}
	if !engine.results.Exists(ctx, pending.value.ID()) {
		t.Fatal("another manager removed the pending result")
	}
	if _, err := engine.results.Load(ctx, solver.CacheResult{ID: pending.value.ID()}); err != nil {
		t.Fatal(err)
	}
	if imports.Load() != 1 {
		t.Fatalf("imports=%d, want one", imports.Load())
	}
	count = 0
	if err := engine.keys.graphCacheKeyStorage.WalkResults(node, func(solver.CacheResult) error { count++; return nil }); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("materialized result did not gain its durable association")
	}
}

func TestInstructionLazyResultConcurrentLoadsAndCanceledWaiter(t *testing.T) {
	ctx := t.Context()
	engine := newLazyInstructionSolver(t, ctx, t.TempDir(), nil)
	started, release := make(chan struct{}), make(chan struct{})
	var imports atomic.Int32
	pending := addLazyInstructionFixture(t, engine, "node", "shared", time.Now().UTC(), func(ctx context.Context) (string, error) {
		imports.Add(1)
		close(started)
		select {
		case <-release:
			return "shared", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	record := solver.CacheResult{ID: pending.value.ID()}
	first := make(chan error, 1)
	go func() { _, err := engine.results.Load(ctx, record); first <- err }()
	<-started
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := engine.results.Load(canceled, record); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting cancellation=%v", err)
	}
	var wait sync.WaitGroup
	errorsCh := make(chan error, 8)
	for range 8 {
		wait.Go(func() { _, err := engine.results.Load(ctx, record); errorsCh <- err })
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if imports.Load() != 1 {
		t.Fatalf("concurrent requests imported %d times", imports.Load())
	}
}

func TestInstructionLazyLoadFailureFallsBackThroughBuildKit(t *testing.T) {
	for _, validAlternative := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "older alternative"}[validAlternative], func(t *testing.T) {
			ctx := t.Context()
			engine := newLazyInstructionSolver(t, ctx, t.TempDir(), nil)
			sourceKey, operation := digest.FromString("scratch"), digest.FromString("operation")
			const node = "portable-operation"
			if err := engine.keys.AddLink(solver.NewCacheKey(sourceKey, "", 0).ID, solver.CacheInfoLink{Digest: operation}, node); err != nil {
				t.Fatal(err)
			}
			var failed, imported atomic.Int32
			bad := addLazyInstructionFixture(t, engine, node, "corrupt selected", time.Now().UTC(), func(context.Context) (string, error) {
				failed.Add(1)
				return "", errors.New("selected image digest differs")
			})
			if validAlternative {
				addLazyInstructionFixture(t, engine, node, "valid old", time.Now().Add(-time.Hour).UTC(), func(context.Context) (string, error) { imported.Add(1); return "valid old", nil })
			}
			pending, err := engine.begin(ctx, engine.source(sourceKey, instructionSolverResult{}, false), operation, "RUN", false)
			if err != nil {
				t.Fatal(err)
			}
			if failed.Load() != 1 || !bad.failed.Load() || bad.owner.cache.stats.Errors != 1 {
				t.Fatal("failed selected import was not isolated and reported once")
			}
			if validAlternative {
				if pending.request != nil || imported.Load() != 1 || pending.result.Entry.ImageID != "valid old" {
					t.Fatal("BuildKit did not try the valid historical result")
				}
			} else {
				if pending.request == nil {
					t.Fatal("all failed cache results did not fall back to execution")
				}
				if err := pending.finish(&instructionSolverResult{Entry: instructionCacheEntry{ImageID: "executed", ManifestDigest: digest.FromString("executed")}}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestInstructionLazyLoadDoesNotRecheckAdmissionTTL(t *testing.T) {
	ctx := t.Context()
	ttl := time.Minute
	engine := newLazyInstructionSolver(t, ctx, t.TempDir(), &ttl)
	pending := addLazyInstructionFixture(t, engine, "node", "admitted", time.Now().Add(-time.Hour).UTC(), func(context.Context) (string, error) { return "admitted", nil })
	if engine.results.Exists(ctx, pending.value.ID()) {
		t.Fatal("expired result was eligible for a new match")
	}
	if _, err := engine.results.Load(ctx, solver.CacheResult{ID: pending.value.ID()}); err != nil {
		t.Fatalf("previously admitted load expired during materialization: %v", err)
	}
}

func TestInstructionGraphReadStagingDeduplicatesAndCloses(t *testing.T) {
	ctx := t.Context()
	sourcePath := t.TempDir()
	source, err := orasoci.New(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	configData := []byte(`{"rootfs":{"type":"layers","diff_ids":[]}}`)
	config := oci.Descriptor(v1.MediaTypeImageConfig, configData)
	manifestData, err := json.Marshal(oci.VersionedManifest(config, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	image := oci.Descriptor(v1.MediaTypeImageManifest, manifestData)
	if err := source.Push(ctx, config, bytes.NewReader(configData)); err != nil {
		t.Fatal(err)
	}
	if err := source.Push(ctx, image, bytes.NewReader(manifestData)); err != nil {
		t.Fatal(err)
	}
	p := &instructionGraphPortable{cache: &portableInstructionCache{stagingDir: t.TempDir()}}
	metadata, results := instructionGraphFixture(t)
	for i := range results {
		results[i].Image = image
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	record := cache.GraphRecord{Version: cache.GraphRecordVersion, Root: digest.FromString("shared-prefix"), CreatedAt: time.Now().UTC(), Results: results, Metadata: raw}
	store, err := cache.NewLocalStore(ctx, t.TempDir(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutGraph(ctx, record.Root, record, sourcePath); err != nil {
		t.Fatal(err)
	}
	var path string
	reader, err := p.graphReader()
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		_, got, err := reader.Lookup(ctx, store, record.Root)
		if err != nil {
			t.Fatal(err)
		}
		if path != "" && path != got {
			t.Fatal("graph prefixes gained separate read layouts")
		}
		path = got
	}
	blobs, err := os.ReadDir(filepath.Join(path, "blobs", "sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 2 {
		t.Fatalf("retained %d blobs for one shared image, want config+manifest only", len(blobs))
	}
	engine := &instructionSolver{portable: p}
	if err := engine.closePortable(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed solver retained its private read layout: %v", err)
	}
	if _, err := os.Stat(sourcePath); err != nil {
		t.Fatal("private staging cleanup removed caller's source")
	}
}

func TestInstructionLazyProgressMeasuresOnlyMaterialization(t *testing.T) {
	var events []OperationProgress
	ctx := withOperationProgress(t.Context(), func(event OperationProgress) { events = append(events, event) })
	engine := newLazyInstructionSolver(t, ctx, t.TempDir(), nil)
	pending := addLazyInstructionFixture(t, engine, "node", "progress", time.Now().UTC(), func(context.Context) (string, error) { return "progress", nil })
	for range 2 {
		if _, err := engine.results.Load(t.Context(), solver.CacheResult{ID: pending.value.ID()}); err != nil {
			t.Fatal(err)
		}
	}
	if len(events) != 2 || events[0].Phase != "restore" || events[0].State != "started" || events[1].State != "completed" || events[1].Cache != "portable" || events[1].DurationNanos < 0 {
		t.Fatalf("restore progress included native cache reads or lost materialization timing: %+v", events)
	}
}

func TestInstructionLazyFreshMetadataRefreshesExistingNativeResult(t *testing.T) {
	ttl := time.Hour
	engine := newLazyInstructionSolver(t, t.Context(), t.TempDir(), &ttl)
	value := &instructionSolverResult{Entry: instructionCacheEntry{ImageID: "same image", ManifestDigest: digest.FromString("same image")}}
	cached, err := engine.results.Save(value, time.Now().Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if engine.results.Exists(t.Context(), cached.ID) {
		t.Fatal("old native result unexpectedly fresh")
	}
	created := time.Now().UTC()
	var imports atomic.Int32
	addLazyInstructionFixture(t, engine, "node", "same image", created, func(context.Context) (string, error) { imports.Add(1); return "same image", nil })
	if !engine.results.Exists(t.Context(), cached.ID) {
		t.Fatal("expired native metadata hid a fresh portable result")
	}
	if _, err := engine.results.Load(t.Context(), cached); err != nil {
		t.Fatal(err)
	}
	saved, err := engine.results.read(cached.ID)
	if err != nil || !saved.CreatedAt.Equal(created) || imports.Load() != 0 {
		t.Fatalf("existing image was reimported or lost refreshed time: %+v imports=%d err=%v", saved, imports.Load(), err)
	}
}

func TestInstructionLazyIndependentEnginesShareMaterialization(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	first, err := newInstructionSolver(ctx, solverTestStore{root: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newInstructionSolver(ctx, solverTestStore{root: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := first.close(); err != nil {
			t.Error(err)
		}
		if err := second.close(); err != nil {
			t.Error(err)
		}
	})
	started, release := make(chan struct{}), make(chan struct{})
	var imports atomic.Int32
	load := func(ctx context.Context) (string, error) {
		if imports.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return "shared engines", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	fresh := time.Now().UTC()
	pending := addLazyInstructionFixture(t, first, "shared node", "shared engines", fresh.Add(-2*time.Hour), load)
	addLazyInstructionFixture(t, second, "shared node", "shared engines", fresh, load)
	record := solver.CacheResult{ID: pending.value.ID()}
	a, b := make(chan error, 1), make(chan error, 1)
	go func() { _, err := first.results.Load(ctx, record); a <- err }()
	<-started
	go func() { _, err := second.results.Load(ctx, record); b <- err }()
	close(release)
	if err := <-a; err != nil {
		t.Fatal(err)
	}
	if err := <-b; err != nil {
		t.Fatal(err)
	}
	saved, err := second.results.read(record.ID)
	if err != nil || !saved.CreatedAt.Equal(fresh) {
		t.Fatalf("post-lock native result lost fresh portable time: %+v %v", saved, err)
	}
	ttl := time.Hour
	third, err := newInstructionSolver(ctx, solverTestStore{root: root}, &ttl)
	if err != nil {
		t.Fatal(err)
	}
	if !third.results.Exists(ctx, record.ID) {
		t.Fatal("fresh metadata expired after reopening engine")
	}
	if err := third.close(); err != nil {
		t.Fatal(err)
	}
	if imports.Load() != 1 {
		t.Fatalf("independent engines imported same result %d times", imports.Load())
	}
}

func TestInstructionLazyProgressPreservesLoadCancellation(t *testing.T) {
	var events []OperationProgress
	invocation := withOperationProgress(t.Context(), func(event OperationProgress) { events = append(events, event) })
	engine := newLazyInstructionSolver(t, invocation, t.TempDir(), nil)
	loadCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pending := addLazyInstructionFixture(t, engine, "node", "canceled progress", time.Now().UTC(), func(ctx context.Context) (string, error) { cancel(); return "", ctx.Err() })
	if _, err := engine.results.Load(loadCtx, solver.CacheResult{ID: pending.value.ID()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("load lost cancellation: %v", err)
	}
	if len(events) != 2 || events[1].State != "canceled" || pending.failed.Load() || pending.owner.cache.stats.Errors != 0 {
		t.Fatalf("canceled import lost observer or poisoned cache: %+v", events)
	}
}

func TestInstructionLazyRefreshedAssociationWinsOverOtherNativeResult(t *testing.T) {
	ctx := t.Context()
	engine := newLazyInstructionSolver(t, ctx, t.TempDir(), nil)
	sourceKey, operation := digest.FromString("refresh source"), digest.FromString("refresh operation")
	const node = "refreshed operation"
	if err := engine.keys.AddLink(solver.NewCacheKey(sourceKey, "", 0).ID, solver.CacheInfoLink{Digest: operation}, node); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	var refreshedID string
	for i, physical := range []string{"physical A", "physical B"} {
		value := &instructionSolverResult{Entry: instructionCacheEntry{ImageID: physical, ManifestDigest: digest.FromString(physical)}}
		saved, err := engine.results.Save(value, now.Add(time.Duration(i-2)*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.keys.AddResult(node, saved); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			refreshedID = saved.ID
		}
	}
	var imported atomic.Int32
	addLazyInstructionFixture(t, engine, node, "physical A", now, func(context.Context) (string, error) { imported.Add(1); return "physical A", nil })
	var refreshed solver.CacheResult
	if err := engine.keys.WalkResults(node, func(r solver.CacheResult) error {
		if r.ID == refreshedID {
			refreshed = r
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !refreshed.CreatedAt.Equal(now) {
		t.Fatalf("enumerated stale association time %s, want %s", refreshed.CreatedAt, now)
	}
	loaded, err := engine.keys.Load(node, refreshedID)
	if err != nil || !loaded.CreatedAt.Equal(now) {
		t.Fatalf("loaded stale association: %+v %v", loaded, err)
	}
	pending, err := engine.begin(ctx, engine.source(sourceKey, instructionSolverResult{}, false), operation, "RUN", false)
	if err != nil {
		t.Fatal(err)
	}
	if pending.request != nil || pending.result.Entry.ImageID != "physical A" || imported.Load() != 0 {
		t.Fatalf("fresh A did not outrank intermediate native B: %+v imports=%d", pending.result, imported.Load())
	}
}

func TestInstructionLazyUnselectedExportRetainsFreshMetadata(t *testing.T) {
	ctx := t.Context()
	engine := newLazyInstructionSolver(t, ctx, t.TempDir(), nil)
	now := time.Now().UTC()
	value := &instructionSolverResult{Entry: instructionCacheEntry{ImageID: "unselected", ManifestDigest: digest.FromString("unselected")}}
	old := now.Add(-time.Hour)
	if _, err := engine.results.Save(value, old); err != nil {
		t.Fatal(err)
	}
	var imports atomic.Int32
	addLazyInstructionFixture(t, engine, "node", "unselected", now, func(context.Context) (string, error) { imports.Add(1); return "unselected", nil })
	image := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: value.Entry.ManifestDigest, Size: 1}
	exporter := &instructionGraphExport{images: map[digest.Digest]v1.Descriptor{image.Digest: image}, results: map[string]cache.GraphResult{}}
	exported, err := exporter.result(ctx, &instructionGraphPortable{engine: engine}, value.ID())
	if err != nil {
		t.Fatal(err)
	}
	var metadata instructionSolverResult
	if err := json.Unmarshal(exported.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if !metadata.CreatedAt.Equal(now) || imports.Load() != 0 {
		t.Fatalf("unselected export lost refresh or imported image: %+v imports=%d", metadata, imports.Load())
	}
	original, err := engine.results.read(value.ID())
	if err != nil || !original.CreatedAt.Equal(old) {
		t.Fatal("unselected export changed durable native freshness")
	}
}

func TestInstructionLazyResultWritesDoNotRollBackFreshness(t *testing.T) {
	ctx := t.Context()
	engine := newLazyInstructionSolver(t, ctx, t.TempDir(), nil)
	now := time.Now().UTC()
	value := &instructionSolverResult{Entry: instructionCacheEntry{ImageID: "reordered", ManifestDigest: digest.FromString("reordered")}}
	fresh, err := engine.results.Save(value, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.keys.AddResult("node", fresh); err != nil {
		t.Fatal(err)
	}
	stale, err := engine.results.Save(value, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !stale.CreatedAt.Equal(now) {
		t.Fatal("older writer rolled back durable metadata")
	}
	stale.CreatedAt = now.Add(-time.Hour)
	if err := engine.keys.AddResult("node", stale); err != nil {
		t.Fatal(err)
	}
	row, err := engine.keys.Load("node", fresh.ID)
	if err != nil || !row.CreatedAt.Equal(now) {
		t.Fatalf("delayed association writer rolled back selection time: %+v %v", row, err)
	}
}
