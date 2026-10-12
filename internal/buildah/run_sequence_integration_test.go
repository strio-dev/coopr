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

func TestRunExecSequenceSharesMountsAndCachesOneInstruction(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	writeLinkedRunContext(t, contextDir)
	store := cacheTestStore(root)
	plan := testPlan(t, `from "scratch" {
 copy "busybox" "/busybox"
 run network="none" {
  mount "tmpfs" target="/temporary"
  exec "/busybox" "sh" "-ec" "printf first >/temporary/value; printf cached >/cache/value"
  mount "cache" target="/cache" id="sequence" sharing="locked"
  exec "/busybox" "sh" "-ec" "test \"$(/busybox cat /temporary/value)\" = first; test \"$(/busybox cat /cache/value)\" = cached; /busybox od -An -N16 -tx1 /dev/urandom >/proof"
 }
}`)
	var coldProof string
	for attempt := range 2 {
		layout := filepath.Join(root, fmt.Sprintf("layout-%d", attempt))
		var progress strings.Builder
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: contextDir, Isolation: "rootless", Runtime: "crun",
			Output: Output{Path: layout}, Stdout: io.Discard, Stderr: &progress,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, layout)
		if len(manifest.Layers) != 2 || len(image.RootFS.DiffIDs) != 2 {
			t.Fatalf("build %d layers/diffids = %d/%d, want COPY plus one RUN layer", attempt, len(manifest.Layers), len(image.RootFS.DiffIDs))
		}
		runHistory := 0
		for _, entry := range image.History {
			if strings.HasPrefix(entry.CreatedBy, "RUN ") {
				runHistory++
				if entry.EmptyLayer {
					t.Fatal("sequence RUN was recorded as an empty layer")
				}
			}
		}
		if runHistory != 1 {
			t.Fatalf("RUN history entries = %d, want 1: %#v", runHistory, image.History)
		}
		blob := filepath.Join(layout, "blobs", "sha256", manifest.Layers[1].Digest.Encoded())
		proof := readLayerFile(t, blob, "proof")
		if strings.TrimSpace(proof) == "" {
			t.Fatal("second exec did not produce proof")
		}
		assertLayerNamesAbsent(t, blob, "temporary/value", "cache/value")
		assertRunSequenceHelperAbsent(t, blob)
		if attempt == 0 {
			coldProof = proof
		} else {
			if proof != coldProof {
				t.Fatalf("warm sequence reran: cold=%q warm=%q", coldProof, proof)
			}
			if !strings.Contains(progress.String(), "--> Using cache ") {
				t.Fatalf("warm sequence did not report a cache hit: %s", progress.String())
			}
		}
	}
}

func TestRunExecSequenceStopsAtFirstFailure(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	writeLinkedRunContext(t, contextDir)
	options := SupervisedPlanOptions{
		Store: cacheTestStore(root), ContextDir: contextDir, Isolation: "rootless", Runtime: "crun",
		Output: Output{Path: filepath.Join(root, "failed")}, Stdout: io.Discard, Stderr: io.Discard,
	}
	failed := testPlan(t, `from "scratch" {
 copy "busybox" "/busybox"
 run network="none" {
  mount "cache" target="/cache" id="failfast" sharing="locked"
  exec "/busybox" "sh" "-ec" "printf reached >/cache/before; exit 23"
  exec "/busybox" "touch" "/cache/after"
 }
}`)
	if _, err := BuildPlanSupervised(ctx, failed, options); err == nil || !strings.Contains(err.Error(), "exit status 23") {
		t.Fatalf("failed sequence error = %v, want exit status 23", err)
	}
	// A cache mount remains observable after a failed build, so the probe proves
	// the first command ran and the later command never did.
	probe := testPlan(t, `from "scratch" {
 copy "busybox" "/busybox"
 run network="none" {
  mount "cache" target="/cache" id="failfast" sharing="locked"
  exec "/busybox" "sh" "-ec" "test \"$(/busybox cat /cache/before)\" = reached; test ! -e /cache/after; printf stopped >/proof"
 }
}`)
	options.Output.Path = filepath.Join(root, "probe")
	if _, err := BuildPlanSupervised(ctx, probe, options); err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, options.Output.Path)
	blob := filepath.Join(options.Output.Path, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded())
	if proof := readLayerFile(t, blob, "proof"); proof != "stopped" {
		t.Fatalf("fail-fast proof = %q", proof)
	}
}

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
