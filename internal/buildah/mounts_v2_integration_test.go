package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/strio-dev/coopr/internal/planner"
)

func TestBuildDefinitionArtifactRunRawViewsWarmCacheAndInvalidation(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0700); err != nil {
		t.Fatal(err)
	}
	store := cacheTestStore(filepath.Join(root, "store"))
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	const reference = "registry.example/mounts/raw:stable"
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	if err := w.WriteHeader(&tar.Header{Name: "unpacked", Mode: 0600, Size: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	seedArtifactCopyInput(t, ctx, store, reference, map[string]string{"payload.tar": archive.String(), "message": "first"})
	definition := parseWorkerDefinition(t, fmt.Sprintf(`from %q {
run "cmp /all/payload.tar /selected && test ! -e /all/unpacked && if echo forbidden >/all/message 2>/dev/null; then exit 42; fi; cat /all/message >/proof" network="none" {
 mount "bind" from=%q target="/all"
 mount "bind" from=%q source="payload.tar" target="/selected"
}
}`, base.reference, reference, reference))
	policy := writeComponentTestPolicy(t, root)
	build := func(name, want string) digest.Digest {
		t.Helper()
		layout := filepath.Join(root, name)
		var log strings.Builder
		_, err := BuildDefinitionSupervised(ctx, definition, planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{Store: store, ContextDir: contextDir, Runtime: "crun", Isolation: "rootless", Output: Output{Path: layout}, PullPolicy: "never", SignaturePolicyPath: policy, CacheLocalDir: filepath.Join(root, "results"), Stdout: io.Discard, Stderr: &log})
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, log.String())
		}
		if name == "warm" && !strings.Contains(log.String(), "Using cache") {
			t.Fatalf("warm RUN cache not reused:\n%s", log.String())
		}
		manifest, _ := readPlanImage(t, layout)
		last := manifest.Layers[len(manifest.Layers)-1]
		if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof"); got != want {
			t.Fatalf("%s proof=%q want=%q", name, got, want)
		}
		return last.Digest
	}
	cold := build("cold", "first")
	if warm := build("warm", "first"); warm != cold {
		t.Fatalf("warm artifact result changed: %s/%s", cold, warm)
	}
	seedArtifactCopyInput(t, ctx, store, reference, map[string]string{"payload.tar": archive.String(), "message": "second"})
	if changed := build("changed", "second"); changed == cold {
		t.Fatal("artifact digest change did not invalidate RUN")
	}
	nonRoot := parseWorkerDefinition(t, fmt.Sprintf(`from %q {
 user "12345:12345"
 run "/bin/busybox test \"$(/bin/busybox id -u)\" = 12345 && /bin/busybox cmp /all/payload.tar /selected && /bin/busybox test \"$(/bin/busybox cat /all/message)\" = second" network="none" {
  mount "bind" from=%q target="/all"
  mount "bind" from=%q source="payload.tar" target="/selected"
 }
}`, base.reference, reference, reference))
	if _, err := BuildDefinitionSupervised(ctx, nonRoot, planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{Store: store, ContextDir: contextDir, Runtime: "crun", Isolation: "rootless", Output: Output{Path: filepath.Join(root, "nonroot")}, PullPolicy: "never", SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard}); err != nil {
		t.Fatalf("inherited non-root USER cannot read private artifact view: %v", err)
	}
}

func TestLiveHostRunBindMutationsAreNeverReplayedFromCache(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	live := filepath.Join(root, "live")
	for _, dir := range []string{contextDir, live} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	store := cacheTestStore(filepath.Join(root, "store"))
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	plan := testPlan(t, fmt.Sprintf(`from %q { run "printf x >>/live/counter && cat /live/counter >/proof" network="none" { mount "bind" target="/live" readonly=#false relabel="shared" } }`, base.reference))
	op := &plan.Stages[0].Operations[0]
	op.HostMounts = map[int]string{0: live}
	delete(op.MountContexts, 0)
	if planUsesWritableContext(plan, nil) {
		t.Fatal("live host bind requested a caller context snapshot")
	}
	policy := writeComponentTestPolicy(t, root)
	for i := range 2 {
		layout := filepath.Join(root, fmt.Sprintf("result-%d", i))
		var log strings.Builder
		if _, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{Store: store, ContextDir: contextDir, Runtime: "crun", Isolation: "rootless", Output: Output{Path: layout}, CacheLocalDir: filepath.Join(root, "results"), SignaturePolicyPath: policy, PullPolicy: "never", Stdout: &log, Stderr: &log}); err != nil {
			t.Fatalf("%v\n%s", err, log.String())
		}
		got, err := os.ReadFile(filepath.Join(live, "counter"))
		if err != nil || string(got) != strings.Repeat("x", i+1) {
			t.Fatalf("live write skipped on run %d: %q %v", i, got, err)
		}
	}
	op.Children[0].Properties["readonly"] = "true"
	if _, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{Store: store, ContextDir: contextDir, Runtime: "crun", Isolation: "rootless", Output: Output{Path: filepath.Join(root, "read-only")}, SignaturePolicyPath: policy, PullPolicy: "never", Stdout: io.Discard, Stderr: io.Discard}); err == nil {
		t.Fatal("read-only live host mount allowed mutation")
	}
	got, err := os.ReadFile(filepath.Join(live, "counter"))
	if err != nil || string(got) != "xx" {
		t.Fatalf("read-only host content changed: %q %v", got, err)
	}
}

