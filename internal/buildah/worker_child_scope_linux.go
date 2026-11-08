//go:build linux

package buildah

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	workerChildDrainTimeout = 2 * time.Second
	workerChildGrace        = 100 * time.Millisecond
)

// workerChildScope belongs only to a dedicated, final-namespace worker. It is
// deliberately absent from runWorkerProcess and the embedding/API process.
// Once managed commands and resource cleanup have finished, the subreaper owns
// detached runtime helpers that would otherwise be reparented outside the job.
type workerChildScope struct{}

func newWorkerChildScope() (*workerChildScope, error) {
	if _, err := os.Stat("/proc/self/task"); err != nil {
		return nil, fmt.Errorf("inspect worker child ownership: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return nil, fmt.Errorf("establish worker child ownership: %w", err)
	}
	return &workerChildScope{}, nil
}

func (scope *workerChildScope) finish(operationErr error) error {
	if scope == nil {
		return errors.Join(ErrWorkerWaitIncomplete, errors.New("worker child ownership was not established"))
	}
	// A timed-out managed waiter still owns its child's status and output pipes.
	// Do not compete with Cmd.Wait, even if that means reporting incomplete cleanup.
	if errors.Is(operationErr, errManagedWorkerWaitPending) {
		return errors.Join(ErrWorkerWaitIncomplete, errors.New("managed worker wait still owns child status"))
	}
	deadline := time.Now().Add(workerChildDrainTimeout)
	for {
		if _, err := os.Stat("/proc/self/task"); err != nil {
			return childScopeFailure(err)
		}
		children, err := linuxProcessChildren(os.Getpid())
		if err != nil {
			return childScopeFailure(err)
		}
		if len(children) == 0 {
			return nil
		}
		owned := make([]int, 0, len(children))
		for _, pid := range children {
			info, err := observeOwnedWorkerChild(pid, deadline)
			if errors.Is(err, syscall.ECHILD) {
				continue
			} // Stale enumeration grants no authority.
			if err != nil {
				return childScopeFailure(err)
			}
			owned = append(owned, pid)
			if info.Signo == 0 {
				if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
					return childScopeFailure(fmt.Errorf("terminate owned child %d: %w", pid, err))
				}
			}
		}
		// One TERM wave and one shared grace, rather than a grace per process.
		grace := time.Now().Add(workerChildGrace)
		if deadline.Before(grace) {
			grace = deadline
		}
		for _, pid := range owned {
			if err := drainOwnedWorkerChild(pid, grace, deadline); err != nil {
				return childScopeFailure(err)
			}
		}
		// Killing a child can adopt its own children. Enumerate again after each
		// generation; neither a process group nor a one-time snapshot covers setsid.
		if !time.Now().Before(deadline) {
			return childScopeFailure(errors.New("timed out draining worker descendants"))
		}
	}
}

func childScopeFailure(err error) error {
	return errors.Join(ErrWorkerWaitIncomplete, fmt.Errorf("drain worker descendants: %w", err))
}

// observeOwnedWorkerChild leaves exit status unreaped. With all managed waiters
// complete, successful waitid reserves this child's numeric identity until our
// exact Wait4; neither UID nor process names or process groups grant authority.
func observeOwnedWorkerChild(pid int, deadline time.Time) (unix.Siginfo, error) {
	var info unix.Siginfo
	for {
		if !time.Now().Before(deadline) {
			return info, fmt.Errorf("timed out verifying owned child %d", pid)
		}
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return info, err
	}
}

func drainOwnedWorkerChild(pid int, grace, deadline time.Time) error {
	info, err := observeOwnedWorkerChild(pid, deadline)
	if err != nil {
		return fmt.Errorf("observe terminating owned child %d: %w", pid, err)
	}
	for info.Signo == 0 && time.Now().Before(grace) {
		time.Sleep(time.Millisecond)
		info, err = observeOwnedWorkerChild(pid, deadline)
		if err != nil {
			return fmt.Errorf("observe terminating owned child %d: %w", pid, err)
		}
	}
	if info.Signo == 0 {
		// Individual child only: an inherited/shared process group is not owned.
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("stop owned child %d: %w", pid, err)
		}
	}
	for {
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timed out waiting for owned child %d", pid)
		}
		reaped, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reap owned child %d: %w", pid, err)
		}
		if reaped == pid {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
}
