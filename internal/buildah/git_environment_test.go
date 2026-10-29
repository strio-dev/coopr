package buildah

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitWrapperConsumesLaunchEnvironmentAndAllowsChildGit(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "git-wrapper")
	probe := filepath.Join(t.TempDir(), "git-probe")
	if err := os.WriteFile(probe, []byte(`#!/bin/sh
printf '%s\n' "${COOPR_REAL_GIT_EXECUTABLE+set}" "${COOPR_GIT_WRAPPER_DIRECTORY+set}" "$PATH" "$HOME" "$XDG_CONFIG_HOME" "$GIT_ALLOW_PROTOCOL" "$GIT_CONFIG_GLOBAL"
command -v git
git --version
`), 0o700); err != nil {
		t.Fatal(err)
	}
	// Repeated exact entries are private launch state. Similar paths and empty
	// search entries belong to the caller and must survive unchanged.
	paths := []string{directory, filepath.Dir(realGit), "", directory + "-other", directory, directory + "/"}
	wantPaths := []string{filepath.Dir(realGit), "", directory + "-other", directory + "/"}
	command := exec.Command(testWorkerBinary(t))
	command.Args[0] = "git"
	command.Env = append(gitWorkerEnvironment(os.Environ()),
		realGitExecutable+"="+probe,
		gitWrapperDirectory+"="+directory,
		"PATH="+strings.Join(paths, string(os.PathListSeparator)),
		"HOME=/ambient-home", "XDG_CONFIG_HOME=/ambient-config",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Git wrapper: %v: %s", err, output)
	}
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	want := []string{"", "", strings.Join(wantPaths, string(os.PathListSeparator)), "/dev/null", "/dev/null", "git:http:https", "/dev/null", realGit}
	if len(lines) != len(want)+1 {
		t.Fatalf("Git probe output = %q", output)
	}
	for index, value := range want {
		if lines[index] != value {
			t.Errorf("Git probe line %d = %q, want %q", index, lines[index], value)
		}
	}
	if !strings.HasPrefix(lines[len(want)], "git version ") {
		t.Fatalf("child Git did not run: %q", lines[len(want)])
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
