package buildah

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.podman.io/storage/pkg/reexec"
	"golang.org/x/sys/unix"
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
		select {}
	case "cooperative-parent":
		startWorkerProcessGrandchild(t)
		select {}
	case "exiting-parent", "detached-exiting-parent":
		startWorkerProcessGrandchild(t)
	case "failing-parent":
		startWorkerProcessGrandchild(t)
		t.Fatal("requested worker failure")
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
	grandchild := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$", "-test.short="+strconv.FormatBool(testing.Short()))
	grandchild.Stdout = os.Stdout
	grandchild.Stderr = os.Stderr
	if os.Getenv(workerProcessHelperMode) == "detached-exiting-parent" {
		grandchild.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	grandchild.Env = replaceEnv(os.Environ(), workerProcessHelperMode, "grandchild")
	grandchildReady := os.Getenv("COOPR_TEST_WORKER_READY") + ".grandchild"
	grandchild.Env = replaceEnv(grandchild.Env, "COOPR_TEST_WORKER_READY", grandchildReady)
	if err := grandchild.Start(); err != nil {
		t.Fatal(err)
	}
	grandPID, grandGroup := waitReady(t, grandchildReady)
	signalReady(t, os.Getpid(), syscall.Getpgrp(), grandPID, grandGroup)
}

func TestRunWorkerProcessReturnsSuccessfulExit(t *testing.T) {
	cmd := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$", "-test.short="+strconv.FormatBool(testing.Short()))
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

func TestRunWorkerProcessStopsDescendantsWhenLeaderExits(t *testing.T) {
	for _, mode := range []string{"exiting-parent", "failing-parent"} {
		for _, captured := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/captured=%t", mode, captured), func(t *testing.T) {
				testWorkerLeaderExit(t, mode, captured)
			})
		}
	}
}

