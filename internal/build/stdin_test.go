package build

import (
	"context"
	"strings"
	"testing"
)

func TestRunAcceptsConsumedDefinitionStdin(t *testing.T) {
	stdin := strings.NewReader("invalid-definition\n")
	_, err := Run(context.Background(), Options{File: "-", Stdin: stdin, RunStdin: stdin})
	if err == nil || strings.Contains(err.Error(), "--stdin") {
		t.Fatalf("error=%v", err)
	}
}

func TestBuildComponentAcceptsConsumedContextStdin(t *testing.T) {
	_, err := BuildComponent(context.Background(), ComponentOptions{File: "component.coopr", Context: "-", RunStdin: strings.NewReader("input")})
	if err == nil || strings.Contains(err.Error(), "--stdin") {
		t.Fatalf("error=%v", err)
	}
}
