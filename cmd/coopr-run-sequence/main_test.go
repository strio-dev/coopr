package main

import (
	"bytes"
	"context"
	"encoding/json"
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

var helper string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "coopr-sequence-test-")
	if err != nil {
		panic(err)
	}
	helper = filepath.Join(dir, "coopr-run-sequence")
	build := exec.Command("go", "build", "-o", helper, ".")
	if output, err := build.CombinedOutput(); err != nil {
		panic(string(output) + err.Error())
	}
	status := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(status)
}

func commandFile(t *testing.T, commands [][]string) string {
	t.Helper()
	data, err := json.Marshal(commands)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "commands.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func invoke(t *testing.T, commands [][]string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return exec.CommandContext(ctx, helper, commandFile(t, commands))
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if e, ok := err.(*exec.ExitError); ok {
		return e.ExitCode()
	}
	return -1
}

func TestSequenceInheritsProcessStateAndPreservesArgv(t *testing.T) {
	dir := t.TempDir()
	commands := [][]string{
		{"/bin/sh", "-c", "printf '%s' \"$1\" > first", "sh", "literal $VALUE; with spaces"},
		{"/bin/sh", "-c", "cat first; printf '|%s|' \"$VALUE\"; cat; pwd"},
	}
	command := invoke(t, commands)
	command.Dir = dir
	command.Env = append(os.Environ(), "VALUE=inherited")
	command.Stdin = strings.NewReader("input|")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run: %v: %s", err, output)
	}
	want := "literal $VALUE; with spaces|inherited|input|" + dir + "\n"
	if string(output) != want {
		t.Fatalf("output = %q, want %q", output, want)
	}
}

func TestSequenceStopsAtFirstFailure(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "later")
	for _, first := range [][]string{{"/bin/sh", "-c", "exit 37"}, {"/bin/sh", "-c", "kill -TERM $$"}} {
		command := invoke(t, [][]string{first, {"/bin/sh", "-c", "touch \"$1\"", "sh", marker}})
		err := command.Run()
		want := 37
		if first[2] == "kill -TERM $$" {
			want = 143
		}
		if got := exitCode(err); got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("later command ran: %v", err)
		}
	}
}

func TestExecErrorStatuses(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(file, []byte("#!/bin/sh\nexit 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		executable string
		status     int
	}{
		{filepath.Join(t.TempDir(), "missing"), 127}, {file, 126},
	} {
		command := invoke(t, [][]string{{test.executable}, {"/bin/true"}})
		output, err := command.CombinedOutput()
		if got := exitCode(err); got != test.status {
			t.Fatalf("%s status = %d, want %d: %s", test.executable, got, test.status, output)
		}
	}
}

func TestValidateWholeSequenceBeforeEffects(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "effect")
	for _, invalid := range [][]string{nil, {""}, {"/bin/echo", "nul\x00argument"}} {
		command := invoke(t, [][]string{{"/bin/sh", "-c", "touch \"$1\"", "sh", marker}, invalid})
		if got := exitCode(command.Run()); got != 125 {
			t.Fatalf("status = %d, want 125", got)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("validation allowed effects: %v", err)
		}
	}
	command := invoke(t, [][]string{{"/bin/true"}})
	if got := exitCode(command.Run()); got != 125 {
		t.Fatalf("single command status = %d", got)
	}
}

func TestExplicitCurrentDirectoryPATH(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "say"), []byte("#!/bin/sh\nprintf local\n"), 0700); err != nil {
		t.Fatal(err)
	}
	command := invoke(t, [][]string{{"say"}, {"say"}})
	command.Dir = dir
	command.Env = append(os.Environ(), "PATH=.")
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "locallocal" {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestSignalAbortsSequenceEvenWhenChildHandlesIt(t *testing.T) {
	dir := t.TempDir()
	ready, later := filepath.Join(dir, "ready"), filepath.Join(dir, "later")
	command := invoke(t, [][]string{
		{"/bin/sh", "-c", "trap 'exit 0' TERM; touch \"$1\"; while :; do sleep 1; done", "sh", ready},
		{"/bin/sh", "-c", "touch \"$1\"", "sh", later},
	})
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if got := exitCode(command.Wait()); got != 143 {
		t.Fatalf("status = %d, want 143: %s", got, output.String())
	}
	if _, err := os.Stat(later); !os.IsNotExist(err) {
		t.Fatalf("later command ran after signal: %v", err)
	}
}

func TestInvalidCommandFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "commands.json")
	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {path, path}, {path}, {path + ".missing"}} {
		command := exec.Command(helper, args...)
		if output, err := command.CombinedOutput(); exitCode(err) != 125 {
			t.Fatalf("args %q: status %d: %s", args, exitCode(err), output)
		}
	}
}

func TestSuccessfulCommandMayLeaveBackgroundProcess(t *testing.T) {
	dir := t.TempDir()
	command := invoke(t, [][]string{
		{"/bin/sh", "-c", "sleep 30 & printf '%s' \"$!\" > background"},
		{"/bin/sh", "-c", "pid=$(cat background); kill -0 \"$pid\" && kill \"$pid\""},
	})
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("background process did not survive command boundary: %v: %s", err, output)
	}
}

func TestAbruptHelperDeathKillsDirectChild(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	command := invoke(t, [][]string{
		{"/bin/sh", "-c", "echo $$ > \"$1\"; exec sleep 30", "sh", ready},
		{"/bin/true"},
	})
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill() }()
	pids := awaitPIDs(t, ready)
	defer func() { _ = syscall.Kill(pids[0], syscall.SIGKILL) }()
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	awaitStopped(t, pids)
}

func TestChildrenRemainInSupervisorCleanupGroup(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	command := invoke(t, [][]string{
		{"/bin/sh", "-c", "sleep 30 & echo \"$$ $!\" > \"$1\"; wait", "sh", ready},
		{"/bin/true"},
	})
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }()
	pids := awaitPIDs(t, ready)
	for _, pid := range pids {
		defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
	}
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	awaitStopped(t, pids)
}

func awaitPIDs(t *testing.T, path string) []int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(strings.Fields(string(data))) > 0 {
			var pids []int
			for _, field := range strings.Fields(string(data)) {
				pid, err := strconv.Atoi(field)
				if err != nil {
					t.Fatal(err)
				}
				pids = append(pids, pid)
			}
			return pids
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child PID file did not become ready")
	return nil
}

func awaitStopped(t *testing.T, pids []int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		alive := false
		for _, pid := range pids {
			data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			// A process reaped after ReadFile opens stat makes the read return ESRCH.
			if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			// An exited orphan can remain a zombie until the host init reaps it.
			fields := strings.Fields(string(data)[strings.LastIndex(string(data), ")")+1:])
			if len(fields) == 0 || fields[0] != "Z" {
				alive = true
			}
		}
		if !alive {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("processes survived cleanup: %v", pids)
}
