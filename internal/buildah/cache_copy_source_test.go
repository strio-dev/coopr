package buildah

import (
	"context"
	"github.com/strio-dev/coopr/internal/definition"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"golang.org/x/sys/unix"
)

func TestBuildPlanWarmSimpleCopyUsesBuildahProbeWithoutApplyingPayload(t *testing.T) {
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
	plan := testPlan(t, `from "scratch" {
    copy "source" "/new-parent/renamed"
}
`)
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
	if probes.Load() != 1 || applies.Load() != 1 {
		t.Fatalf("cold Buildah copier digest calls = probe %d, apply %d; want 1, 1", probes.Load(), applies.Load())
	}
	probes.Store(0)
	applies.Store(0)
	// Buildah excludes source timestamps from its content digest.
	future := time.Unix(1900000000, 0)
	if err := os.Chtimes(filepath.Join(contextDir, "source"), future, future); err != nil {
		t.Fatal(err)
	}
	warmLayer := build("warm")
	if warmLayer != coldLayer {
		t.Fatalf("warm layer %s differs from cold %s", warmLayer, coldLayer)
	}
	if probes.Load() != 1 || applies.Load() != 0 {
		t.Fatalf("warm Buildah copier digest calls = probe %d, apply %d; want 1, 0", probes.Load(), applies.Load())
	}
	for _, change := range []struct {
		name  string
		apply func() error
	}{
		{"content", func() error {
			return os.WriteFile(filepath.Join(contextDir, "source"), []byte("changed payload\n"), 0o640)
		}},
		{"mode", func() error { return os.Chmod(filepath.Join(contextDir, "source"), 0o600) }},
		{"xattr", func() error {
			return unix.Setxattr(filepath.Join(contextDir, "source"), "user.coopr-cache", []byte("changed"), 0)
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			if err := change.apply(); err != nil {
				t.Fatal(err)
			}
			probes.Store(0)
			applies.Store(0)
			build("changed-" + change.name)
			if probes.Load() != 1 || applies.Load() != 1 {
				t.Fatalf("changed %s reused cache: probe=%d apply=%d", change.name, probes.Load(), applies.Load())
			}
		})
	}
}

func TestBuildahCopyProbePreservesDigesterAndWritesNoPayload(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("payload"), 0o640); err != nil {
		t.Fatal(err)
	}
	lease, err := acquireStore(cacheTestStore(filepath.Join(root, "store")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}()
	builder, err := upstream.NewBuilder(ctx, lease.store, upstream.BuilderOptions{FromImage: "scratch", PullPolicy: define.PullNever, Isolation: define.IsolationChroot})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := builder.Delete(); err != nil {
			t.Error(err)
		}
	}()
	builder.ContentDigester.Start("")
	if _, err := builder.ContentDigester.Hash().Write([]byte("previous")); err != nil {
		t.Fatal(err)
	}
	beforeType, beforeDigest := builder.ContentDigester.Digest()
	adapter := nativeBuilder{Builder: builder, isolation: define.IsolationChroot}
	operation := Copy{Sources: []string{"source"}, Destination: "/created/copied"}
	probed, err := probeCopyAddDigestContext(ctx, adapter, root, nil, operation)
	if err != nil {
		t.Fatal(err)
	}
	afterType, afterDigest := builder.ContentDigester.Digest()
	if beforeType != afterType || beforeDigest != afterDigest {
		t.Fatalf("probe changed native digester: %s:%s -> %s:%s", beforeType, beforeDigest, afterType, afterDigest)
	}
	mount, err := builder.Mount(builder.MountLabel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(mount, "created/copied")); !os.IsNotExist(err) {
		t.Fatalf("probe wrote file payload: %v", err)
	}
	applied, err := applyCopyAddWithDigestContext(ctx, adapter, root, nil, operation)
	if err != nil {
		t.Fatal(err)
	}
	if applied != probed {
		t.Fatalf("probe %s differs from apply %s", probed, applied)
	}
	afterApplyType, afterApplyDigest := builder.ContentDigester.Digest()
	if beforeType != afterApplyType || beforeDigest != afterApplyDigest {
		t.Fatal("apply changed previous native digester state")
	}
	// Consecutive operations must not accumulate the previous input digest.
	if repeated, err := probeCopyAddDigestContext(ctx, adapter, root, nil, operation); err != nil || repeated != probed {
		t.Fatalf("consecutive digest=%s, err=%v; want %s", repeated, err, probed)
	}
	inline := Copy{InlineFiles: []definition.InlineFile{{Path: "first", Data: "one"}, {Path: "second", Data: "two"}}, Destination: "/inline/"}
	inlineProbe, err := probeCopyAddDigestContext(ctx, adapter, root, nil, inline)
	if err != nil {
		t.Fatal(err)
	}
	inlineApplied, err := applyCopyAddWithDigestContext(ctx, adapter, root, nil, inline)
	if err != nil || inlineProbe != inlineApplied {
		t.Fatalf("inline probe/apply %s/%s: %v", inlineProbe, inlineApplied, err)
	}
	inline.InlineFiles[1].Data = "changed"
	changedInline, err := probeCopyAddDigestContext(ctx, adapter, root, nil, inline)
	if err != nil || changedInline == inlineProbe {
		t.Fatalf("changed inline digest %s: %v", changedInline, err)
	}
	if data, err := os.ReadFile(filepath.Join(mount, "created/copied")); err != nil || string(data) != "payload" {
		t.Fatalf("applied file = %q, %v", data, err)
	}
}
