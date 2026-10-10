package buildah

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

func TestWorkerFailurePreservesRUNExitCodeAcrossBothRelays(t *testing.T) {
	err := exec.Command("sh", "-c", "exit 42").Run()
	for range 2 {
		err = workerFailure(fmt.Errorf("execute RUN: %w", err))
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
