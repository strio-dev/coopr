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

func TestBuildCommandsRejectGitContextFragmentsBeforeBuilding(t *testing.T) {
	for _, command := range [][]string{{"build"}, {"component", "build"}} {
		for _, named := range []bool{false, true} {
			for _, source := range []string{
				"https://example.invalid/source.git#main:src",
				"https://example.invalid/source.git?branch=main&subdir=src#refs/heads/main:src",
				"git@example.invalid:team/source.git#",
			} {
				args := append([]string{}, command...)
				if named {
					args = append(args, "missing.coopr", "--build-context", "source="+source)
				} else {
					args = append(args, "--file", "missing.coopr", source)
				}
				var stdout, stderr bytes.Buffer
				status := run(args, &stdout, &stderr)
				if status == 0 || !strings.Contains(stderr.String(), "query selectors") {
					t.Errorf("run(%v) status = %d, stderr = %q", args, status, stderr.String())
				}
				if stdout.Len() != 0 || strings.Contains(stderr.String(), "example.invalid") {
					t.Errorf("run(%v) exposed context URL or success output: stdout=%q stderr=%q", args, stdout.String(), stderr.String())
				}
			}
		}
	}
}
