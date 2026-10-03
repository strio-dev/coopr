package build

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishRejectsContainerDefinitionBeforeBuilder(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "app.coopr")
	if err := os.WriteFile(file, []byte("from \"scratch\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := PublishComponent(context.Background(), PublishOptions{
		File: file, Reference: "example.com/team/component:1",
	})
	if err == nil || !strings.Contains(err.Error(), "component definition") {
		t.Fatalf("container definition was not rejected before builder: %v", err)
	}
}

func TestPublishCancelledCleansOwnedDirectories(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	file := filepath.Join(dir, "component.coopr")
	if err := os.WriteFile(file, []byte("extend as=\"base\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := PublishComponent(ctx, PublishOptions{
		File: file, Reference: "example.com/team/component:1",
	})
	if err == nil {
		t.Fatal("cancelled publication succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".coopr-stage-") || strings.HasPrefix(entry.Name(), ".coopr-build-worker-") {
			t.Fatalf("cancelled publication leaked %q", entry.Name())
		}
	}
}

func TestBuildComponentRejectsInvalidLocalTagBeforeExecution(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "component.coopr")
	if err := os.WriteFile(file, []byte("extend as=\"base\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := BuildComponent(context.Background(), ComponentOptions{
		File: file, Tag: "team/component",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid local component tag") {
		t.Fatalf("invalid local tag reached execution: %v", err)
	}
}

func TestBuildComponentRejectsDefinitionAsArchiveOutput(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "component.coopr")
	if err := os.WriteFile(file, []byte("extend as=\"base\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := BuildComponent(context.Background(), ComponentOptions{
		File: file, Tag: "oci-archive:" + file,
	})
	if err == nil || !strings.Contains(err.Error(), "overwrite an input") {
		t.Fatalf("definition overwrite reached execution: %v", err)
	}
}
