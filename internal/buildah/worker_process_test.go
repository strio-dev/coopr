package buildah

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.podman.io/storage/pkg/reexec"
)

const workerProcessHelperMode = "COOPR_TEST_WORKER_PROCESS_MODE"

func TestBuildWorkersDoNotInheritSigningPassword(t *testing.T) {
	environment := []string{"PATH=/tools", "COSIGN_PASSWORD=private", "HTTP_PROXY=proxy"}
	filtered := withoutSigningPassword(environment)
	if strings.Contains(strings.Join(filtered, "\n"), "COSIGN_PASSWORD") || len(filtered) != 2 || len(environment) != 3 {
		t.Fatalf("filtered environment = %v; parent = %v", filtered, environment)
	}
}

func TestWorkerProcessHelper(t *testing.T) {
	switch os.Getenv(workerProcessHelperMode) {
	case "":
		return
	case "success":
		return
	case "self-kill":
		if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		select {}
	case "grandchild":
		signalIgnoreTermination()
		signalReady(t, os.Getpid(), syscall.Getpgrp(), 0, 0)
		select {}
	case "parent":
		signalIgnoreTermination()
		startWorkerProcessGrandchild(t)
	case "cooperative-parent":
		startWorkerProcessGrandchild(t)
	default:
		t.Fatalf("unknown helper mode %q", os.Getenv(workerProcessHelperMode))
	}
}

func testWorkerBinary(t *testing.T) string {
	t.Helper()
	// Nix build sandboxes may omit /proc, while the rootless harness can run
	// from an anonymous memfd whose synthetic os.Executable path is absent.
	if executable, err := os.Executable(); err == nil {
		if _, err := os.Stat(executable); err == nil {
			return executable
		}
	}
	return reexec.Self()
}

func startWorkerProcessGrandchild(t *testing.T) {
	t.Helper()
	grandchild := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$")
	grandchild.Env = replaceEnv(os.Environ(), workerProcessHelperMode, "grandchild")
	grandchildReady := os.Getenv("COOPR_TEST_WORKER_READY") + ".grandchild"
	grandchild.Env = replaceEnv(grandchild.Env, "COOPR_TEST_WORKER_READY", grandchildReady)
	if err := grandchild.Start(); err != nil {
		t.Fatal(err)
	}
	grandPID, grandGroup := waitReady(t, grandchildReady)
	signalReady(t, os.Getpid(), syscall.Getpgrp(), grandPID, grandGroup)
	select {}
}

func TestRunWorkerProcessReturnsSuccessfulExit(t *testing.T) {
	cmd := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$")
	cmd.Env = replaceEnv(os.Environ(), workerProcessHelperMode, "success")
	if err := runWorkerProcess(context.Background(), cmd, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("worker process state = %v", cmd.ProcessState)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("worker process attributes = %+v", cmd.SysProcAttr)
	}
}

func TestRunWorkerProcessKillsTermIgnoringGroup(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "parent")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$")
	cmd.Env = replaceEnv(os.Environ(), workerProcessHelperMode, "parent")
	cmd.Env = replaceEnv(cmd.Env, "COOPR_TEST_WORKER_READY", ready)

	cancelled := make(chan [4]int, 1)
	go func() {
		pids := waitReadyValues(ready, 3*time.Second)
		cancelled <- pids
		cancel()
	}()
	started := time.Now()
	err := runWorkerProcess(ctx, cmd, 100*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("worker error = %v, want context cancellation", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("worker cancellation took %s", elapsed)
	}

	pids := <-cancelled
	if pids[0] == 0 || pids[2] == 0 {
		t.Fatalf("helper did not become ready: %v", pids)
	}
	if pids[0] != pids[1] || pids[1] != pids[3] {
		t.Fatalf("helper process groups = parent %d/%d grandchild %d/%d", pids[0], pids[1], pids[2], pids[3])
	}
	waitProcessGone(t, pids[0])
	waitProcessGone(t, pids[2])
}

func TestRunWorkerProcessKillsRemainingGroupAfterWorkerExitsOnTerm(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "parent")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$")
	cmd.Env = replaceEnv(os.Environ(), workerProcessHelperMode, "cooperative-parent")
	cmd.Env = replaceEnv(cmd.Env, "COOPR_TEST_WORKER_READY", ready)

	cancelled := make(chan [4]int, 1)
	go func() {
		pids := waitReadyValues(ready, 3*time.Second)
		cancelled <- pids
		cancel()
	}()
	started := time.Now()
	err := runWorkerProcess(ctx, cmd, 2*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("worker error = %v, want context cancellation", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cooperative worker cancellation waited for grace period: %s", elapsed)
	}

	pids := <-cancelled
	if pids[0] == 0 || pids[2] == 0 {
		t.Fatalf("helper did not become ready: %v", pids)
	}
	t.Cleanup(func() { _ = syscall.Kill(-pids[1], syscall.SIGKILL) })
	waitProcessGone(t, pids[0])
	waitProcessGone(t, pids[2])
}

func TestRunWorkerProcessBoundsSignalAndDirectKillFailures(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "parent")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$")
	cmd.Env = replaceEnv(os.Environ(), workerProcessHelperMode, "parent")
	cmd.Env = replaceEnv(cmd.Env, "COOPR_TEST_WORKER_READY", ready)

	cancelled := make(chan [4]int, 1)
	go func() {
		pids := waitReadyValues(ready, 3*time.Second)
		cancelled <- pids
		cancel()
	}()
	directKillCalled := false
	started := time.Now()
	err := runWorkerProcessControlled(ctx, cmd, time.Second, workerProcessControl{
		signalGroup: func(int, syscall.Signal) error { return syscall.EPERM },
		killProcess: func(*os.Process) error {
			directKillCalled = true
			return syscall.EPERM
		},
		killWait: 50 * time.Millisecond,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("worker error = %v, want context cancellation", err)
	}
	if !directKillCalled {
		t.Fatal("direct worker kill was not attempted after group signal failure")
	}
	if !strings.Contains(err.Error(), "timed out waiting for build worker exit") {
		t.Fatalf("worker error = %v, want bounded wait error", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("signal failure path took %s", elapsed)
	}

	pids := <-cancelled
	if pids[0] == 0 || pids[2] == 0 {
		t.Fatalf("helper did not become ready: %v", pids)
	}
	if err := syscall.Kill(-pids[1], syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("clean up helper group: %v", err)
	}
	waitProcessGone(t, pids[0])
	waitProcessGone(t, pids[2])
}

func signalIgnoreTermination() {
	signal.Ignore(syscall.SIGTERM)
}

func signalReady(t *testing.T, pid, group, childPID, childGroup int) {
	t.Helper()
	ready := os.Getenv("COOPR_TEST_WORKER_READY")
	data := fmt.Sprintf("%d %d %d %d", pid, group, childPID, childGroup)
	if err := os.WriteFile(ready, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitReady(t *testing.T, path string) (int, int) {
	t.Helper()
	values := waitReadyValues(path, 3*time.Second)
	if values[0] == 0 {
		t.Fatalf("timed out waiting for helper readiness at %s", path)
	}
	return values[0], values[1]
}

func waitReadyValues(path string, timeout time.Duration) [4]int {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			fields := strings.Fields(string(data))
			if len(fields) == 4 {
				var values [4]int
				valid := true
				for index, field := range fields {
					values[index], err = strconv.Atoi(field)
					valid = valid && err == nil
				}
				if valid {
					return values
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return [4]int{}
}

func waitProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d still exists after worker cancellation", pid)
}

func replaceEnv(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return append(result, prefix+value)
}
