//go:build linux

package build

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/strio-dev/coopr/internal/buildah"
	"golang.org/x/sys/unix"
)

// Exercise the actual frontend -> dedicated plan worker -> frontend return path.
// Remote ADD blocks without a workload RUN, runtime daemon, registry or package
// publication. The native suite supplies its normal rootless storage namespace.
func TestNativeFrontendsRetainWorkerEvidenceAfterForcedLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native rootless storage, procfs and Linux pidfd child ownership")
	}
	for _, kind := range []string{"image", "component"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			temporary := filepath.Join(root, "temporary")
			contextDir := filepath.Join(root, "context")
			for _, directory := range []string{temporary, contextDir} {
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("TMPDIR", temporary)
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			arrived := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.Header().Set("Content-Length", "1")
					return
				}
				if r.Method != http.MethodGet || r.URL.Path != "/owned-input" {
					http.NotFound(w, r)
					return
				}
				select {
				case arrived <- struct{}{}:
				default:
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			defer server.CloseClientConnections()
			source := fmt.Sprintf("from \"scratch\" { add %q \"/payload\"; }\n", server.URL+"/owned-input")
			if kind == "component" {
				source = fmt.Sprintf("package as=\"payload\" { add %q \"/payload\"; }\nextend { copy \"/payload\" \"/installed\" from=\"payload\"; }\n", server.URL+"/owned-input")
			}
			definition := filepath.Join(contextDir, "input.coopr")
			if err := os.WriteFile(definition, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			input := "owned materialized input"
			if err := writer.WriteHeader(&tar.Header{Name: "input", Mode: 0o600, Size: int64(len(input))}); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write([]byte(input)); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			metadata := filepath.Join(root, "delivered-metadata.json")
			iid := filepath.Join(root, "delivered-image.id")
			store := nativeBuildTestStore(filepath.Join(root, "native"))
			type outcome struct {
				image     Result
				component string
				err       error
			}
			done := make(chan outcome, 1)
			go func() {
				if kind == "image" {
					result, err := Run(ctx, Options{File: definition, Context: "-", Stdin: &archive, Platform: "linux/amd64", BuildStore: store, ComponentStoreDir: filepath.Join(root, "components"), MetadataFile: metadata, IIDFile: iid})
					done <- outcome{image: result, err: err}
				} else {
					result, err := BuildComponent(ctx, ComponentOptions{File: definition, Context: "-", Stdin: &archive, Platform: "linux/amd64", BuildStore: store, StoreDir: filepath.Join(root, "components"), MetadataFile: metadata})
					done <- outcome{component: result, err: err}
				}
			}()
			select {
			case <-arrived:
			case result := <-done:
				t.Fatalf("native frontend did not reach controlled ADD: %v", result.err)
			case <-ctx.Done():
				t.Fatal("native frontend did not start ADD", ctx.Err())
			}
			primaries, err := filepath.Glob(filepath.Join(temporary, "coopr-primary-archive-*", "download"))
			if err != nil || len(primaries) != 1 {
				cancel()
				select {
				case <-done:
				case <-time.After(30 * time.Second):
					t.Fatal("native frontend failed to drain after fixture setup failure")
				}
				t.Fatalf("native frontend did not materialize its owned primary: %v (%v)", primaries, err)
			}
			primary := primaries[0]
			pid, err := killOwnedFrontendPlanChild(temporary, primary)
			if err != nil {
				cancel()
				select {
				case <-done:
				case <-time.After(30 * time.Second):
					t.Fatal("native frontend failed to drain after ownership proof failure")
				}
				t.Fatal(err)
			}
			var result outcome
			select {
			case result = <-done:
			case <-ctx.Done():
				t.Fatal("frontend did not drain after owned worker loss", ctx.Err())
			}
			if !errors.Is(result.err, buildah.ErrWorkerWaitIncomplete) {
				t.Fatalf("lost incomplete-worker classification after killing %d: %v", pid, result.err)
			}
			if result.image.ImageID != "" || len(result.image.References) != 0 || result.component != "" {
				t.Fatalf("forced worker loss returned a completed result: %+v", result)
			}
			stages, err := filepath.Glob(filepath.Join(temporary, ".coopr-stage-*"))
			if err != nil || len(stages) != 1 {
				t.Fatalf("frontend deleted retained worker ancestor: %v (%v), result=%v", stages, err, result.err)
			}
			if !strings.Contains(result.err.Error(), stages[0]) {
				t.Fatalf("retained ancestor path missing from diagnostic: %v", result.err)
			}
			workers, err := filepath.Glob(filepath.Join(stages[0], ".coopr-build-worker-*"))
			if err != nil || len(workers) != 1 {
				t.Fatalf("private worker evidence missing: %v (%v)", workers, err)
			}
			request, err := os.ReadFile(filepath.Join(workers[0], "request.json"))
			if err != nil {
				t.Fatalf("worker request deleted by outer frontend cleanup: %v", err)
			}
			var captured struct {
				Mode       string `json:"mode"`
				ContextDir string `json:"context_dir"`
			}
			if err := json.Unmarshal(request, &captured); err != nil {
				t.Fatal(err)
			}
			expected := "build"
			if kind == "component" {
				expected = "publish"
			}
			if captured.Mode != expected || captured.ContextDir != primary {
				t.Fatalf("retained evidence belongs to another invocation: %+v", captured)
			}
			if data, err := os.ReadFile(filepath.Join(primary, "input")); err != nil || string(data) != input {
				t.Fatalf("frontend deleted owned primary still referenced by incomplete worker: %q (%v)", data, err)
			}
			if !strings.Contains(result.err.Error(), primary) {
				t.Fatalf("retained primary path missing from diagnostic: %v", result.err)
			}
			if _, err := os.Stat(definition); err != nil {
				t.Fatalf("caller-owned definition removed: %v", err)
			}
			for _, path := range []string{metadata, iid, filepath.Join(stages[0], "image")} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("incomplete output was promoted: %s (%v)", path, err)
				}
			}
			t.Logf("%s frontend retained plan %d evidence through its return: %s", kind, pid, workers[0])
		})
	}
}

