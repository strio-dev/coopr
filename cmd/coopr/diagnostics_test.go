package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

func TestCommandErrorsUseUpstreamPrefix(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"root", []string{"--not-a-real-option"}, "Error: unknown flag: --not-a-real-option\nSee 'coopr --help'\n"},
		{"build", []string{"build", "--not-a-real-option"}, "Error: unknown flag: --not-a-real-option\nSee 'coopr build --help'\n"},
		{"nested", []string{"image", "inspect", "--not-a-real-option"}, "Error: unknown flag: --not-a-real-option\nSee 'coopr image inspect --help'\n"},
		{"value", []string{"build", "--file"}, "Error: flag needs an argument: --file\nSee 'coopr build --help'\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			status := run(test.args, &stdout, &stderr)
			if status != 125 || stdout.Len() != 0 || stderr.String() != test.want {
				t.Fatalf("status=%d stdout=%q stderr=%q", status, &stdout, &stderr)
			}
		})
	}
}

func TestCommandErrorPrefixIsNotDuplicated(t *testing.T) {
	for _, message := range []string{"namespace unavailable", "Error: namespace unavailable"} {
		t.Run(message, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			status := runContextWithStorageNamespace(context.Background(), []string{"images"}, &stdout, &stderr, func() error {
				return errors.New(message)
			})
			if status != 125 || stdout.Len() != 0 || stderr.String() != "Error: namespace unavailable\n" {
				t.Fatalf("status=%d stdout=%q stderr=%q", status, &stdout, &stderr)
			}
		})
	}
}

func TestCommandFailuresDefaultTo125(t *testing.T) {
	for _, args := range [][]string{{"no-such-command"}, {"image", "inspect"}, {"--log-level=invalid", "images"}} {
		var stdout, stderr bytes.Buffer
		if status := run(args, &stdout, &stderr); status != 125 || stdout.Len() != 0 || !bytes.HasPrefix(stderr.Bytes(), []byte("Error: ")) || bytes.Contains(stderr.Bytes(), []byte("See '")) {
			t.Fatalf("args=%v status=%d stdout=%q stderr=%q", args, status, &stdout, &stderr)
		}
	}
}

func TestCommandSpecificExitStatusesArePreserved(t *testing.T) {
	for _, code := range []int{1, 42, 125, 126, 127} {
		var stdout, stderr bytes.Buffer
		status := runContextWithStorageNamespace(context.Background(), []string{"images"}, &stdout, &stderr, func() error {
			return commandExitError{Code: code, Err: errors.New("explicit failure")}
		})
		if status != code || stdout.Len() != 0 || stderr.String() != "Error: explicit failure\n" {
			t.Fatalf("code=%d status=%d stdout=%q stderr=%q", code, status, &stdout, &stderr)
		}
	}
}

func TestCommandPreservesWrappedSubprocessExitStatus(t *testing.T) {
	childErr := exec.Command("false").Run()
	var exit *exec.ExitError
	if !errors.As(childErr, &exit) {
		t.Fatalf("expected subprocess exit error: %v", childErr)
	}
	var stdout, stderr bytes.Buffer
	status := runContextWithStorageNamespace(context.Background(), []string{"images"}, &stdout, &stderr, func() error {
		return fmt.Errorf("wrapped failure: %w", childErr)
	})
	if status != exit.ExitCode() || stdout.Len() != 0 || !bytes.HasPrefix(stderr.Bytes(), []byte("Error: wrapped failure: ")) {
		t.Fatalf("status=%d stdout=%q stderr=%q", status, &stdout, &stderr)
	}
}
