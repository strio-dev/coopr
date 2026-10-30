package buildah

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const workerProcessKillWait = 5 * time.Second

// ErrWorkerWaitIncomplete reports that worker completion has not been confirmed.
var ErrWorkerWaitIncomplete = errors.New("build worker wait did not complete")

// Workers receive signing passphrases only through an explicit private file;
// never inherit the CLI's private-key password through the environment.
func withoutSigningPassword(environment []string) []string {
	return slices.DeleteFunc(slices.Clone(environment), func(entry string) bool {
		return strings.HasPrefix(entry, "COSIGN_PASSWORD=")
	})
}

type workerProcessControl struct {
	signalGroup func(int, syscall.Signal) error
	killProcess func(*os.Process) error
	killWait    time.Duration
	waitExit    func(int) error
}

// runWorkerProcess starts cmd in a new process group and waits for it exactly
// once. Cancellation first gives the complete group a chance to clean up, then
// kills the group after grace has elapsed. A leader's exit also closes its group:
// no worker transfers a running descendant to the caller.
func runWorkerProcess(ctx context.Context, cmd *exec.Cmd, grace time.Duration) error {
	return runWorkerProcessControlled(ctx, cmd, grace, workerProcessControl{
		signalGroup: syscall.Kill,
		killProcess: func(process *os.Process) error { return process.Kill() },
		killWait:    workerProcessKillWait,
	})
}

func runWorkerProcessControlled(ctx context.Context, cmd *exec.Cmd, grace time.Duration, control workerProcessControl) error {
	if ctx == nil {
		return errors.New("worker context is nil")
	}
	if cmd == nil {
		return errors.New("worker command is nil")
	}
	if grace < 0 {
		return errors.New("worker termination grace period is negative")
	}
	if control.signalGroup == nil || control.killProcess == nil || control.killWait <= 0 {
		return errors.New("worker process control is invalid")
	}
	if control.waitExit == nil {
		control.waitExit = waitWorkerExit
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pgid = 0
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = control.killWait
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start build worker: %w", err)
	}

	// Observe without reaping, as os.Process does on Linux. The unreaped leader
	// retains the group identity until its final signal; Cmd.Wait then owns the
	// sole reap and output drain. Serialize cancellation with that transition.
	var groupMu sync.Mutex
	groupOpen := true
	nativeControl := control
	control.signalGroup = func(group int, signal syscall.Signal) error {
		groupMu.Lock()
		defer groupMu.Unlock()
		if !groupOpen {
			return syscall.ESRCH
		}
		return nativeControl.signalGroup(group, signal)
	}
	waited := make(chan error, 1)
	observationFailed := make(chan error, 1)
	go func() {
		var observeErr error
		for {
			observeErr = nativeControl.waitExit(cmd.Process.Pid)
			if !errors.Is(observeErr, syscall.EINTR) {
				break
			}
		}
		groupMu.Lock()
		var cleanupErr error
		if observeErr == nil {
			cleanupErr = nativeControl.killRemainingGroup(cmd.Process.Pid)
			if cleanupErr != nil {
				cleanupErr = errors.Join(ErrWorkerWaitIncomplete, cleanupErr)
			}
		}
		groupOpen = false
		groupMu.Unlock()
		if observeErr != nil {
			observeErr = errors.Join(ErrWorkerWaitIncomplete, fmt.Errorf("observe build worker exit: %w", observeErr))
			observationFailed <- observeErr
		}
		waitErr := cmd.Wait()
		if cmd.ProcessState == nil {
			waitErr = errors.Join(ErrWorkerWaitIncomplete, waitErr)
		}
		if observeErr == nil && cleanupErr == nil {
			waited <- waitErr
		} else {
			waited <- errors.Join(observeErr, cleanupErr, waitErr)
		}
	}()

	select {
	case err := <-waited:
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Join(ctxErr, err)
		}
		return err
	case err := <-observationFailed:
		return workerSignalFailure(ctx.Err(), cmd.Process, waited, control, "observe", err)
	case <-ctx.Done():
	}

	workerGroup := -cmd.Process.Pid
	if err := control.signalGroup(workerGroup, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return workerSignalFailure(ctx.Err(), cmd.Process, waited, control, "terminate", err)
	}

	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-waited:
		return errors.Join(ctx.Err(), err)
	case err := <-observationFailed:
		return workerSignalFailure(ctx.Err(), cmd.Process, waited, control, "observe", err)
	case <-timer.C:
	}

	if err := control.signalGroup(workerGroup, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return workerSignalFailure(ctx.Err(), cmd.Process, waited, control, "kill", err)
	}
	return errors.Join(ctx.Err(), awaitWorkerProcess(waited, control.killWait))
}

func waitWorkerExit(pid int) error {
	var info unix.Siginfo
	return unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
}

func (control workerProcessControl) killRemainingGroup(pid int) error {
	if err := control.signalGroup(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill remaining build worker group: %w", err)
	}
	return nil
}

func workerSignalFailure(ctxErr error, process *os.Process, waited <-chan error, control workerProcessControl, action string, signalErr error) error {
	var killErr error
	if !errors.Is(signalErr, syscall.ECHILD) {
		killErr = control.killProcess(process)
	}
	return errors.Join(ctxErr, fmt.Errorf("%s build worker group: %w", action, signalErr), killErr, awaitWorkerProcess(waited, control.killWait))
}

func awaitWorkerProcess(waited <-chan error, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-waited:
		return err
	case <-timer.C:
		// The wait pipeline retains ownership and will reap a later exit.
		return errors.Join(ErrWorkerWaitIncomplete, errors.New("timed out waiting for build worker exit"))
	}
}

// RemoveWorkerDirectory retains a private worker directory when supervision
// could not confirm completion. Callers must not remove files still in use.
func RemoveWorkerDirectory(directory string, workerErr error) error {
	if errors.Is(workerErr, ErrWorkerWaitIncomplete) {
		return fmt.Errorf("worker files retained at %s: %w", directory, workerErr)
	}
	return errors.Join(workerErr, os.RemoveAll(directory))
}
