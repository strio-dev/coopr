package buildah

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	gitWrapperDirectory = "COOPR_GIT_WRAPPER_DIRECTORY"
	realGitExecutable   = "COOPR_REAL_GIT_EXECUTABLE"
)

// gitWorkerEnvironment prevents a build's remote Git inputs from depending on
// the host's Git configuration, credential helpers, or interactive prompts.
// Workers have their own process environment, so this never mutates the
// caller's environment while other stages run.
func gitWorkerEnvironment(parent []string) []string {
	environment := make([]string, 0, len(parent)+4)
	for _, entry := range parent {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "GIT_") || name == "SSH_ASKPASS" {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ASKPASS=/bin/false",
		"SSH_ASKPASS=/bin/false",
	)
}

// prepareGitWorkerEnvironment places this same binary ahead of Git on PATH.
// Buildah invokes Git without an environment hook; the reexec wrapper gives
// only that Git subprocess an empty HOME while registry resolution in the
// worker retains its normal credential search paths.
func prepareGitWorkerEnvironment(parent []string, jobDir string) ([]string, error) {
	filtered := make([]string, 0, len(parent))
	for _, entry := range parent {
		name, _, _ := strings.Cut(entry, "=")
		if name != gitWrapperDirectory && name != realGitExecutable {
			filtered = append(filtered, entry)
		}
	}
	environment := gitWorkerEnvironment(filtered)
	real, err := exec.LookPath("git")
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, exec.ErrNotFound) {
			return environment, nil
		}
		return nil, fmt.Errorf("find Git executable: %w", err)
	}
	directory := filepath.Join(jobDir, "git-wrapper")
	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create Git wrapper directory: %w", err)
	}
	// Rootless Buildah may reexec Coopr from a memfd, which has no stable
	// filesystem pathname. procfs resolves this to the current worker binary
	// both during PATH lookup and when the Git subprocess is executed.
	if err := os.Symlink("/proc/self/exe", filepath.Join(directory, "git")); err != nil {
		return nil, fmt.Errorf("create Git wrapper: %w", err)
	}
	path := os.Getenv("PATH")
	return append(environment,
		"PATH="+directory+string(os.PathListSeparator)+path,
		gitWrapperDirectory+"="+directory,
		realGitExecutable+"="+real,
	), nil
}

// runGitWrapper is registered with reexec under argv[0] == "git". Git's
// children inherit the empty HOME too, including its HTTP transport.
func runGitWrapper() {
	real := os.Getenv(realGitExecutable)
	if !filepath.IsAbs(real) {
		_, _ = fmt.Fprintln(os.Stderr, "coopr Git wrapper has no absolute Git executable")
		os.Exit(127)
	}
	environment := gitWorkerEnvironment(os.Environ())
	isolated := make([]string, 0, len(environment)+2)
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if name != "HOME" && name != "XDG_CONFIG_HOME" {
			isolated = append(isolated, entry)
		}
	}
	environment = append(isolated, "HOME=/dev/null", "XDG_CONFIG_HOME=/dev/null")
	if err := syscall.Exec(real, append([]string{real}, os.Args[1:]...), environment); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "coopr Git wrapper: %v\n", err)
		os.Exit(127)
	}
}
