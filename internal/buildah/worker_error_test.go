package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerFailurePreservesRUNExitCodeAcrossBothRelays(t *testing.T) {
	err := exec.Command("sh", "-c", "exit 42").Run()
	for range 2 {
		err = WorkerFailure(fmt.Errorf("execute RUN: %w", err))
		var status interface{ ExitCode() int }
		if !errors.As(err, &status) || status.ExitCode() != 42 {
			t.Fatalf("lost RUN exit code: %v", err)
		}
	}
}

func TestReportedWorkerFailureDoesNotOwnDiagnostic(t *testing.T) {
	err := reportedWorkerFailure(errors.New("failed RUN"))
	if !workerFailureReported(err) {
		t.Fatal("worker would print an already serialized diagnostic")
	}
	if workerFailureReported(errors.New("write response failed")) {
		t.Fatal("worker would hide a protocol failure")
	}
}

func TestWorkerFailurePreservesIncompleteWaitAcrossRelays(t *testing.T) {
	err := errors.Join(context.Canceled, fmt.Errorf("stage did not stop: %w", ErrWorkerWaitIncomplete))
	for range 2 {
		data, marshalErr := json.Marshal(WorkerFailure(err))
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		var failure workerError
		if decodeErr := json.Unmarshal(data, &failure); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		err = &failure
		if !errors.Is(err, ErrWorkerWaitIncomplete) {
			t.Fatalf("worker response lost incomplete-wait identity: %s", data)
		}
	}
}

func TestMissingWorkerFailureDoesNotReportIncompleteWait(t *testing.T) {
	var failure *workerError
	if errors.Is(failure, ErrWorkerWaitIncomplete) {
		t.Fatal("an absent failure reported incomplete work")
	}
}

func TestWorkerSupervisionErrorRetainsUnexpectedExits(t *testing.T) {
	unexpected := exec.Command("sh", "-c", "exit 17").Run()
	killed := exec.Command(testWorkerBinary(t), "-test.run=^TestWorkerProcessHelper$", "-test.short")
	killed.Env = replaceEnv(os.Environ(), workerProcessHelperMode, "self-kill")
	signaled := killed.Run()
	for _, err := range []error{unexpected, signaled} {
		if err == nil || !errors.Is(WorkerSupervisionError(err), err) {
			t.Fatalf("unexpected worker exit was discarded: %v", err)
		}
	}
}

func TestWorkerDirectoryCleanupPreservesIncompleteWork(t *testing.T) {
	failure := errors.New("fixture operation failure")
	for _, test := range []struct {
		name string
		err  error
		keep bool
	}{
		{"success", nil, false},
		{"ordinary failure", failure, false},
		{"incomplete worker", fmt.Errorf("cancelled worker: %w", ErrWorkerWaitIncomplete), true},
		{"relayed incomplete stage", WorkerFailure(errors.Join(context.Canceled, ErrWorkerWaitIncomplete)), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			directory, err := os.MkdirTemp(root, "private-worker-")
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(directory, "secret")
			if err := os.WriteFile(file, []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			result := RemoveWorkerDirectory(directory, test.err)
			if test.err != nil && !errors.Is(result, test.err) {
				t.Fatalf("cleanup lost original error: %v", result)
			}
			info, statErr := os.Stat(directory)
			if !test.keep {
				if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("completed worker directory remains: %v", statErr)
				}
				return
			}
			if statErr != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("incomplete worker directory privacy = %v, %v", info, statErr)
			}
			if !errors.Is(result, ErrWorkerWaitIncomplete) || !strings.Contains(result.Error(), directory) {
				t.Fatalf("retained directory is not reported: %v", result)
			}
			data, readErr := os.ReadFile(file)
			if readErr != nil || string(data) != "fixture" {
				t.Fatalf("incomplete worker lost its files: %q, %v", data, readErr)
			}
		})
	}
}