func testWorkerLeaderExit(t *testing.T, mode string, captured bool) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "parent")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$", "-test.short="+strconv.FormatBool(testing.Short()))
	cmd.Env = replaceEnv(os.Environ(), workerProcessHelperMode, mode)
	cmd.Env = replaceEnv(cmd.Env, "COOPR_TEST_WORKER_READY", ready)
	if captured {
		cmd.Stdout = new(bytes.Buffer)
		cmd.Stderr = new(bytes.Buffer)
	}
	cleaned := false
	t.Cleanup(func() {
		if !cleaned && cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	err := runWorkerProcess(ctx, cmd, time.Second)
	if mode == "failing-parent" {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
			t.Fatalf("worker error = %v, want original exit status 1", err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	pids := waitReadyValues(ready, time.Second)
	if pids[0] == 0 || pids[2] == 0 {
		t.Fatalf("helper did not become ready: %v", pids)
	}
	if pids[0] != pids[1] || pids[1] != pids[3] {
		t.Fatalf("helper process groups = parent %d/%d grandchild %d/%d", pids[0], pids[1], pids[2], pids[3])
	}
	waitProcessGone(t, pids[0])
	waitProcessGone(t, pids[2])
	cleaned = true
}

func TestRunWorkerProcessKillsTermIgnoringGroup(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "parent")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$", "-test.short="+strconv.FormatBool(testing.Short()))
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

func TestRunWorkerProcessReportsRemainingGroupCleanupErrors(t *testing.T) {
	for _, signalErr := range []error{nil, syscall.ESRCH, syscall.EPERM} {
		t.Run(fmt.Sprint(signalErr), func(t *testing.T) {
			cmd := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$", "-test.short="+strconv.FormatBool(testing.Short()))
			cmd.Env = replaceEnv(os.Environ(), workerProcessHelperMode, "success")
			called := false
			err := runWorkerProcessControlled(context.Background(), cmd, time.Second, workerProcessControl{
				signalGroup: func(group int, signal syscall.Signal) error {
					called = true
					if group != -cmd.Process.Pid || signal != syscall.SIGKILL {
						t.Errorf("group signal = %d/%v, want %d/SIGKILL", group, signal, -cmd.Process.Pid)
					}
					return signalErr
				},
				killProcess: func(process *os.Process) error { return process.Kill() },
				killWait:    time.Second,
			})
			if !called {
				t.Fatal("worker completion did not close its process group")
			}
			if errors.Is(signalErr, syscall.EPERM) {
				if !errors.Is(err, syscall.EPERM) || !errors.Is(err, ErrWorkerWaitIncomplete) || !strings.Contains(err.Error(), "kill remaining build worker group") {
					t.Fatalf("cleanup error = %v, want group signal failure", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunWorkerProcessRetriesInterruptedExitObservation(t *testing.T) {
	cmd := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$", "-test.short="+strconv.FormatBool(testing.Short()))
	cmd.Env = replaceEnv(os.Environ(), workerProcessHelperMode, "success")
	var observations atomic.Int32
	err := runWorkerProcessControlled(context.Background(), cmd, time.Second, workerProcessControl{
		signalGroup: syscall.Kill,
		killProcess: func(process *os.Process) error { return process.Kill() },
		killWait:    time.Second,
		waitExit: func(pid int) error {
			if observations.Add(1) == 1 {
				return syscall.EINTR
			}
			return waitWorkerExit(pid)
		},
	})
	if err != nil || observations.Load() != 2 || cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("interrupted observation result = %v, attempts=%d, state=%v", err, observations.Load(), cmd.ProcessState)
	}
}

func TestRunWorkerProcessObservationFailureDoesNotSignalGroup(t *testing.T) {
	for _, observeErr := range []error{syscall.ECHILD, syscall.EPERM} {
		t.Run(observeErr.Error(), func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "worker")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$", "-test.short="+strconv.FormatBool(testing.Short()))
			mode, wantKills := "grandchild", int32(1)
			if errors.Is(observeErr, syscall.ECHILD) {
				mode, wantKills = "success", 0
			}
			cmd.Env = replaceEnv(os.Environ(), workerProcessHelperMode, mode)
			cmd.Env = replaceEnv(cmd.Env, "COOPR_TEST_WORKER_READY", ready)
			t.Cleanup(func() {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
			})
			var groupSignals, processKills atomic.Int32
			err := runWorkerProcessControlled(ctx, cmd, time.Second, workerProcessControl{
				signalGroup: func(int, syscall.Signal) error {
					groupSignals.Add(1)
					return syscall.EPERM
				},
				killProcess: func(process *os.Process) error {
					processKills.Add(1)
					return process.Kill()
				},
				killWait: time.Second,
				waitExit: func(int) error {
					if errors.Is(observeErr, syscall.ECHILD) {
						if _, err := cmd.Process.Wait(); err != nil {
							return err
						}
						return syscall.ECHILD
					}
					if waitReadyValues(ready, 3*time.Second)[0] == 0 {
						return errors.New("worker did not become ready")
					}
					return observeErr
				},
			})
			if !errors.Is(err, observeErr) || !errors.Is(err, ErrWorkerWaitIncomplete) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("observation error = %v, want %v before context deadline", err, observeErr)
			}
			if groupSignals.Load() != 0 || processKills.Load() != wantKills {
				t.Fatalf("unconfirmed exit signals = group %d, process %d", groupSignals.Load(), processKills.Load())
			}
			waitProcessGone(t, cmd.Process.Pid)
		})
	}
}

func TestRunWorkerProcessSignalsBeforeReaping(t *testing.T) {
	cmd := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$", "-test.short="+strconv.FormatBool(testing.Short()))
	cmd.Env = replaceEnv(os.Environ(), workerProcessHelperMode, "success")
	var signals atomic.Int32
	err := runWorkerProcessControlled(context.Background(), cmd, time.Second, workerProcessControl{
		signalGroup: func(group int, signal syscall.Signal) error {
			var info unix.Siginfo
			if err := unix.Waitid(unix.P_PID, -group, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil); err != nil {
				return fmt.Errorf("group leader was reaped before signal: %w", err)
			}
			if info.Signo != int32(syscall.SIGCHLD) {
				return fmt.Errorf("group leader was not observed exited: signal=%d", info.Signo)
			}
			signals.Add(1)
			return syscall.Kill(group, signal)
		},
		killProcess: func(process *os.Process) error { return process.Kill() },
		killWait:    time.Second,
	})
	if err != nil || signals.Load() != 1 {
		t.Fatalf("pre-reap group cleanup = %v, signals=%d", err, signals.Load())
	}
}

func TestRunWorkerProcessCancellationAfterObservedExit(t *testing.T) {
	for _, mode := range []string{"success", "failing-parent"} {
		t.Run(mode, func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "parent")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$", "-test.short="+strconv.FormatBool(testing.Short()))
			cmd.Env = replaceEnv(os.Environ(), workerProcessHelperMode, mode)
			cmd.Env = replaceEnv(cmd.Env, "COOPR_TEST_WORKER_READY", ready)
			cmd.Stdout = new(bytes.Buffer)
			cmd.Stderr = new(bytes.Buffer)
			cleaned := false
			t.Cleanup(func() {
				if !cleaned && mode == "failing-parent" && cmd.Process != nil {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				}
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
			})
			err := runWorkerProcessControlled(ctx, cmd, time.Second, workerProcessControl{
				signalGroup: syscall.Kill,
				killProcess: func(process *os.Process) error { return process.Kill() },
				killWait:    time.Second,
				waitExit: func(pid int) error {
					err := waitWorkerExit(pid)
					cancel()
					return err
				},
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("observed-exit cancellation = %v", err)
			}
			if mode == "failing-parent" {
				var status *exec.ExitError
				if !errors.As(err, &status) || status.ExitCode() != 1 {
					t.Fatalf("observed-exit failure = %v, want original exit status 1", err)
				}
				pids := waitReadyValues(ready, time.Second)
				if pids[2] == 0 {
					t.Fatalf("worker child was not ready: %v", pids)
				}
				waitProcessGone(t, pids[2])
			}
			cleaned = true
		})
	}
}

func TestRunWorkerProcessReapsAfterWaitTimeout(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "worker")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$", "-test.short="+strconv.FormatBool(testing.Short()))
	cmd.Env = replaceEnv(os.Environ(), workerProcessHelperMode, "grandchild")
	cmd.Env = replaceEnv(cmd.Env, "COOPR_TEST_WORKER_READY", ready)
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	readyValues := make(chan [4]int, 1)
	go func() {
		readyValues <- waitReadyValues(ready, 3*time.Second)
		cancel()
	}()
	err := runWorkerProcessControlled(ctx, cmd, time.Second, workerProcessControl{
		signalGroup: func(int, syscall.Signal) error { return syscall.EPERM },
		killProcess: func(*os.Process) error { return syscall.EPERM },
		killWait:    50 * time.Millisecond,
	})
	if !errors.Is(err, ErrWorkerWaitIncomplete) || !errors.Is(err, context.Canceled) {
		t.Fatalf("incomplete worker result = %v", err)
	}
	if stageWorkerExitConfirmed(cmd, err) {
		t.Fatal("timed-out worker was treated as reaped")
	}
	pids := <-readyValues
	if pids[0] == 0 {
		t.Fatalf("worker did not become ready: %v", pids)
	}
	if killErr := cmd.Process.Kill(); killErr != nil {
		t.Fatal(killErr)
	}
	if stageWorkerExitConfirmed(cmd, err) {
		t.Fatal("incomplete result changed while the wait pipeline was still finishing")
	}
	waitProcessGone(t, pids[0])
}

func TestRunWorkerProcessBoundsEscapedOutputPipes(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "parent")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$", "-test.short="+strconv.FormatBool(testing.Short()))
	cmd.Env = replaceEnv(os.Environ(), workerProcessHelperMode, "detached-exiting-parent")
	cmd.Env = replaceEnv(cmd.Env, "COOPR_TEST_WORKER_READY", ready)
	cmd.Stdout = new(bytes.Buffer)
	cmd.Stderr = new(bytes.Buffer)
	cmd.WaitDelay = 50 * time.Millisecond
	t.Cleanup(func() {
		pids := waitReadyValues(ready, time.Second)
		if pids[2] != 0 {
			if process, err := os.FindProcess(pids[2]); err == nil {
				_ = process.Kill()
				_ = process.Release()
			}
			waitProcessGone(t, pids[2])
		}
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	err := runWorkerProcess(ctx, cmd, time.Second)
	if !errors.Is(err, exec.ErrWaitDelay) || ctx.Err() != nil {
		t.Fatalf("escaped output-pipe result = %v, context=%v", err, ctx.Err())
	}
	pids := waitReadyValues(ready, time.Second)
	if pids[0] == 0 || pids[2] == 0 || pids[1] == pids[3] {
		t.Fatalf("helper did not create an independent session: %v", pids)
	}
}

func TestRunWorkerProcessKillsRemainingGroupAfterWorkerExitsOnTerm(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "parent")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$", "-test.short="+strconv.FormatBool(testing.Short()))
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
	cmd := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$", "-test.short="+strconv.FormatBool(testing.Short()))
	cmd.Env = replaceEnv(os.Environ(), workerProcessHelperMode, "parent")
	cmd.Env = replaceEnv(cmd.Env, "COOPR_TEST_WORKER_READY", ready)

	cancelled := make(chan [4]int, 1)
	cancelledAt := make(chan time.Time, 1)
	go func() {
		pids := waitReadyValues(ready, 3*time.Second)
		cancelled <- pids
		cancelledAt <- time.Now()
		cancel()
	}()
	directKillCalled := false
	err := runWorkerProcessControlled(ctx, cmd, time.Second, workerProcessControl{
		signalGroup: func(int, syscall.Signal) error { return syscall.EPERM },
		killProcess: func(*os.Process) error {
			directKillCalled = true
			return syscall.EPERM
		},
		killWait: 50 * time.Millisecond,
	})
	returnedAt := time.Now()
	pids := <-cancelled
	started := <-cancelledAt
	t.Cleanup(func() {
		if cmd.Process != nil {
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				t.Errorf("clean up helper group: %v", err)
			}
			waitProcessGone(t, cmd.Process.Pid)
		}
		if pids[2] > 0 {
			waitProcessGone(t, pids[2])
		}
	})
	if pids[0] == 0 || pids[2] == 0 {
		t.Fatalf("helper did not become ready: %v", pids)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("worker error = %v, want context cancellation", err)
	}
	if !directKillCalled {
		t.Fatal("direct worker kill was not attempted after group signal failure")
	}
	if !strings.Contains(err.Error(), "timed out waiting for build worker exit") {
		t.Fatalf("worker error = %v, want bounded wait error", err)
	}
	if elapsed := returnedAt.Sub(started); elapsed > time.Second {
		t.Fatalf("signal failure path took %s", elapsed)
	}
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
