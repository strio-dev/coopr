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
	"testing"
	"time"
)

const gitOuterKillHelperMode = "COOPR_TEST_GIT_OUTER_KILL_MODE"

func TestGitOuterKillHelper(t *testing.T) {
	switch os.Getenv(gitOuterKillHelperMode) {
	case "":
		return
	case "outer":
		signalIgnoreTermination()
		command := gitCommandContext(context.Background(), testWorkerBinary(t), "-test.run=^TestGitOuterKillHelper$", "-test.short="+strconv.FormatBool(testing.Short()))
		command.Env = replaceEnv(os.Environ(), gitOuterKillHelperMode, "git")
		if err := command.Run(); err != nil {
			t.Fatal(err)
		}
	case "git":
		signalIgnoreTermination()
		grandchild := exec.Command(testWorkerBinary(t), "-test.run=^TestGitOuterKillHelper$", "-test.short="+strconv.FormatBool(testing.Short()))
		grandchild.Env = replaceEnv(os.Environ(), gitOuterKillHelperMode, "helper")
		if err := grandchild.Start(); err != nil {
			t.Fatal(err)
		}
		ready := os.Getenv("COOPR_TEST_GIT_OUTER_KILL_READY")
		data := fmt.Sprintf("%d %d %d %d", os.Getpid(), syscall.Getpgrp(), grandchild.Process.Pid, processGroup(t, grandchild.Process.Pid))
		if err := os.WriteFile(ready, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		select {}
	case "helper":
		signalIgnoreTermination()
		select {}
	default:
		t.Fatalf("unknown Git outer-kill helper mode %q", os.Getenv(gitOuterKillHelperMode))
	}
}

func TestOuterWorkerKillIncludesGitProcessTree(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.Command(testWorkerBinary(t), "-test.run=^TestGitOuterKillHelper$", "-test.short="+strconv.FormatBool(testing.Short()))
	command.Env = replaceEnv(os.Environ(), gitOuterKillHelperMode, "outer")
	command.Env = replaceEnv(command.Env, "COOPR_TEST_GIT_OUTER_KILL_READY", ready)

	cancelled := make(chan [4]int, 1)
	go func() {
		pids := waitReadyValues(ready, 3*time.Second)
		cancelled <- pids
		cancel()
	}()
	err := runWorkerProcess(ctx, command, 100*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("worker error = %v, want context cancellation", err)
	}

	pids := <-cancelled
	if pids[0] == 0 || pids[2] == 0 {
		t.Fatalf("Git helper did not become ready: %v", pids)
	}
	if pids[1] != pids[3] || pids[1] != command.Process.Pid {
		t.Fatalf("process groups escaped worker boundary: worker %d, Git %d/%d, helper %d/%d", command.Process.Pid, pids[0], pids[1], pids[2], pids[3])
	}
	t.Cleanup(func() { _ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL) })
	waitProcessGone(t, pids[0])
	waitProcessGone(t, pids[2])
}

func processGroup(t *testing.T, pid int) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		t.Fatal(err)
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		t.Fatalf("invalid process stat for %d", pid)
	}
	fields := strings.Fields(string(data)[end+1:])
	if len(fields) < 3 {
		t.Fatalf("short process stat for %d", pid)
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil {
		t.Fatal(err)
	}
	return group
}
