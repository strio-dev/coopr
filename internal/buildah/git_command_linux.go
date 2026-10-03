//go:build linux

package buildah

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func gitCommandContext(ctx context.Context, executable string, arguments ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, executable, arguments...)
	// Keep Git and its helpers in the worker's process group so the supervising
	// parent can always terminate the complete build. For cancellation scoped to
	// this command, freeze and kill Git's Linux process tree instead of creating
	// a nested process group that would escape the worker's hard-kill boundary.
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return killGitProcessTree(command.Process.Pid)
	}
	command.WaitDelay = 2 * time.Second
	return command
}

func killGitProcessTree(root int) error {
	seen := map[int]struct{}{}
	pending := []int{root}
	order := make([]int, 0, 4)
	var result error
	for len(pending) > 0 {
		pid := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if _, found := seen[pid]; found {
			continue
		}
		seen[pid] = struct{}{}
		order = append(order, pid)
		if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
			if !errors.Is(err, syscall.ESRCH) {
				result = errors.Join(result, fmt.Errorf("stop Git process %d: %w", pid, err))
			}
			continue
		}
		children, err := linuxProcessChildren(pid)
		if err != nil {
			result = errors.Join(result, fmt.Errorf("inspect Git process %d children: %w", pid, err))
		}
		pending = append(pending, children...)
	}
	for index := len(order) - 1; index >= 0; index-- {
		pid := order[index]
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			result = errors.Join(result, fmt.Errorf("kill Git process %d: %w", pid, err))
		}
	}
	return result
}

func linuxProcessChildren(pid int) ([]int, error) {
	tasks, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "task"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	children := make([]int, 0)
	seen := map[int]struct{}{}
	for _, task := range tasks {
		data, readErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "task", task.Name(), "children"))
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			return nil, readErr
		}
		for _, field := range strings.Fields(string(data)) {
			child, parseErr := strconv.Atoi(field)
			if parseErr != nil {
				return nil, fmt.Errorf("parse child PID %q: %w", field, parseErr)
			}
			if _, found := seen[child]; !found {
				seen[child] = struct{}{}
				children = append(children, child)
			}
		}
	}
	return children, nil
}
