package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moby/buildkit/solver"
	digest "github.com/opencontainers/go-digest"
	"go.podman.io/storage"
)

type solverTestStore struct {
	storage.Store
	root string
}

func (s solverTestStore) GraphRoot() string { return s.root }
func (s solverTestStore) Image(id string) (*storage.Image, error) {
	if id == "missing" {
		return nil, storage.ErrImageUnknown
	}
	return &storage.Image{ID: id}, nil
}
func (s solverTestStore) ImageBigData(id, key string) ([]byte, error) { return []byte(id), nil }
func (s solverTestStore) AddNames(string, []string) error             { return nil }

// Build the terminal edge once, as a worker does. Warm lookup must not execute
// or load intermediate results merely to discover the terminal cache key.
func TestInstructionSolverDeclaredGraphPrunesAncestors(t *testing.T) {
	root := t.TempDir()
	for _, phase := range []string{"cold", "warm", "changed"} {
		t.Run(phase, func(t *testing.T) {
			engine, err := newInstructionSolver(t.Context(), solverTestStore{root: root}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := engine.close(); err != nil {
					t.Error(err)
				}
			}()
			calls := 0
			source := engine.source(digest.FromString("immutable source"), instructionSolverResult{}, false)
			input := "source content"
			if phase == "changed" {
				input = "different source content"
			}
			operation := func(name string) func(context.Context, solver.Builder, []*instructionSolverResult) (*instructionSolverResult, error) {
				return func(_ context.Context, _ solver.Builder, _ []*instructionSolverResult) (*instructionSolverResult, error) {
					calls++
					return &instructionSolverResult{Entry: instructionCacheEntry{ImageID: name, ManifestDigest: digest.FromString(name)}}, nil
				}
			}
			copy := engine.declare(source, nil, digest.FromString(input), "COPY", false, operation("copy"))
			build := engine.declare(copy, nil, digest.FromString("compile"), "RUN", false, operation("build"))
			final := engine.declare(nil, []*instructionVertex{build}, digest.FromString("package"), "final COPY", false, operation("final"))
			if phase == "warm" {
				// Remove intermediate metadata without damaging the terminal image.
				// A terminal hit must neither load these results nor execute them.
				for _, name := range []string{"copy", "build"} {
					id := (&instructionSolverResult{Entry: instructionCacheEntry{ImageID: name, ManifestDigest: digest.FromString(name)}}).ID()
					path, err := engine.results.path(id)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
			}
			result, err := engine.build(t.Context(), final, nil)
			if err != nil {
				t.Fatal(err)
			}
			wantCalls := 3
			if phase == "warm" {
				wantCalls = 0
			}
			if calls != wantCalls || result.Entry.ImageID != "final" {
				t.Fatalf("executions=%d result=%s, want %d/final", calls, result.Entry.ImageID, wantCalls)
			}
		})
	}
}

func TestInstructionSolverAggregateUsesUpstreamSubBuild(t *testing.T) {
	engine, err := newInstructionSolver(t.Context(), solverTestStore{root: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := engine.close(); err != nil {
			t.Error(err)
		}
	}()
	source := engine.source(digest.FromString("source"), instructionSolverResult{}, false)
	outer := engine.declare(source, nil, digest.FromString("stage"), "stage", false, func(ctx context.Context, builder solver.Builder, _ []*instructionSolverResult) (*instructionSolverResult, error) {
		if builder == nil {
			return nil, errors.New("stage lacks upstream subgraph builder")
		}
		child, err := engine.begin(withInstructionBuilder(ctx, builder), source, digest.FromString("operation"), "operation", false)
		if err != nil {
			return nil, err
		}
		defer child.abort()
		if child.request != nil {
			err = child.finish(&instructionSolverResult{Entry: instructionCacheEntry{ImageID: "complete", ManifestDigest: digest.FromString("complete")}})
		}
		return child.result, err
	})
	if _, err := engine.build(t.Context(), outer, nil); err != nil {
		t.Fatal(err)
	}
}

// This test runs the real upstream solver and persistent key database. Physical
// results are deliberately opaque: the graph must retain the original complete
// branch after a forced sibling writes a different COPY result.
func TestInstructionSolverRetainsCompleteGraphAlternatives(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	solve := func(run string, forceCopy, forceRun bool, copyPhysical, runPhysical string) (string, int) {
		t.Helper()
		engine, err := newInstructionSolver(ctx, solverTestStore{root: root}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := engine.close(); err != nil {
				t.Error(err)
			}
		}()
		source := engine.source(digest.FromString("scratch"), instructionSolverResult{}, false)
		calls := 0
		copySolve, err := engine.begin(ctx, source, digest.FromString("copy payload"), "COPY", forceCopy)
		if err != nil {
			t.Fatal(err)
		}
		if copySolve.request != nil {
			calls++
			if err := copySolve.finish(&instructionSolverResult{Entry: instructionCacheEntry{ImageID: copyPhysical, ManifestDigest: digest.FromString(copyPhysical)}}); err != nil {
				t.Fatal(err)
			}
		}
		runSolve, err := engine.begin(ctx, copySolve.vertex, digest.FromString(run), "RUN", forceRun)
		if err != nil {
			t.Fatal(err)
		}
		if runSolve.request != nil {
			calls++
			if err := runSolve.finish(&instructionSolverResult{Entry: instructionCacheEntry{ImageID: runPhysical, ManifestDigest: digest.FromString(runPhysical)}, ParentLayers: 1}); err != nil {
				t.Fatal(err)
			}
		}
		return runSolve.result.Entry.ManifestDigest.String(), calls
	}
	a, calls := solve("run A", false, false, "copy A", "result A")
	if calls != 2 {
		t.Fatalf("cold calls=%d", calls)
	}
	if got, calls := solve("run A", false, false, "unused", "unused"); got != a || calls != 0 {
		t.Fatalf("warm=%s calls=%d, want %s/0", got, calls, a)
	}
	if _, calls := solve("run B", true, true, "copy B", "result B"); calls != 2 {
		t.Fatalf("forced sibling calls=%d", calls)
	}
	if got, calls := solve("run A", false, false, "unused", "unused"); got != a || calls != 0 {
		t.Fatalf("after sibling=%s calls=%d, want complete A %s/0", got, calls, a)
	}
	if _, calls := solve("run A", true, false, "copy C", "result C"); calls != 2 {
		t.Fatalf("forced parent bypassed consumer: calls=%d", calls)
	}
}

func TestInstructionSolverCancellationDrainsExec(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	engine, err := newInstructionSolver(ctx, solverTestStore{root: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := engine.close(); err != nil {
			t.Error(err)
		}
	}()
	source := engine.source(digest.FromString("scratch"), instructionSolverResult{}, false)
	pending, err := engine.begin(ctx, source, digest.FromString("wait"), "RUN", false)
	if err != nil {
		t.Fatal(err)
	}
	if pending.request == nil {
		t.Fatal("cold instruction did not enter Exec")
	}
	start := time.Now()
	pending.abort()
	if time.Since(start) > time.Second {
		t.Fatal("canceled Exec did not drain promptly")
	}
	retry, err := engine.begin(ctx, source, digest.FromString("retry"), "RUN", false)
	if err != nil {
		t.Fatal(err)
	}
	retry.abort()
	// A canceled operation must not have published a result record.
	records := 0
	if err := engine.keys.Walk(func(id string) error {
		return engine.keys.WalkResults(id, func(r solver.CacheResult) error { records++; return nil })
	}); err != nil {
		t.Fatal(err)
	}
	if records != 1 {
		t.Fatalf("canceled instructions published results: count=%d, want only immutable source", records)
	}
}

func TestInstructionResultMetadataRejectsIdentityChanges(t *testing.T) {
	s := instructionResultStorage{ctx: context.Background(), dir: t.TempDir()}
	if _, err := s.path("../../escape"); err == nil {
		t.Fatal("accepted invalid result path")
	}
	path, err := s.path(digest.FromString("safe").String())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != s.dir {
		t.Fatal("result escaped its private directory")
	}
}

func TestInstructionResultSemanticStateIdentityAndValidation(t *testing.T) {
	ctx := t.Context()
	s := instructionResultStorage{ctx: ctx, store: solverTestStore{}, dir: t.TempDir()}
	value := instructionSolverResult{Entry: instructionCacheEntry{
		ImageID: "stage", ManifestDigest: digest.FromString("stage"),
		LogicalConfig: json.RawMessage(`{"config":{"Cmd":["ready"]},"extension":"one"}`),
		CommandSet:    new(true), RetainsCaller: new(true), BaseImageID: digest.FromString("base").Encoded(),
	}}
	record, err := s.Save(&value, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.read(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := instructionStageState(loaded)
	if err != nil || !state.config.CommandSet() || !state.retainsCaller || state.baseImageID != value.Entry.BaseImageID {
		t.Fatalf("semantic state did not restore: state=%+v error=%v", state, err)
	}
	for _, test := range []struct {
		name    string
		invalid bool
		change  func(*instructionCacheEntry)
	}{
		{"logical config", false, func(e *instructionCacheEntry) {
			e.LogicalConfig = json.RawMessage(`{"config":{"Cmd":["ready"]},"extension":"two"}`)
		}},
		{"command authorship", false, func(e *instructionCacheEntry) { e.CommandSet = new(false) }},
		{"caller lineage", false, func(e *instructionCacheEntry) { e.RetainsCaller = new(false) }},
		{"base identity", false, func(e *instructionCacheEntry) { e.BaseImageID = digest.FromString("other base").Encoded() }},
		{"missing config", true, func(e *instructionCacheEntry) { e.LogicalConfig = nil }},
		{"missing command state", true, func(e *instructionCacheEntry) { e.CommandSet = nil }},
		{"missing caller state", true, func(e *instructionCacheEntry) { e.RetainsCaller = nil }},
		{"malformed config", true, func(e *instructionCacheEntry) { e.LogicalConfig = json.RawMessage(`[]`) }},
		{"malformed base", true, func(e *instructionCacheEntry) { e.BaseImageID = "not-an-image" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := value
			test.change(&changed.Entry)
			if changed.ID() == record.ID {
				t.Fatal("changed semantic state retained the original result identity")
			}
			if _, err := instructionStageState(&changed); (err != nil) != test.invalid {
				t.Fatalf("semantic state restore: invalid=%v error=%v", test.invalid, err)
			}
			// Valid edits cannot replace an existing result's semantic state.
			// Malformed state must also fail with its own consistent result ID.
			id := record.ID
			if test.invalid {
				id = changed.ID()
			}
			path, err := s.path(id)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.read(id); err == nil {
				t.Fatal("persisted semantic-state tampering was accepted")
			}
		})
	}
}

func TestInstructionResultMissingImageIsNotAvailable(t *testing.T) {
	ctx := context.Background()
	s := instructionResultStorage{ctx: ctx, store: solverTestStore{}, dir: t.TempDir()}
	r := &instructionSolverResult{Entry: instructionCacheEntry{ImageID: "present", ManifestDigest: digest.FromString("present")}}
	record, err := s.Save(r, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !s.Exists(ctx, record.ID) {
		t.Fatal("persisted native image missing")
	}
	path, _ := s.path(record.ID)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved instructionSolverResult
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	saved.Entry.ImageID = "missing"
	raw, _ = json.Marshal(saved)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if s.Exists(ctx, record.ID) {
		t.Fatal("deleted image is still available")
	}
	if _, err := s.Load(ctx, record); !errors.Is(err, storage.ErrImageUnknown) {
		t.Fatalf("missing image load error=%v", err)
	}
	saved.Entry.ImageID = ""
	raw, _ = json.Marshal(saved)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if s.Exists(ctx, record.ID) {
		t.Fatal("non-source metadata without image accepted")
	}
}

func TestInstructionResultAgeDoesNotRemoveDurableExport(t *testing.T) {
	for _, test := range []struct {
		name string
		ttl  time.Duration
		age  time.Duration
	}{
		{name: "zero", ttl: 0},
		{name: "expired", ttl: time.Minute, age: 2 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			engine, err := newInstructionSolver(ctx, solverTestStore{root: t.TempDir()}, &test.ttl)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := engine.close(); err != nil {
					t.Error(err)
				}
			}()
			result := &instructionSolverResult{Entry: instructionCacheEntry{ImageID: "durable", ManifestDigest: digest.FromString("durable")}}
			saved, err := engine.results.Save(result, time.Now().Add(-test.age))
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.keys.AddResult("key", saved); err != nil {
				t.Fatal(err)
			}
			if engine.results.Exists(ctx, saved.ID) {
				t.Fatal("aged result remained eligible for reuse")
			}
			if _, err := engine.results.read(saved.ID); err != nil {
				t.Fatalf("export lost durable result: %v", err)
			}
			// BuildKit's opportunistic cleanup releases unavailable records. The
			// retention check must distinguish age from deleted native images.
			if err := engine.keys.Release(saved.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.keys.Load("key", saved.ID); err != nil {
				t.Fatalf("age removed graph export association: %v", err)
			}
		})
	}
}

func TestInstructionSolverZeroTTLStillWritesFreshResults(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	zero := time.Duration(0)
	for _, physical := range []string{"first", "second"} {
		engine, err := newInstructionSolver(ctx, solverTestStore{root: root}, &zero)
		if err != nil {
			t.Fatal(err)
		}
		source := engine.source(digest.FromString("scratch"), instructionSolverResult{}, true)
		pending, err := engine.begin(ctx, source, digest.FromString("operation"), "RUN", true)
		if err != nil {
			t.Fatal(err)
		}
		if pending.request == nil {
			t.Fatal("zero TTL reused an earlier execution")
		}
		result := &instructionSolverResult{Entry: instructionCacheEntry{ImageID: physical, ManifestDigest: digest.FromString(physical)}}
		if err := pending.finish(result); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.results.read(result.ID()); err != nil {
			t.Fatalf("fresh result unavailable to export: %v", err)
		}
		if err := engine.close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInstructionGraphUnportableRootFallsBackToNativeExecution(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "payload", "native execution remains available\n")
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	// user.overlay.* is writable in an unprivileged fixture but deliberately
	// excluded from Coopr's portable metadata contract. Ordinary Buildah RUN
	// still accepts it; cache eligibility must not reject the build.
	for _, name := range []string{"cold", "again"} {
		result, layout := f.build(t, name, fmt.Sprintf(`from %q {
    run "/bin/busybox setfattr -n user.overlay.coopr-test -v native-only /" network="none"
    copy "payload" "/proof"
}
`, base.reference))
		if got := localComponentLastFile(t, layout, "proof"); got != "native execution remains available\n" {
			t.Fatalf("native continuation output = %q", got)
		}
		if result.CacheStats.Errors != 0 || result.CacheStats.Stored != 0 {
			t.Fatalf("nonportable root advertised reusable graph results: %+v", result.CacheStats)
		}
		if count := instructionCacheRecordCount(t, f.options.Store); count != 0 {
			t.Fatalf("nonportable root retained %d instruction keys", count)
		}
	}
}

func TestInstructionGraphParentLayerShapeAcrossImageFormats(t *testing.T) {
	for _, format := range []string{"oci", "docker"} {
		t.Run(format, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
			secret := filepath.Join(f.contextDir, "mode")
			build := func(name, mode, consumer string, force bool) string {
				t.Helper()
				f.write(t, "mode", mode)
				plan := testPlan(t, fmt.Sprintf(`from %q {
    run "/bin/busybox mkdir -p /run/secrets && /bin/busybox touch /run/secrets/mode" network="none"
    run "if [ \"$(cat /run/secrets/mode)\" = filled ]; then touch /maybe; fi" network="none" {
        mount "secret" id="mode" required="true"
    }
    run %q network="none"
}
`, base.reference, consumer))
				options := f.options
				options.NoCache = force
				options.Secrets = []string{"id=mode,src=" + secret}
				options.Output = Output{Path: filepath.Join(f.root, name), Format: format}
				if _, err := BuildPlan(f.ctx, plan, options); err != nil {
					t.Fatal(err)
				}
				return options.Output.Path
			}
			const consumer = "od -An -N16 -tx1 /dev/urandom > /proof; if [ -e /maybe ]; then echo filled; else echo empty; fi > /parent-kind"
			cold := build("filled", "filled", consumer, false)
			coldProof := localComponentLastFile(t, cold, "proof")
			if got := localComponentLastFile(t, cold, "parent-kind"); got != "filled\n" {
				t.Fatalf("cold parent = %q", got)
			}
			// The same logical producer now has an empty filesystem diff. Its sibling
			// consumer differs, leaving the old consumer result with a larger parent.
			empty := build("empty-sibling", "empty", "true", true)
			_, coldImage := readPlanImage(t, cold)
			_, emptyImage := readPlanImage(t, empty)
			coldParentLayers := len(coldImage.RootFS.DiffIDs) - 1
			emptyParentLayers := len(emptyImage.RootFS.DiffIDs)
			if format == "docker" {
				emptyParentLayers--
			}
			if (coldParentLayers != emptyParentLayers) != (format == "oci") {
				t.Fatalf("fixture parent shapes cold=%d empty=%d, format=%s", coldParentLayers, emptyParentLayers, format)
			}
			refreshed := build("shape-mismatch", "empty", consumer, false)
			if format == "oci" {
				if got := localComponentLastFile(t, refreshed, "parent-kind"); got != "empty\n" {
					t.Fatalf("shape retry did not execute on current complete parent: %q", got)
				}
				if proof := localComponentLastFile(t, refreshed, "proof"); proof == coldProof {
					t.Fatal("incompatible parent shape reused the old consumer")
				}
			} else {
				// Docker retains empty diffs as physical layers, so both parents
				// have the same shape and selecting the old complete result is valid.
				if got := localComponentLastFile(t, refreshed, "parent-kind"); got != "filled\n" {
					t.Fatalf("same-shape hit did not retain its complete ancestry: %q", got)
				}
				if proof := localComponentLastFile(t, refreshed, "proof"); proof != coldProof {
					t.Fatal("same-shape hit unexpectedly reexecuted")
				}
			}
		})
	}
}

func TestInstructionGraphRestorationKeySeparatesParentShapes(t *testing.T) {
	operation := digest.FromString("operation")
	first := instructionGraphRestorationKey(operation, 1)
	if first != instructionGraphRestorationKey(operation, 1) {
		t.Fatal("same shape changed identity")
	}
	if first == instructionGraphRestorationKey(operation, 2) {
		t.Fatal("different shapes alias")
	}
	if first == instructionGraphRestorationKey(digest.FromString("different operation"), 1) {
		t.Fatal("different operations alias")
	}
}

func mustInstructionPortableDigest(t *testing.T, vertex *instructionVertex) digest.Digest {
	t.Helper()
	value, err := vertex.portableDigest()
	if err != nil {
		t.Fatal(err)
	}
	return value
}
