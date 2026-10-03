package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestBuildContextFlagIsRepeatable(t *testing.T) {
	for _, command := range []struct {
		name string
		cmd  interface {
			ParseFlags([]string) error
		}
	}{
		{name: "image", cmd: newBuildCommand()},
		{name: "component", cmd: newComponentBuildCommand()},
	} {
		t.Run(command.name, func(t *testing.T) {
			if err := command.cmd.ParseFlags([]string{
				"--build-context", "source=.",
				"--build-context", "base=docker-image://example.com/base:latest",
			}); err != nil {
				t.Fatalf("parse repeated --build-context: %v", err)
			}
		})
	}
}

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
