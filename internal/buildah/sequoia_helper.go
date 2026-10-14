package buildah

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const SequoiaHelperName = "coopr-sequoia-signing"
const SequoiaHelperProtocol = "coopr-sequoia-signing/1"

// FindSequoiaHelper locates the optional installed backend and checks its wire version.
// Callers invoke it only for explicitly requested Sequoia signing.
func FindSequoiaHelper(ctx context.Context) (string, error) {
	executable, _ := os.Executable()
	directory := filepath.Dir(executable)
	candidates := []string{filepath.Join(directory, SequoiaHelperName), filepath.Join(directory, "..", "libexec", "coopr", SequoiaHelperName)}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil && resolved != executable {
		directory = filepath.Dir(resolved)
		candidates = append(candidates, filepath.Join(directory, SequoiaHelperName), filepath.Join(directory, "..", "libexec", "coopr", SequoiaHelperName))
	}
	if path, err := exec.LookPath(SequoiaHelperName); err == nil {
		candidates = append(candidates, path)
	}
	for _, path := range candidates {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return "", fmt.Errorf("sequoia helper %q is not executable", path)
		}
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		command := exec.Command(path, "--protocol-version")
		var output limitedHelperOutput
		command.Stdout = &output
		command.Env = withoutSigningPassword(os.Environ())
		err = runWorkerProcess(probeCtx, command, workerGrace)
		cancel()
		if err != nil || output.String() != SequoiaHelperProtocol+"\n" {
			return "", errors.Join(fmt.Errorf("incompatible Sequoia helper %q (requires %s)", path, SequoiaHelperProtocol), err, output.err)
		}
		return path, nil
	}
	return "", fmt.Errorf("--sign-by-sq-fingerprint requires the optional %s package on PATH or beside Coopr", SequoiaHelperName)
}

type limitedHelperOutput struct {
	bytes.Buffer
	err error
}

func (output *limitedHelperOutput) Write(data []byte) (int, error) {
	if output.Len()+len(data) > 1024 {
		output.err = errors.New("helper handshake output exceeds limit")
		return 0, output.err
	}
	return output.Buffer.Write(data)
}

// RunSequoiaWorker shares the native worker's process supervision and terminal routing.
func RunSequoiaWorker(ctx context.Context, helper, worker, request, jobDir string, progress io.Writer) error {
	command := exec.Command(helper, request)
	command.Args[0] = worker
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stderr, progress
	command.Env = append(withoutSigningPassword(os.Environ()), "TMPDIR="+jobDir)
	return runWorkerProcess(ctx, command, workerGrace)
}

// WriteSigningWorkerJSON and ReadSigningWorkerJSON retain the existing bounded worker protocol.
func WriteSigningWorkerJSON(path string, value any) error { return writeWorkerJSON(path, value) }
func ReadSigningWorkerJSON(path string, value any) error  { return readWorkerJSON(path, value) }

// StageSigningSecret stores raw secret bytes in the worker's private job directory.
func StageSigningSecret(directory, name string, secret []byte) (string, error) {
	if len(secret) == 0 {
		return "", nil
	}
	path := filepath.Join(directory, name)
	return path, os.WriteFile(path, secret, 0600)
}
