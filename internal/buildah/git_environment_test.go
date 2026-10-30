package buildah

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestGitReexecDoesNotSelectPrivateExecutable(t *testing.T) {
	arguments := []string{"git-reexec-no-dispatch", "selected-executable-ran"}
	if slices.Equal(flag.Args(), arguments) {
		_, _ = fmt.Fprint(os.Stdout, "native-bootstrap-completed")
		return
	}
	echo, err := exec.LookPath("echo")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(testWorkerBinary(t), "-test.run=^TestGitReexecDoesNotSelectPrivateExecutable$", "-test.short=true", "--", arguments[0], arguments[1])
	command.Args[0] = "git"
	command.Env = append(os.Environ(), "COOPR_REAL_GIT_EXECUTABLE="+echo)
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "native-bootstrap-completed") || strings.Contains(string(output), "selected-executable-ran") {
		t.Fatalf("Git argv collision dispatched a private executable: %v: %s", err, output)
	}
}

func TestResolveGitCommitIsolatesHomeConfiguration(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "git")
	if err := os.WriteFile(executable, []byte(`#!/bin/sh
test "$HOME" = /dev/null || exit 1
test "$XDG_CONFIG_HOME" = /dev/null || exit 1
test "$GIT_CONFIG_GLOBAL" = /dev/null || exit 1
test "$GIT_CONFIG_NOSYSTEM" = 1 || exit 1
test "$GIT_ALLOW_PROTOCOL" = git:http:https || exit 1
test -z "$SSH_AUTH_SOCK" || exit 1
printf 'A1B2C3\n'
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", "/ambient-home")
	t.Setenv("XDG_CONFIG_HOME", "/ambient-config")
	t.Setenv("SSH_AUTH_SOCK", "/ambient-agent")
	commit, err := resolveGitCommit(context.Background(), directory)
	if err != nil || commit != "a1b2c3" {
		t.Fatalf("isolated Git commit = %q, %v", commit, err)
	}
}

func TestGitWorkerEnvironmentRemovesHostGitInputs(t *testing.T) {
	environment := gitWorkerEnvironment([]string{
		"HOME=/tmp/user", "SOURCE_DATE_EPOCH=123", "GIT_CONFIG_GLOBAL=/tmp/user/.gitconfig",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=host-helper", "GIT_ASKPASS=/tmp/host-askpass",
		"SSH_ASKPASS=/tmp/host-askpass", "GIT_TERMINAL_PROMPT=1",
		"SSH_AUTH_SOCK=/tmp/ambient-agent", "GIT_ALLOW_PROTOCOL=ssh:ext", "GIT_SSH_COMMAND=ambient-ssh",
	})
	values := map[string]string{}
	for _, entry := range environment {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	if values["HOME"] != "/tmp/user" || values["SOURCE_DATE_EPOCH"] != "123" {
		t.Fatalf("unrelated worker environment changed: %v", values)
	}
	for _, name := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"} {
		if _, exists := values[name]; exists {
			t.Fatalf("host Git configuration %s survived: %v", name, values)
		}
	}
	if values["SSH_AUTH_SOCK"] != "" || values["GIT_ALLOW_PROTOCOL"] != "git:http:https" || values["GIT_SSH_COMMAND"] != "/bin/false" {
		t.Fatalf("worker admitted ambient SSH: %v", values)
	}
	if values["GIT_CONFIG_GLOBAL"] != "/dev/null" || values["GIT_CONFIG_NOSYSTEM"] != "1" ||
		values["GIT_TERMINAL_PROMPT"] != "0" || values["GIT_ASKPASS"] != "/bin/false" ||
		values["SSH_ASKPASS"] != "/bin/false" {
		t.Fatalf("Git worker environment is not isolated: %v", values)
	}
}
