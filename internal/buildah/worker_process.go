package buildah

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"
)

const workerProcessKillWait = 5 * time.Second

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
}

// runWorkerProcess starts cmd in a new process group and waits for it exactly
// once. Cancellation first gives the complete group a chance to clean up, then
// kills the group after grace has elapsed.
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
	if err := ctx.Err(); err != nil {
		return err
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pgid = 0
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start build worker: %w", err)
	}

	waited := make(chan error, 1)
	go func() {
		waited <- cmd.Wait()
	}()

	select {
	case err := <-waited:
		return err
	case <-ctx.Done():
	}

	workerGroup := -cmd.Process.Pid
	if err := control.signalGroup(workerGroup, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return workerSignalFailure(ctx.Err(), cmd.Process, waited, control, "terminate", err)
	}

	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-waited:
		// A cooperative worker can exit while a descendant ignores SIGTERM.
		// Descendants keep the process group alive after its leader is reaped,
		// so close the group before returning cancellation to the caller.
		if err := control.signalGroup(workerGroup, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return errors.Join(ctx.Err(), fmt.Errorf("kill remaining build worker group: %w", err))
		}
		return ctx.Err()
	case <-timer.C:
	}

	if err := control.signalGroup(workerGroup, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return workerSignalFailure(ctx.Err(), cmd.Process, waited, control, "kill", err)
	}
	<-waited
	return ctx.Err()
}

func workerSignalFailure(ctxErr error, process *os.Process, waited <-chan error, control workerProcessControl, action string, signalErr error) error {
	killErr := control.killProcess(process)
	timer := time.NewTimer(control.killWait)
	defer timer.Stop()
	select {
	case <-waited:
		return errors.Join(ctxErr, fmt.Errorf("%s build worker group: %w", action, signalErr), killErr)
	case <-timer.C:
		return errors.Join(ctxErr, fmt.Errorf("%s build worker group: %w", action, signalErr), killErr, errors.New("timed out waiting for build worker exit"))
	}
}
