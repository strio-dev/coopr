package buildah

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/bboltcachestorage"
	digest "github.com/opencontainers/go-digest"
	"go.podman.io/storage/pkg/reexec"
)

func TestGraphCacheStoragePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata", "keys.db")
	s, err := newGraphCacheKeyStorage(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	first := solver.CacheResult{ID: "first", CreatedAt: time.Unix(123, 0).UTC()}
	second := solver.CacheResult{ID: "second", CreatedAt: time.Unix(124, 0).UTC()}
	for _, id := range []string{"source", "other"} {
		if err := s.AddResult(id, first); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddResult("source", second); err != nil {
		t.Fatal(err)
	}
	link := solver.CacheInfoLink{Input: 1, Output: 2, Digest: digest.FromString("operation"), Selector: digest.FromString("files")}
	for _, target := range []string{"left", "right"} {
		if err := s.AddLink("source", link, target); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddLink("other", link, "left"); err != nil {
		t.Fatal(err)
	}
	reopened, err := newGraphCacheKeyStorage(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Exists("source") || !reopened.HasLink("source", link, "right") {
		t.Fatal("reopened database lost key/link")
	}
	loaded, err := reopened.Load("source", "first")
	if err != nil || !reflect.DeepEqual(loaded, first) {
		t.Fatalf("Load: %+v, %v", loaded, err)
	}
	var results []string
	if err := reopened.WalkResults("source", func(row solver.CacheResult) error { results = append(results, row.ID); return nil }); err != nil {
		t.Fatal(err)
	}
	graphCacheAssertIDs(t, results, []string{"first", "second"})
	var ids []string
	if err := reopened.WalkIDsByResult("first", func(id string) error { ids = append(ids, id); return nil }); err != nil {
		t.Fatal(err)
	}
	graphCacheAssertIDs(t, ids, []string{"source", "other"})
	var targets []string
	if err := reopened.WalkLinks("source", link, func(id string) error { targets = append(targets, id); return nil }); err != nil {
		t.Fatal(err)
	}
	graphCacheAssertIDs(t, targets, []string{"left", "right"})
	var allTargets []string
	if err := reopened.WalkLinksAll("source", func(id string, _ solver.CacheInfoLink) error {
		allTargets = append(allTargets, id)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	graphCacheAssertIDs(t, allTargets, []string{"left", "right"})
	var backlinks []string
	if err := reopened.WalkBacklinks("left", func(id string, _ solver.CacheInfoLink) error { backlinks = append(backlinks, id); return nil }); err != nil {
		t.Fatal(err)
	}
	graphCacheAssertIDs(t, backlinks, []string{"source", "other"})
	if err := reopened.Release("first"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("source", "first"); !errors.Is(err, solver.ErrNotFound) {
		t.Fatalf("released result: %v", err)
	}
	if _, err := s.Load("source", "second"); err != nil {
		t.Fatalf("release removed alternative: %v", err)
	}
	if s.Err() != nil {
		t.Fatalf("cache miss poisoned storage: %v", s.Err())
	}
	// No writable DB handle survives an operation, including on a second instance.
	direct, err := bboltcachestorage.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := direct.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGraphCacheStorageCallbacksReenter(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	s, err := newGraphCacheKeyStorage(ctx, filepath.Join(t.TempDir(), "metadata", "keys.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddResult("source", solver.CacheResult{ID: "result"}); err != nil {
		t.Fatal(err)
	}
	link := solver.CacheInfoLink{Digest: digest.FromString("edge")}
	if err := s.AddLink("source", link, "target"); err != nil {
		t.Fatal(err)
	}
	reenter := func(string) error {
		if !s.Exists("source") {
			return fmt.Errorf("reentrant Exists: %w", s.Err())
		}
		return nil
	}
	walks := []func() error{
		func() error { return s.Walk(reenter) },
		func() error { return s.WalkResults("source", func(solver.CacheResult) error { return reenter("") }) },
		func() error { return s.WalkIDsByResult("result", reenter) },
		func() error { return s.WalkLinks("source", link, reenter) },
		func() error {
			return s.WalkLinksAll("source", func(string, solver.CacheInfoLink) error { return reenter("") })
		},
		func() error {
			return s.WalkBacklinks("target", func(string, solver.CacheInfoLink) error { return reenter("") })
		},
	}
	for i, walk := range walks {
		if err := walk(); err != nil {
			t.Fatalf("walk %d reentry: %v", i, err)
		}
	}
	// Match the solver manager's cleanup nesting: Walk -> WalkResults -> Release.
	if err := s.Walk(func(id string) error {
		return s.WalkResults(id, func(row solver.CacheResult) error { return s.Release(row.ID) })
	}); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("callback stopped")
	if err := s.WalkLinks("source", link, func(string) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("callback error lost: %v", err)
	}
}

func TestGraphCacheStorageCanceledLock(t *testing.T) {
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &graphCacheWaitingContext{Context: base, waiting: make(chan struct{})}
	path := filepath.Join(t.TempDir(), "metadata", "keys.db")
	s, err := newGraphCacheKeyStorage(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	holder := flock.New(path + ".lock")
	if err := holder.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := holder.Close(); err != nil {
			t.Error(err)
		}
	}()
	finished := make(chan error, 1)
	go func() { finished <- s.AddResult("blocked", solver.CacheResult{ID: "result"}) }()
	// Done is first queried by flock after a failed nonblocking acquisition.
	select {
	case <-ctx.waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not contend on the held lock")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting operation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled lock wait did not finish")
	}
	if s.Exists("blocked") {
		t.Fatal("canceled bool operation succeeded")
	}
	if !errors.Is(s.Err(), context.Canceled) {
		t.Fatalf("bool failure not observable: %v", s.Err())
	}
}

func TestGraphCacheStorageReleaseRechecksAvailability(t *testing.T) {
	s, err := newGraphCacheKeyStorage(t.Context(), filepath.Join(t.TempDir(), "metadata", "keys.db"))
	if err != nil {
		t.Fatal(err)
	}
	var available atomic.Bool
	s.keepResult = func(string) bool { return available.Load() }
	result := solver.CacheResult{ID: "refreshable"}
	if err := s.AddResult("source", result); err != nil {
		t.Fatal(err)
	}
	// A manager's stale observation requests cleanup, while a second process
	// refreshes and publishes the same ID before the Release transaction.
	if available.Load() {
		t.Fatal("expected initial missing-result observation")
	}
	available.Store(true)
	if err := s.AddResult("source", result); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(result.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("source", result.ID); err != nil {
		t.Fatalf("stale release deleted refreshed result: %v", err)
	}
	available.Store(false)
	if err := s.Release(result.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("source", result.ID); !errors.Is(err, solver.ErrNotFound) {
		t.Fatalf("missing result was not released: %v", err)
	}
}

func TestGraphCacheStorageRejectsUnsafePaths(t *testing.T) {
	for _, name := range []string{"database symlink", "lock symlink", "database directory", "public parent"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "keys.db")
			switch name {
			case "database symlink", "lock symlink":
				target := filepath.Join(root, "target")
				if err := os.WriteFile(target, nil, 0600); err != nil {
					t.Fatal(err)
				}
				dest := path
				if name == "lock symlink" {
					dest += ".lock"
				}
				if err := os.Symlink(target, dest); err != nil {
					t.Fatal(err)
				}
			case "database directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "public parent":
				if err := os.Chmod(root, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := newGraphCacheKeyStorage(t.Context(), path); err == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
}

func TestGraphCacheStorageConcurrentProcesses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "metadata")
	path := filepath.Join(root, "keys.db")
	// Rootless bootstrap can run this binary from a memfd, whose display name
	// is not an executable filesystem path. Use the native same-binary path.
	executable := reexec.Self()
	type child struct {
		cmd    *exec.Cmd
		output strings.Builder
	}
	children := make([]*child, 2)
	for i := range children {
		c := &child{}
		c.cmd = exec.CommandContext(t.Context(), executable, "-test.short", "-test.run=^TestGraphCacheStorageHelperProcess$")
		c.cmd.Env = append(os.Environ(), "COOPR_GRAPH_CACHE_TEST_HELPER="+fmt.Sprint(i), "COOPR_GRAPH_CACHE_TEST_PATH="+path)
		c.cmd.Stdout = &c.output
		c.cmd.Stderr = &c.output
		if err := c.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		children[i] = c
		t.Cleanup(func() {
			if c.cmd.ProcessState == nil {
				_ = c.cmd.Process.Kill()
				_ = c.cmd.Wait()
			}
		})
	}
	// Both processes must finish initialization while the other is idle, proving
	// that no store owns the DB lock across the simulated execution interval.
	deadline := time.Now().Add(10 * time.Second)
	for i := range children {
		for {
			if _, err := os.Stat(filepath.Join(root, fmt.Sprintf("ready-%d", i))); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("child %d did not become ready", i)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "start"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for i, c := range children {
		if err := c.cmd.Wait(); err != nil {
			t.Fatalf("child %d: %v\n%s", i, err, c.output.String())
		}
	}
	s, err := newGraphCacheKeyStorage(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	if err := s.WalkResults("shared", func(row solver.CacheResult) error { ids = append(ids, row.ID); return nil }); err != nil {
		t.Fatal(err)
	}
	var want []string
	for i := range children {
		for j := range 20 {
			want = append(want, fmt.Sprintf("%d-%02d", i, j))
		}
	}
	graphCacheAssertIDs(t, ids, want)
}

func TestGraphCacheStorageHelperProcess(t *testing.T) {
	identity, ok := os.LookupEnv("COOPR_GRAPH_CACHE_TEST_HELPER")
	if !ok {
		t.Skip("subprocess only")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	path := os.Getenv("COOPR_GRAPH_CACHE_TEST_PATH")
	s, err := newGraphCacheKeyStorage(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(path)
	if err := os.WriteFile(filepath.Join(root, "ready-"+identity), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "start")); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	for i := range 20 {
		if err := s.AddResult("shared", solver.CacheResult{ID: fmt.Sprintf("%s-%02d", identity, i)}); err != nil {
			t.Fatal(err)
		}
	}
}

func graphCacheAssertIDs(t *testing.T, got, want []string) {
	t.Helper()
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs %v, want %v", got, want)
	}
}

// Observe the lock retry select without timing-dependent sleeps.
type graphCacheWaitingContext struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (ctx *graphCacheWaitingContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}
