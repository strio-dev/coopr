package buildah

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	storagearchive "go.podman.io/storage/pkg/archive"
)

func assertRunSequenceHelperAbsent(t *testing.T, blob string) {
	t.Helper()
	file, err := os.Open(blob)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck
	reader, err := storagearchive.DecompressStream(file)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close() //nolint:errcheck
	entries := tar.NewReader(reader)
	for {
		entry, err := entries.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimPrefix(entry.Name, "./")
		if strings.HasPrefix(name, ".coopr-run-") || filepath.Base(name) == runSequenceHelper {
			t.Fatalf("RUN helper leaked into image: %s", entry.Name)
		}
	}
}

// Chroot shares the host PID namespace. Cancellation must therefore terminate
// the exec's descendants even when killing the helper cannot tear down a PIDns.
func TestRunExecSequenceChrootCancellationStopsDescendants(t *testing.T) {
	requireLiveInstructionCache(t)
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	writeLinkedRunContext(t, contextDir)
	monitor := filepath.Join(contextDir, "monitor")
	if err := os.Mkdir(monitor, 0o700); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, `from "scratch" {
 copy "busybox" "/busybox"
 run {
  exec "/busybox" "sh" "-c" "/busybox sleep 60 & child=$!; printf '%s 0 %s 0' $$ \"$child\" >/monitor/ready; wait \"$child\""
  exec "/busybox" "touch" "/monitor/later"
 }
}`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: cacheTestStore(root), ContextDir: contextDir,
			RunControls: RunControls{Volumes: []string{monitor + ":/monitor:rw,Z"}},
			Isolation:   "chroot", Output: Output{Path: filepath.Join(root, "output")},
			Stdout: io.Discard, Stderr: io.Discard,
		})
		done <- err
	}()
	var values [4]int
	readyDeadline := time.Now().Add(15 * time.Second)
	for values[0] == 0 && time.Now().Before(readyDeadline) {
		select {
		case err := <-done:
			t.Fatalf("sequence exited before readiness: %v", err)
		default:
		}
		values = waitReadyValues(filepath.Join(monitor, "ready"), 50*time.Millisecond)
	}
	if values[0] <= 0 || values[2] <= 0 {
		cancel()
		select {
		case err := <-done:
			t.Fatalf("sequence did not become ready: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("sequence did not become ready or stop after cancellation")
		}
	}
	pids := []int{values[0], values[2]}
	identities := make(map[int]string, len(pids))
	// PID start times prevent failed-test cleanup from signaling a reused PID.
	defer func() {
		for pid, identity := range identities {
			current, state, err := runSequenceProcessState(pid)
			if err == nil && current == identity && state != "Z" && state != "X" {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}()
	for _, pid := range pids {
		identity, state, err := runSequenceProcessState(pid)
		if err != nil || state == "Z" || state == "X" {
			t.Fatalf("command process %d was not running: state=%q err=%v", pid, state, err)
		}
		identities[pid] = identity
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervised build did not stop after cancellation")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		running := []int{}
		for pid, identity := range identities {
			current, state, err := runSequenceProcessState(pid)
			if errors.Is(err, os.ErrNotExist) || err == nil && (current != identity || state == "Z" || state == "X") {
				continue
			}
			if err != nil {
				t.Fatalf("inspect canceled process %d: %v", pid, err)
			}
			running = append(running, pid)
		}
		if len(running) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("canceled sequence left running processes: %v", running)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(monitor, "later")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("later exec ran after cancellation: %v", err)
	}
}

func runSequenceProcessState(pid int) (identity, state string, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", "", err
	}
	// comm can contain spaces and parentheses; fields after its final ')' begin
	// with state (field 3), followed by the start time at field 22.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", "", fmt.Errorf("malformed process stat for %d", pid)
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 {
		return "", "", fmt.Errorf("short process stat for %d", pid)
	}
	return fields[19], fields[0], nil
}
