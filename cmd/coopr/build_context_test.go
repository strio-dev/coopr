package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestBuildCommandsAcceptSSHContextBeforeBuilding(t *testing.T) {
	for _, args := range [][]string{
		{"build", "missing.coopr", "--build-context", "source=ssh://git@example.com/context.git"},
		{"component", "build", "missing.coopr", "--build-context", "source=git@example.com:context.git"},
	} {
		var stdout, stderr bytes.Buffer
		if status := run(args, &stdout, &stderr); status == 0 || !strings.Contains(stderr.String(), "open definition") {
			t.Fatalf("run(%v) status = %d, stderr = %q", args, status, stderr.String())
		}
		if strings.Contains(stderr.String(), "unsupported remote build context") {
			t.Fatalf("supported SSH context was rejected: %q", stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("failed build printed success: %q", stdout.String())
		}
	}
}
