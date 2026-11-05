package buildah

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestArtifactRunForcedWorkerExitCleansSupervisorOwnedViews(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "payload"), []byte("context"), 0600); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(root, "live")
	if err := os.Mkdir(live, 0700); err != nil {
		t.Fatal(err)
	}
	store := cacheTestStore(filepath.Join(root, "store"))
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	const reference = "registry.example/mounts/forced:stable"
	seedArtifactCopyInput(t, ctx, store, reference, map[string]string{"message": "verified"})
	command := `/bin/busybox test "$(/bin/busybox cat /files/message)" = verified && /bin/busybox test "$(/bin/busybox cat /input/payload)" = context && printf ready >/live/started && while /bin/busybox test ! -e /live/release; do /bin/busybox sleep 0.05; done`
	plan := testPlan(t, fmt.Sprintf(`from %q {
 run %q network="none" {
  mount "bind" from=%q target="/files"
  mount "bind" target="/input" readonly=#true
  mount "bind" target="/live" readonly=#false relabel="shared"
 }
}`, base.reference, command, reference))
	plan.Stages[0].Operations[0].HostMounts = map[int]string{2: live}
	delete(plan.Stages[0].Operations[0].MountContexts, 2)
	policy := writeComponentTestPolicy(t, root)
	var logs strings.Builder
	done := make(chan error, 1)
	go func() {
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{Store: store, ContextDir: contextDir, CacheMountDir: filepath.Join(root, "mounts"), Runtime: "crun", Isolation: "rootless", Output: Output{Path: filepath.Join(root, "result")}, PullPolicy: "never", SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: &logs})
		done <- err
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	ready := false
	for !ready {
		select {
		case err := <-done:
			t.Fatalf("RUN did not reach its mounted-input gate: %v\n%s", err, logs.String())
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
			data, err := os.ReadFile(filepath.Join(live, "started"))
			ready = err == nil && string(data) == "ready"
		}
	}
	jobs, err := filepath.Glob(filepath.Join(root, ".coopr-build-worker-*"))
	if err != nil || len(jobs) != 1 {
		t.Fatalf("owned worker job identity ambiguous: %v %v", jobs, err)
	}
	job := jobs[0]
	views, contexts := 0, 0
	if err := filepath.WalkDir(job, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "coopr-artifact-files-") {
			views++
		}
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "coopr-context-") {
			contexts++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if views != 1 || contexts != 1 {
		t.Fatalf("private inputs were not scoped under supervisor job: artifact=%d context=%d", views, contexts)
	}
	requestPath := filepath.Join(job, "request.json")
	group := ownedPlanWorkerGroup(t, requestPath)
	if err := syscall.Kill(-group, syscall.SIGKILL); err != nil {
		t.Fatalf("terminate owned worker group: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("forced worker exit reported success")
		}
	case <-ctx.Done():
		t.Fatal("supervisor did not drain forced worker exit", ctx.Err())
	}
	if _, err := os.Stat(job); !os.IsNotExist(err) {
		t.Fatalf("supervisor job/private artifact view remains: %v", err)
	}
	for _, parent := range []string{filepath.Join(root, "mounts"), store.GraphRoot + "-tmp"} {
		if err := filepath.WalkDir(parent, func(path string, entry fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if entry.IsDir() && (strings.HasPrefix(entry.Name(), "coopr-artifact-files-") || strings.HasPrefix(entry.Name(), "coopr-context-")) {
				return fmt.Errorf("private input escaped owned job: %s", path)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// Discover only the process that names this test's exact private request. The
// process-group guard prevents accidentally signaling the test/session group.
func ownedPlanWorkerGroup(t *testing.T, requestPath string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	groups := map[int]bool{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := bytes.Split(bytes.TrimSuffix(data, []byte{0}), []byte{0})
		if len(args) != 2 || string(args[1]) != requestPath || !strings.HasPrefix(string(args[0]), planWorkerName) {
			continue
		}
		group, err := syscall.Getpgid(pid)
		if err != nil {
			continue
		}
		if group <= 1 || group == syscall.Getpgrp() {
			t.Fatal("owned worker process has an unsafe group identity")
		}
		groups[group] = true
	}
	if len(groups) != 1 {
		t.Fatalf("owned worker group identity ambiguous: %v", groups)
	}
	for group := range groups {
		return group
	}
	panic("unreachable")
}
