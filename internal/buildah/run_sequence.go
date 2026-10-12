package buildah

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/opencontainers/runtime-spec/specs-go"
)

const runSequenceHelper = "coopr-run-sequence"

// A single Buildah Run owns all temporary mounts. The static helper keeps that
// lifetime intact without requiring a shell or loader in the target image.
func prepareRunSequence(builder operationBuilder, commands [][]string) ([]string, specs.Mount, func() error, error) {
	var helper string
	var err error
	if native, ok := builder.(nativeBuilder); ok && native.executableOrigin != "" {
		helper, err = findRunSequenceHelperFrom(native.executableOrigin)
	} else {
		helper, err = findRunSequenceHelper()
	}
	if err != nil {
		return nil, specs.Mount{}, nil, err
	}
	command, mount, cleanup, err := materializeRunSequence(helper, commands)
	if err != nil {
		return nil, specs.Mount{}, nil, err
	}
	if native, ok := builder.(nativeBuilder); ok {
		if err := native.checkRunSequenceTarget(mount.Destination); err != nil {
			return nil, specs.Mount{}, nil, errors.Join(err, cleanup())
		}
	}
	return command, mount, cleanup, nil
}

func findRunSequenceHelper() (string, error) {
	return findRunSequenceHelperFrom(currentRunSequenceOrigin())
}

// Capture the installation hint before namespace workers replace argv[0]. An
// ordinary build does not need its executable to remain in the filesystem.
func currentRunSequenceOrigin() string {
	executable, _ := os.Executable()
	return captureRunSequenceOrigin(executable, os.Args[0])
}

func captureRunSequenceOrigin(executable, argv0 string) string {
	// containers/storage reexecs a detached bind-fd (reported as "/") or sealed
	// memfd, retaining the original argv[0] with this suffix until dispatch.
	if strings.HasPrefix(executable, "/memfd:") || executable == "/" {
		original, ok := strings.CutSuffix(argv0, "-in-a-user-namespace")
		if !ok {
			return ""
		}
		executable = original
	}
	return executable
}

func resolveRunSequenceOrigin(executable, argv0 string) (string, error) {
	origin := captureRunSequenceOrigin(executable, argv0)
	if origin == "" {
		return "", errors.New("coopr executable origin was not preserved before worker dispatch")
	}
	resolved, err := exec.LookPath(origin)
	if err != nil {
		return "", fmt.Errorf("resolve Coopr origin %q: %w", origin, err)
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func findRunSequenceHelperFrom(executable string) (string, error) {
	resolved, err := resolveRunSequenceOrigin(executable, "")
	if err != nil {
		return "", fmt.Errorf("locate RUN sequence helper: %w", err)
	}
	helper, err := runSequenceHelperBeside(resolved)
	if err == nil {
		return helper, nil
	}
	if !errors.Is(err, os.ErrNotExist) || !isGoDevelopmentExecutable(resolved) {
		return "", fmt.Errorf("locate installed RUN sequence helper: %w", err)
	}
	// go test/go run executables live in temporary directories; the Nix dev
	// shell supplies the packaged helper alongside its installed Coopr.
	developmentPackage, lookupErr := exec.LookPath("coopr")
	if lookupErr == nil {
		helper, lookupErr = runSequenceHelperBeside(developmentPackage)
	}
	if lookupErr != nil {
		return "", fmt.Errorf("multiple RUN execs require the packaged coopr-run-sequence helper; install the complete Coopr package or use nix develop: %w", errors.Join(err, lookupErr))
	}
	return helper, nil
}

func isGoDevelopmentExecutable(executable string) bool {
	if strings.HasSuffix(filepath.Base(executable), ".test") {
		return true
	}
	directory := filepath.Dir(executable)
	// cmd/go places temporary executables in go-build*/bNNN/exe/.
	object := filepath.Base(filepath.Dir(directory))
	if filepath.Base(directory) == "exe" && len(object) > 1 && object[0] == 'b' && strings.Trim(object[1:], "0123456789") == "" {
		return strings.HasPrefix(filepath.Base(filepath.Dir(filepath.Dir(directory))), "go-build")
	}
	// Cached go run executables live in <hash[:2]>/<hash>-d/<name>.
	entry := filepath.Base(directory)
	hash, ok := strings.CutSuffix(entry, "-d")
	if !ok || len(hash) != 64 || filepath.Base(filepath.Dir(directory)) != hash[:2] {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}

func runSequenceHelperBeside(executable string) (string, error) {
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	prefix := filepath.Dir(resolved)
	if filepath.Base(prefix) == "bin" {
		prefix = filepath.Dir(prefix)
	}
	helper := filepath.Join(prefix, "libexec", "coopr", runSequenceHelper)
	info, err := os.Lstat(helper)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		info, err = os.Stat(helper)
		if err != nil {
			return "", fmt.Errorf("invalid RUN sequence helper link %q: %v", helper, err)
		}
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("RUN sequence helper %q is not executable", helper)
	}
	return helper, nil
}

func materializeRunSequence(helper string, commands [][]string) (_ []string, _ specs.Mount, _ func() error, retErr error) {
	if len(commands) < 2 {
		return nil, specs.Mount{}, nil, errors.New("RUN sequence requires at least two commands")
	}
	for index, argv := range commands {
		if len(argv) == 0 || argv[0] == "" {
			return nil, specs.Mount{}, nil, fmt.Errorf("RUN exec %d requires an executable", index+1)
		}
		for _, arg := range argv {
			if strings.ContainsRune(arg, 0) {
				return nil, specs.Mount{}, nil, fmt.Errorf("RUN exec %d contains a NUL byte", index+1)
			}
		}
	}
	directory, err := os.MkdirTemp("", ".coopr-run-")
	if err != nil {
		return nil, specs.Mount{}, nil, err
	}
	cleanup := func() error {
		return errors.Join(os.Chmod(directory, 0o700), os.RemoveAll(directory))
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, cleanup())
		}
	}()
	source, err := os.Open(helper)
	if err != nil {
		return nil, specs.Mount{}, nil, err
	}
	destination, err := os.OpenFile(filepath.Join(directory, runSequenceHelper), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o555)
	if err != nil {
		return nil, specs.Mount{}, nil, errors.Join(err, source.Close())
	}
	_, copyErr := io.Copy(destination, source)
	if err := errors.Join(copyErr, source.Close(), destination.Chmod(0o555), destination.Close()); err != nil {
		return nil, specs.Mount{}, nil, err
	}
	data, err := json.Marshal(commands)
	if err != nil {
		return nil, specs.Mount{}, nil, err
	}
	if err := os.WriteFile(filepath.Join(directory, "commands.json"), data, 0o444); err != nil {
		return nil, specs.Mount{}, nil, err
	}
	if err := os.Chmod(filepath.Join(directory, "commands.json"), 0o444); err != nil {
		return nil, specs.Mount{}, nil, err
	}
	if err := os.Chmod(directory, 0o555); err != nil {
		return nil, specs.Mount{}, nil, err
	}
	target := "/" + filepath.Base(directory)
	mount := specs.Mount{Source: directory, Destination: target, Type: "bind", Options: []string{"ro", "bind", "exec", "Z"}}
	return []string{target + "/" + runSequenceHelper, target + "/commands.json"}, mount, cleanup, nil
}

func (b nativeBuilder) checkRunSequenceTarget(target string) (retErr error) {
	root, err := b.Mount(b.MountLabel)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, b.Unmount()) }()
	_, err = os.Lstat(filepath.Join(root, target))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("RUN helper mount target %q already exists", target)
}
