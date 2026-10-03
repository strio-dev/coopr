package buildah

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	upstream "go.podman.io/buildah"
)

func TestNativeBuilderSSHKeySourceProvidesReadOnlyAgent(t *testing.T) {
	privateKey := filepath.Join(t.TempDir(), "id_ed25519")
	runCommand(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", privateKey)
	builder := nativeBuilder{Builder: &upstream.Builder{}, sshSpecs: []string{"default=" + privateKey, "default=" + privateKey}}
	socket, cleanup, found, err := builder.addSourceSSH("default")
	if err != nil {
		t.Fatal(err)
	}
	if !found || socket == "" || cleanup == nil {
		t.Fatalf("SSH source = socket %q, cleanup %v, found %t", socket, cleanup != nil, found)
	}
	t.Cleanup(func() { _ = cleanup() })
	command := exec.Command("ssh-add", "-L")
	command.Env = append(removeEnvironment(os.Environ(), "SSH_AUTH_SOCK"), "SSH_AUTH_SOCK="+socket)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("list forwarded SSH keys: %v\n%s", err, output)
	}
	publicKey, err := os.ReadFile(privateKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(strings.Fields(string(publicKey))[:2], " ")
	if !strings.Contains(string(output), want) {
		t.Fatalf("forwarded keys do not contain %q: %s", want, output)
	}
}

func TestCloneGitAddSourceCancellationKillsGitProcessGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process-group cancellation is Linux-specific")
	}
	directory := t.TempDir()
	executable := filepath.Join(directory, "git")
	childPID := filepath.Join(directory, "child.pid")
	script := "#!/bin/sh\nsleep 30 &\nchild=$!\nprintf '%s' \"$child\" > \"$COOPR_TEST_CHILD_PID\"\nwait \"$child\"\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(realGitExecutable, executable)
	t.Setenv("COOPR_TEST_CHILD_PID", childPID)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := cloneGitAddSource(ctx, &recordingBuilder{}, "git://example.invalid/repository.git", filepath.Join(directory, "repository"), "")
	if err == nil || ctx.Err() == nil {
		t.Fatalf("cancelled Git clone error = %v, context error = %v", err, ctx.Err())
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("cancelled Git clone returned after %s", elapsed)
	}
	contents, err := os.ReadFile(childPID)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(contents))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Git descendant process %d survived cancellation: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runCommand(t *testing.T, name string, arguments ...string) string {
	t.Helper()
	command := exec.Command(name, arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(arguments, " "), err, output)
	}
	return string(output)
}
