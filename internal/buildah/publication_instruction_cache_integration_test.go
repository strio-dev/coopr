package buildah

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestPublishPlanPublishesInstructionCandidatesOnlyAfterSuccess(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live component publication cache")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(busybox)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "busybox"), binary, 0o755); err != nil {
		t.Fatal(err)
	}
	plan := testPublicationPlan(t, `
package as="payload"
copy "busybox" "/busybox"
run network="none" { exec "/busybox" "sh" "-c" "/busybox od -An -N16 -tx1 /dev/urandom | /busybox tr -d ' \\n' >/proof" }
run network="none" {
  exec "/busybox" "sh" "-c" "test -s /proof; /busybox touch /cache/marker"
  mount "cache" target="/cache" id="publication-portable-boundary"
}
extend
copy "/proof" "/proof" from="payload"
`)

	build := func(name, cacheDir string, noCache, failSnapshot bool) (string, StoreOptions, error) {
		t.Helper()
		baseDir := filepath.Join(root, name)
		store := StoreOptions{RunRoot: filepath.Join(baseDir, "run"), GraphRoot: filepath.Join(baseDir, "graph"), GraphDriverName: "vfs"}
		output := filepath.Join(baseDir, "payload.tar")
		if failSnapshot {
			if err := os.MkdirAll(output, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		_, err := PublishPlan(ctx, plan, PublicationOptions{
			PlanOptions: PlanOptions{
				Store: store, ContextDir: contextDir, Isolation: "rootless", Runtime: "crun",
				CacheLocalDir: cacheDir, NoCache: noCache,
			},
			PackagePaths: map[string]string{"payload": output},
		})
		if err != nil {
			return "", store, err
		}
		return readPublicationPackageFile(t, output, "proof"), store, nil
	}

	cacheDir := filepath.Join(root, "cache")
	cold, _, err := build("cold", cacheDir, false, false)
	if err != nil {
		t.Fatal(err)
	}
	refreshed, _, err := build("refreshed", cacheDir, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed == cold {
		t.Fatalf("no-cache publication reused ordinary instruction result %q", cold)
	}
	warm, _, err := build("warm", cacheDir, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if warm != refreshed {
		t.Fatalf("fresh store proof %q, want no-cache refresh %q", warm, refreshed)
	}
	if count := publicationCacheManifestCount(t, cacheDir); count == 0 {
		t.Fatal("successful publication stored no portable instruction cache manifests")
	}

	failedCache := filepath.Join(root, "failed-cache")
	_, failedStore, err := build("failed", failedCache, false, true)
	if err == nil || !strings.Contains(err.Error(), "payload.tar") {
		t.Fatalf("failed package snapshot error = %v", err)
	}
	if count := instructionCacheRecordCount(t, failedStore); count == 0 {
		t.Fatal("failed publication did not execute far enough to create an instruction candidate")
	}
	if count := publicationCacheManifestCount(t, failedCache); count != 0 {
		t.Fatalf("failed publication stored %d portable cache manifests", count)
	}
}

func readPublicationPackageFile(t *testing.T, archivePath, name string) string {
	t.Helper()
	file, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if filepath.ToSlash(header.Name) != name && filepath.ToSlash(header.Name) != "./"+name {
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	t.Fatalf("package %s has no %s", archivePath, name)
	return ""
}

func publicationCacheManifestCount(t *testing.T, cacheDir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cacheDir, "index.json"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	return len(index.Manifests)
}
