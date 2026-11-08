package build

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/buildah"
)

func TestStagedLayoutPreservesIncompleteWorkerEvidence(t *testing.T) {
	for _, operationErr := range []error{buildah.ErrWorkerWaitIncomplete, fmt.Errorf("platform build: %w", buildah.ErrWorkerWaitIncomplete), errors.Join(errors.New("operation failed"), buildah.ErrWorkerWaitIncomplete)} {
		t.Run(operationErr.Error(), func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			layout, cleanup, err := stageLayout(filepath.Join(t.TempDir(), "source.coopr"))
			if err != nil {
				t.Fatal(err)
			}
			ancestor := filepath.Dir(layout)
			// This directory is the frontend's own fixture; no live command uses it.
			t.Cleanup(func() { _ = os.RemoveAll(ancestor) })
			worker := filepath.Join(ancestor, ".coopr-build-worker-owned")
			if err := os.Mkdir(worker, 0o700); err != nil {
				t.Fatal(err)
			}
			request := filepath.Join(worker, "request.json")
			if err := os.WriteFile(request, []byte("owned evidence"), 0o600); err != nil {
				t.Fatal(err)
			}
			resultErr := cleanupBuildFiles(filepath.Dir(layout), cleanup, operationErr)
			if !errors.Is(resultErr, operationErr) || !errors.Is(resultErr, buildah.ErrWorkerWaitIncomplete) {
				t.Fatalf("lost operation classification: %v", resultErr)
			}
			if !strings.Contains(resultErr.Error(), ancestor) {
				t.Errorf("retained ancestor path missing: %v", resultErr)
			}
			if data, err := os.ReadFile(request); err != nil || string(data) != "owned evidence" {
				t.Fatalf("retained worker evidence was removed: %q (%v)", data, err)
			}
		})
	}
}

func TestStagedLayoutCleansSuccessAndOrdinaryFailure(t *testing.T) {
	for _, operationErr := range []error{nil, errors.New("ordinary failure")} {
		t.Setenv("TMPDIR", t.TempDir())
		layout, cleanup, err := stageLayout(filepath.Join(t.TempDir(), "source.coopr"))
		if err != nil {
			t.Fatal(err)
		}
		resultErr := cleanupBuildFiles(filepath.Dir(layout), cleanup, operationErr)
		if resultErr != operationErr {
			t.Fatalf("ordinary result changed: %v -> %v", operationErr, resultErr)
		}
		if _, err := os.Stat(filepath.Dir(layout)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ordinary staging retained: %v", err)
		}
	}
}

func TestPrimaryContextPreservesIncompleteWorkerInputs(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	content := "still required by the worker"
	if err := writer.WriteHeader(&tar.Header{Name: "input", Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	primary, cleanup, err := preparePrimaryContext(context.Background(), "definition.coopr", "-", nil, nil, &archive)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })
	// Force stageLayout's existing output-parent fallback inside the owned
	// primary materialization. Both ancestor cleanup defers must retain it.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing-temporary"))
	layout, stageCleanup, err := stageLayout(filepath.Join(primary.Path, "definition.coopr"))
	if err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join(filepath.Dir(layout), ".coopr-build-worker-owned")
	if err := os.Mkdir(worker, 0o700); err != nil {
		t.Fatal(err)
	}
	request := filepath.Join(worker, "request.json")
	if err := os.WriteFile(request, []byte("fallback evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	operationErr := fmt.Errorf("worker still incomplete: %w", buildah.ErrWorkerWaitIncomplete)
	run := func() (retErr error) {
		defer func() { retErr = cleanupBuildFiles(primary.Path, func() { _ = cleanup() }, retErr) }()
		defer func() { retErr = cleanupBuildFiles(filepath.Dir(layout), stageCleanup, retErr) }()
		return operationErr
	}
	resultErr := run()
	if _, err := os.Stat(request); err != nil {
		t.Fatalf("fallback staging ancestor removed by primary cleanup: %v", err)
	}
	if !errors.Is(resultErr, operationErr) {
		t.Errorf("lost operation error: %v", resultErr)
	}
	if data, err := os.ReadFile(filepath.Join(primary.Path, "input")); err != nil || string(data) != content {
		t.Fatalf("materialized primary deleted before worker completion: %q (%v)", data, err)
	}
	if !strings.Contains(resultErr.Error(), primary.Path) {
		t.Fatalf("retained primary path missing: %v", resultErr)
	}
}

func TestBuildFilesCleanupPreservesCallerOwnedPrimary(t *testing.T) {
	directory := t.TempDir()
	marker := filepath.Join(directory, "user-input")
	if err := os.WriteFile(marker, []byte("caller owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	primary, cleanup, err := preparePrimaryContext(context.Background(), filepath.Join(directory, "definition.coopr"), directory, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, operationErr := range []error{nil, errors.New("ordinary failure"), buildah.ErrWorkerWaitIncomplete} {
		resultErr := cleanupBuildFiles(primary.Path, func() { _ = cleanup() }, operationErr)
		if operationErr != nil && !errors.Is(resultErr, operationErr) {
			t.Fatalf("lost caller failure: %v", resultErr)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("caller-owned primary removed: %v", err)
		}
	}
}
