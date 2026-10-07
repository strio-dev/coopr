package build

import (
	"context"
	"coopr/internal/buildah"
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

func TestBuildComponentRejectsAllFileOutputAliasesBeforeExecution(t *testing.T) {
	for _, kind := range []string{"log-definition", "rusage-definition", "metadata-log", "archive-log", "rusage-log", "symlink-definition", "hardlink-definition", "metadata-log-symlink", "archive-log-hardlink", "split-metadata-log"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "component.coopr")
			source := []byte("extend as=\"base\"\n")
			if err := os.WriteFile(file, source, 0600); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(dir, "output")
			if err := os.WriteFile(log, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			opts := ComponentOptions{File: file, Context: dir, StoreDir: filepath.Join(dir, "components"), BuildStore: buildah.StoreOptions{GraphRoot: filepath.Join(dir, "graph"), RunRoot: filepath.Join(dir, "run"), GraphDriverName: "vfs"}}
			switch kind {
			case "log-definition":
				opts.LogFile = file
			case "rusage-definition":
				opts.RusageLogFile = file
			case "metadata-log":
				opts.LogFile, opts.MetadataFile = log, log
			case "archive-log":
				opts.LogFile, opts.Tag = log, "oci-archive:"+log
			case "rusage-log":
				opts.LogFile, opts.RusageLogFile = log, log
			case "metadata-log-symlink":
				alias := filepath.Join(dir, "alias")
				if err := os.Symlink(log, alias); err != nil {
					t.Fatal(err)
				}
				opts.LogFile, opts.MetadataFile = log, alias
			case "archive-log-hardlink":
				alias := filepath.Join(dir, "alias")
				if err := os.Link(log, alias); err != nil {
					t.Fatal(err)
				}
				opts.LogFile, opts.Tag = log, "oci-archive:"+alias
			case "split-metadata-log":
				opts.LogFile, opts.LogSplit = log, true
				opts.Platforms = []string{"linux/arm/v6", "linux/arm/v7"}
				opts.MetadataFile = platformLogPath(log, "linux/arm/v6")
			case "symlink-definition":
				alias := filepath.Join(dir, "alias")
				if err := os.Symlink(file, alias); err != nil {
					t.Fatal(err)
				}
				opts.LogFile = alias
			case "hardlink-definition":
				alias := filepath.Join(dir, "alias")
				if err := os.Link(file, alias); err != nil {
					t.Fatal(err)
				}
				opts.LogFile = alias
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := BuildComponent(ctx, opts)
			if err == nil || !strings.Contains(err.Error(), "overlap") {
				t.Fatalf("output alias reached execution: %v", err)
			}
			got, err := os.ReadFile(file)
			if err != nil || string(got) != string(source) {
				t.Fatalf("definition changed: %q, %v", got, err)
			}
			got, err = os.ReadFile(log)
			if err != nil || string(got) != "preserve" {
				t.Fatalf("output changed: %q, %v", got, err)
			}
		})
	}
}