func TestPersistentRunCacheMountCrossesGraphRootsAndLocks(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0700); err != nil {
		t.Fatal(err)
	}
	mountDir := filepath.Join(root, "mounts")
	policy := writeComponentTestPolicy(t, root)
	id := "mounts-v2-" + digest.FromString(root).Encoded()[:16]
	type fixture struct {
		store StoreOptions
		base  string
	}
	fixtures := make([]fixture, 2)
	for i := range fixtures {
		fixtures[i].store = cacheTestStore(filepath.Join(root, fmt.Sprintf("store-%d", i)))
		fixtures[i].base = newLiveBusyBoxStorage(t, ctx, root, fixtures[i].store).reference
	}
	build := func(index int, name, script string) error {
		fixture := fixtures[index]
		plan := testPlan(t, fmt.Sprintf(`from %q { run %q network="none" { mount "cache" target="/cache" id=%q sharing="locked" } }`, fixture.base, script, id))
		var log strings.Builder
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{Store: fixture.store, ContextDir: contextDir, CacheMountDir: mountDir, Runtime: "crun", Isolation: "rootless", NoCache: true, Output: Output{Path: filepath.Join(root, name)}, SignaturePolicyPath: policy, PullPolicy: "never", Stdout: &log, Stderr: &log})
		if err != nil {
			return fmt.Errorf("%s: %w\n%s", name, err, log.String())
		}
		return nil
	}
	if err := build(0, "seed", "test ! -e /cache/value; printf persisted >/cache/value"); err != nil {
		t.Fatal(err)
	}
	if err := build(1, "consume", "test \"$(cat /cache/value)\" = persisted"); err != nil {
		t.Fatalf("cache did not survive a different native graph root: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- build(i, fmt.Sprintf("concurrent-%d", i), "/bin/busybox mkdir /cache/held || exit 77; /bin/busybox sleep 1; /bin/busybox rmdir /cache/held; printf x >>/cache/count")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("cross-root locked cache overlapped: %v", err)
		}
	}
	if err := build(0, "verify", "test \"$(cat /cache/count)\" = xx"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveWorkerDirectory(mountDir, nil); err != nil {
		t.Fatal(err)
	}
	if err := build(1, "empty-cache", "test ! -e /cache/value; printf recovered >/cache/value"); err != nil {
		t.Fatalf("task did not tolerate an empty cache: %v", err)
	}
}

func TestPersistentCacheMountParentPropagatesToIsolatedStages(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0700); err != nil {
		t.Fatal(err)
	}
	store := cacheTestStore(filepath.Join(root, "store"))
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	id := "isolated-" + digest.FromString(root).Encoded()[:16]
	critical := `/bin/busybox mkdir /cache/held || exit 77; /bin/busybox sleep 1; /bin/busybox rmdir /cache/held; printf x >>/cache/count; touch /marker`
	plan := testPlan(t, fmt.Sprintf(`from %q as="left" {
 run %q network="none" { mount "cache" target="/cache" id=%q sharing="locked" }
}
from %q as="right" {
 run %q network="none" { mount "cache" target="/cache" id=%q sharing="locked" }
}
from "left" {
 copy "/marker" "/right-marker" from="right"
 run %q network="none" { mount "cache" target="/cache" id=%q sharing="locked" }
}`, base.reference, critical, id, base.reference, critical, id, `test "$(cat /cache/count)" = xx`, id))
	var log strings.Builder
	if _, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{Store: store, ContextDir: contextDir, CacheMountDir: filepath.Join(root, "mounts"), Runtime: "crun", Isolation: "rootless", NoCache: true, Jobs: 2, Output: Output{Path: filepath.Join(root, "output")}, SignaturePolicyPath: writeComponentTestPolicy(t, root), PullPolicy: "never", Stdout: &log, Stderr: &log}); err != nil {
		t.Fatalf("isolated stages lost persistent cache coordination: %v\n%s", err, log.String())
	}
}
