package buildah

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/types"
	"golang.org/x/sys/unix"
)

func TestSimpleLocalCopyDigestCandidatesInvalidateSourceMetadata(t *testing.T) {
	contextDir := t.TempDir()
	path := filepath.Join(contextDir, "source")
	if err := os.WriteFile(path, []byte("first\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	fixed := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(path, fixed, fixed); err != nil {
		t.Fatal(err)
	}
	operation := Copy{Sources: []string{"source"}, Destination: "/renamed"}
	digests := func() []digest.Digest {
		t.Helper()
		values, eligible := simpleLocalCopyDigestCandidates(contextDir, nil, operation)
		if !eligible || len(values) != 2 {
			t.Fatalf("simple copy candidates = %v, %v, want two", values, eligible)
		}
		return values
	}
	base := digests()
	if again := digests(); !slices.Equal(base, again) {
		t.Fatalf("stable candidates changed: %v != %v", base, again)
	}

	if err := os.WriteFile(path, []byte("other\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if changed := digests(); slices.Equal(base, changed) {
		t.Fatal("content change did not invalidate candidates")
	} else {
		base = changed
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if changed := digests(); slices.Equal(base, changed) {
		t.Fatal("mode change did not invalidate candidates")
	} else {
		base = changed
	}
	if err := os.Chtimes(path, fixed.Add(time.Second), fixed.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if changed := digests(); slices.Equal(base, changed) {
		t.Fatal("mtime change did not invalidate candidates")
	} else {
		base = changed
	}
	if err := unix.Setxattr(path, "user.coopr", []byte("value"), 0); err != nil {
		t.Skipf("xattrs unavailable: %v", err)
	}
	if changed := digests(); slices.Equal(base, changed) {
		t.Fatal("xattr change did not invalidate candidates")
	}
}

func TestSimpleLocalCopyHeaderStripsSetIDBits(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source")
	if err := os.WriteFile(path, []byte("payload\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o6755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	header, ok := simpleLocalCopyHeader(info, nil, "source")
	if !ok {
		t.Fatal("set-ID source was not representable as a simple local COPY")
	}
	if got := header.Mode & 0o7777; got != 0o755 {
		t.Fatalf("COPY tar mode = %#o, want 0755", got)
	}
}

func TestSimpleLocalCopyDigestCandidatesRejectUncertainInputs(t *testing.T) {
	contextDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(contextDir, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "source"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(contextDir, filepath.Join(contextDir, "link")); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		operation Copy
		artifacts []string
	}{
		{name: "multiple", operation: Copy{Sources: []string{"source", "other"}, Destination: "/target"}},
		{name: "glob", operation: Copy{Sources: []string{"s*"}, Destination: "/target"}},
		{name: "directory", operation: Copy{Sources: []string{"directory"}, Destination: "/target"}},
		{name: "symlink ancestor", operation: Copy{Sources: []string{"link/source"}, Destination: "/target"}},
		{name: "chown", operation: Copy{Sources: []string{"source"}, Destination: "/target", Chown: "1:2"}},
		{name: "chmod", operation: Copy{Sources: []string{"source"}, Destination: "/target", Chmod: "0644"}},
		{name: "exclude", operation: Copy{Sources: []string{"source"}, Destination: "/target", Excludes: []string{"other"}}},
		{name: "protected artifact", operation: Copy{Sources: []string{"source"}, Destination: "/target"}, artifacts: []string{filepath.Join(contextDir, "artifact")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if candidates, eligible := simpleLocalCopyDigestCandidates(contextDir, test.artifacts, test.operation); eligible || candidates != nil {
				t.Fatalf("candidates = %v, %v, want fallback", candidates, eligible)
			}
		})
	}

	if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), []byte("ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	operation := Copy{Sources: []string{"source"}, Destination: "/target"}
	if candidates, eligible := simpleLocalCopyDigestCandidates(contextDir, nil, operation); eligible || candidates != nil {
		t.Fatalf("dockerignore candidates = %v, %v, want fallback", candidates, eligible)
	}
}

func TestSimpleLocalCopyDigestCandidatesConfineAncestorSwap(t *testing.T) {
	contextDir := t.TempDir()
	outside := t.TempDir()
	parent := filepath.Join(contextDir, "parent")
	saved := filepath.Join(contextDir, "parent.saved")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	insideSource := filepath.Join(parent, "source")
	if err := os.WriteFile(insideSource, []byte("inside\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "source"), []byte("outside\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	// Keep atime newer than mtime so ordinary reads do not perturb the tar
	// header under relatime while the confinement race is exercised.
	mtime := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(insideSource, mtime.Add(time.Hour), mtime); err != nil {
		t.Fatal(err)
	}
	operation := Copy{Sources: []string{"parent/source"}, Destination: "/target"}
	want, eligible := simpleLocalCopyDigestCandidates(contextDir, nil, operation)
	if !eligible {
		t.Fatal("stable confined source unexpectedly ineligible")
	}

	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 500 {
			if err := os.Rename(parent, saved); err != nil {
				errCh <- err
				return
			}
			if err := os.Symlink(outside, parent); err != nil {
				errCh <- err
				return
			}
			runtime.Gosched()
			if err := os.Remove(parent); err != nil {
				errCh <- err
				return
			}
			if err := os.Rename(saved, parent); err != nil {
				errCh <- err
				return
			}
		}
	}()
	fallbacks := 0
	for {
		select {
		case err := <-errCh:
			t.Fatal(err)
		case <-done:
			if fallbacks == 0 {
				t.Fatal("ancestor swap never exercised the fallback path")
			}
			return
		default:
			got, eligible := simpleLocalCopyDigestCandidates(contextDir, nil, operation)
			if !eligible {
				fallbacks++
				continue
			}
			if !slices.Equal(got, want) {
				t.Fatalf("ancestor swap escaped context: candidates %v, want %v", got, want)
			}
		}
	}
}

func TestSimpleLocalCopyDigestCandidatesRejectFIFOWithoutBlocking(t *testing.T) {
	contextDir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(contextDir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan bool, 1)
	go func() {
		_, eligible := simpleLocalCopyDigestCandidates(contextDir, nil, Copy{Sources: []string{"pipe"}, Destination: "/pipe"})
		result <- eligible
	}()
	select {
	case eligible := <-result:
		if eligible {
			t.Fatal("FIFO was accepted by simple COPY fast path")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("simple COPY fast path blocked opening a FIFO")
	}
}

func TestSimpleLocalCopyDigestCandidatesMatchBuildah(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "source"), []byte("candidate\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	fixed := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(filepath.Join(contextDir, "source"), fixed, fixed); err != nil {
		t.Fatal(err)
	}

	lease, err := acquireStore(cacheTestStore(root))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	network, err := newNetworkInterface(lease.store)
	if err != nil {
		t.Fatal(err)
	}
	newBuilder := func() *upstream.Builder {
		t.Helper()
		builder, err := upstream.NewBuilder(ctx, lease.store, upstream.BuilderOptions{
			FromImage: "scratch", PullPolicy: define.PullNever, Isolation: define.IsolationChroot,
			Format: define.OCIv1ImageManifest, NetworkInterface: network,
			SystemContext: &types.SystemContext{BigFilesTemporaryDir: root},
		})
		if err != nil {
			t.Fatal(err)
		}
		return builder
	}

	for _, test := range []struct {
		name        string
		destination string
		prepare     func(*upstream.Builder)
	}{
		{name: "renamed absent file", destination: "/bin/sh"},
		{name: "existing directory", destination: "/bin", prepare: func(builder *upstream.Builder) {
			if err := builder.EnsureContainerPathAs("/bin", "", nil); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "same basename", destination: "/source"},
	} {
		t.Run(test.name, func(t *testing.T) {
			builder := newBuilder()
			defer func() {
				if err := builder.Delete(); err != nil {
					t.Errorf("delete builder: %v", err)
				}
			}()
			if test.prepare != nil {
				test.prepare(builder)
			}
			operation := Copy{Sources: []string{"source"}, Destination: test.destination}
			candidates, eligible := simpleLocalCopyDigestCandidates(contextDir, nil, operation)
			if !eligible {
				t.Fatal("simple COPY unexpectedly ineligible")
			}
			actual, err := probeCopyAddDigest(nativeBuilder{Builder: builder, isolation: define.IsolationChroot}, contextDir, nil, operation)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(candidates, actual) {
				t.Fatalf("Buildah digest %s is outside candidates %v", actual, candidates)
			}
		})
	}
}

func TestBuildPlanWarmSimpleCopySkipsBuildahCopier(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "source"), []byte("warm\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	store := cacheTestStore(root)
	plan := testPlan(t, "from \"scratch\"\ncopy \"source\" \"/renamed\"\n")
	var probes, applies atomic.Int32
	observer := copyAddDigestObservation(func(dryRun bool) {
		if dryRun {
			probes.Add(1)
		} else {
			applies.Add(1)
		}
	})
	copyAddDigestObserver.Store(&observer)
	t.Cleanup(func() { copyAddDigestObserver.Store(nil) })
	build := func(name string) digest.Digest {
		t.Helper()
		layout := filepath.Join(root, name)
		if _, err := BuildPlan(ctx, plan, PlanOptions{
			Store: store, ContextDir: contextDir, Isolation: "rootless", Output: Output{Path: layout},
		}); err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		if len(manifest.Layers) != 1 {
			t.Fatalf("%s layers = %d, want 1", name, len(manifest.Layers))
		}
		return manifest.Layers[0].Digest
	}
	coldLayer := build("cold")
	if probes.Load() != 0 || applies.Load() != 1 {
		t.Fatalf("cold Buildah copier digest calls = probe %d, apply %d; want 0, 1", probes.Load(), applies.Load())
	}
	probes.Store(0)
	applies.Store(0)
	warmLayer := build("warm")
	if warmLayer != coldLayer {
		t.Fatalf("warm layer %s differs from cold %s", warmLayer, coldLayer)
	}
	if probes.Load() != 0 || applies.Load() != 0 {
		t.Fatalf("warm Buildah copier digest calls = probe %d, apply %d; want 0, 0", probes.Load(), applies.Load())
	}
}