// Enumeration and request metadata select this fixture's plan, but grant no
// signal authority. The pidfd plus waitid child proof binds the signal to the
// caller's actual unreaped child without consuming Cmd.Wait's exit status.
func killOwnedFrontendPlanChild(temporary, contextDir string) (int, error) {
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return 0, err
	}
	seen := map[int]bool{}
	for _, task := range tasks {
		children, err := os.ReadFile(filepath.Join("/proc/self/task", task.Name(), "children"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		for _, field := range strings.Fields(string(children)) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				return 0, err
			}
			if seen[pid] {
				continue
			}
			seen[pid] = true
			matched, err := signalOwnedFrontendPlanCandidate(pid, temporary, contextDir)
			if err != nil {
				return 0, err
			}
			if matched {
				return pid, nil
			}
		}
	}
	return 0, errors.New("controlled ADD did not expose this fixture's owned plan child")
}

func signalOwnedFrontendPlanCandidate(pid int, temporary, contextDir string) (bool, error) {
	// Acquire identity before reading any numeric /proc metadata. If the child
	// exits or is reaped while selection runs, the final pidfd child proof fails;
	// PID reuse cannot retarget the signal to a new process.
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open candidate child pidfd: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	command, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	args := strings.Split(strings.TrimRight(string(command), "\x00"), "\x00")
	if len(args) != 2 || (args[0] != "coopr-buildah-plan-worker-v1" && args[0] != "coopr-buildah-plan-worker-v1-in-a-user-namespace") {
		return false, nil
	}
	relative, err := filepath.Rel(temporary, args[1])
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false, nil
	}
	request, err := os.ReadFile(args[1])
	if err != nil {
		return false, err
	}
	var captured struct {
		ContextDir string `json:"context_dir"`
	}
	if err := json.Unmarshal(request, &captured); err != nil {
		return false, err
	}
	if captured.ContextDir != contextDir {
		return false, nil
	}
	var info unix.Siginfo
	deadline := time.Now().Add(time.Second)
	for {
		err = unix.Waitid(unix.P_PIDFD, fd, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
		if !time.Now().Before(deadline) {
			return false, errors.New("timed out verifying owned plan child")
		}
	}
	if err != nil {
		return false, fmt.Errorf("verify owned plan child %d: %w", pid, err)
	}
	if info.Signo != 0 {
		return false, errors.New("owned plan exited before forced-loss injection")
	}
	if err := unix.PidfdSendSignal(fd, syscall.SIGKILL, nil, 0); err != nil {
		return false, fmt.Errorf("signal owned plan child %d: %w", pid, err)
	}
	return true, nil
}
