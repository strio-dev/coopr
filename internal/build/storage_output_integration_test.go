package build

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/buildah"
	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"coopr/internal/testutil"
)

// Opt-in because it writes to the selected containers-storage image store.
func TestNativeStorageMatchesArchiveAndRuns(t *testing.T) {
	loadTestBackend(t)
	if _, err := exec.LookPath("podman"); err != nil {
		t.Fatalf("Podman is required for load acceptance: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	work := t.TempDir()
	t.Setenv("TMPDIR", work)
	file := loadTestDefinition(t, ctx, work)
	cli := storageTestCLI(t, ctx)
	marker := fmt.Sprintf("ready-%d-%d", os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(filepath.Join(work, "marker"), []byte(marker+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(work, "image.oci.tar")
	base := Options{File: file, Platform: "linux/" + runtime.GOARCH}
	archiveBuild := base
	archiveBuild.Tag = "oci-archive:" + archive
	if _, err := Run(ctx, archiveBuild); err != nil {
		t.Fatalf("archive build: %v", err)
	}
	manifest, image := readExampleImage(t, ctx, archive)
	if len(manifest.Layers) != 2 || len(image.RootFS.DiffIDs) != 2 || image.Config.Labels["coopr_test"] != "load" || len(image.Config.Cmd) != 1 || image.Config.Cmd[0] != "/runner" {
		t.Fatalf("archive lost layers/config: layers=%d diffIDs=%d config=%+v", len(manifest.Layers), len(image.RootFS.DiffIDs), image.Config)
	}
	archiveID, err := oci.ArchiveImageConfigDigest(ctx, archive)
	if err != nil {
		t.Fatal(err)
	}
	if archiveID != manifest.Config.Digest {
		t.Fatalf("archive image ID = %s, manifest config = %s", archiveID, manifest.Config.Digest)
	}
	if output, err := exec.CommandContext(ctx, "podman", "image", "exists", archiveID.String()).CombinedOutput(); err != nil {
		t.Fatalf("archive build must retain its image in the shared native store: %v: %s", err, output)
	}
	tag := fmt.Sprintf("localhost/coopr-load-test-%d-%d:acceptance", os.Getpid(), time.Now().UnixNano())
	if output, err := exec.CommandContext(ctx, "podman", "image", "exists", tag).CombinedOutput(); err == nil {
		t.Fatalf("test tag already exists: %s", tag)
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("inspect test tag: %v: %s", err, output)
	}
	storedName, err := runStorageCLI(ctx, cli, file, tag, nil)
	if err != nil {
		t.Fatalf("native storage build: %v", err)
	}
	if decoded, err := hex.DecodeString(storedName); err != nil || len(decoded) != 32 {
		t.Fatalf("build did not return raw native ID: %q", storedName)
	}
	t.Cleanup(func() {
		command := exec.Command("podman", "rmi", tag)
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("remove owned Podman tag %s: %v: %s", tag, err, output)
		}
	})
	loadedID := strings.TrimSpace(string(loadPodman(t, ctx, "image", "inspect", "--format", "{{.Id}}", tag)))
	if strings.TrimPrefix(loadedID, "sha256:") != storedName {
		t.Fatalf("build ID %s differs from stored ID %s", storedName, loadedID)
	}
	if digest.Digest("sha256:"+strings.TrimPrefix(loadedID, "sha256:")).Validate() != nil {
		t.Fatalf("Podman returned invalid stored image ID %q", loadedID)
	}
	output := string(loadPodman(t, ctx, "run", "--rm", "--network=none", tag))
	if strings.TrimSpace(output) != "loaded:"+marker {
		t.Fatalf("stored image did not run with copied file/config: %q", output)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("archive output lost after storage build: %v", err)
	}
	assertNoStageDirs(t, work)
}

func TestDefaultNativeStoreRetagRunsWithoutContext(t *testing.T) {
	loadTestBackend(t)
	if _, err := exec.LookPath("podman"); err != nil {
		t.Fatalf("Podman is required for native storage acceptance: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	work := t.TempDir()
	t.Setenv("TMPDIR", work)
	file := loadTestDefinition(t, ctx, work)
	cli := storageTestCLI(t, ctx)
	marker := fmt.Sprintf("unnamed-%d-%d", os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(filepath.Join(work, "marker"), []byte(marker+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	localRef, err := runStorageCLI(ctx, cli, file, "", nil)
	if err != nil {
		t.Fatalf("store default image: %v", err)
	}
	if decoded, err := hex.DecodeString(localRef); err != nil || len(decoded) != 32 {
		t.Fatalf("default build returned invalid native ID %q", localRef)
	}
	storeOptions, err := buildah.DefaultStoreOptions()
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := testStoredImageSelection(ctx, storeOptions, localRef, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}); err != nil || !found {
		t.Fatalf("default image missing from native storage: found=%t err=%v", found, err)
	}
	if err := os.Remove(filepath.Join(work, "runner")); err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("localhost/coopr-copy-test-%d-%d:acceptance", os.Getpid(), time.Now().UnixNano())
	copy := exec.CommandContext(ctx, cli, "copy", localRef, tag)
	var copyOutput, copyDiagnostics bytes.Buffer
	copy.Stdout = &copyOutput
	copy.Stderr = &copyDiagnostics
	err = copy.Run()
	if err != nil || strings.TrimSpace(copyOutput.String()) != tag {
		t.Fatalf("retag stored image: %v: stdout=%s stderr=%s", err, copyOutput.String(), copyDiagnostics.String())
	}
	t.Cleanup(func() {
		command := exec.Command("podman", "rmi", tag)
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("remove owned retagged image %s: %v: %s", tag, err, output)
		}
	})
	output := string(loadPodman(t, ctx, "run", "--rm", "--network=none", tag))
	if strings.TrimSpace(output) != "loaded:"+marker {
		t.Fatalf("retagged image did not run: %q", output)
	}
	assertNoStageDirs(t, work)
}

func loadTestBackend(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires native build integration")
	}
}

func storageTestCLI(t *testing.T, ctx context.Context) string {
	t.Helper()
	if path, supplied := os.LookupEnv("COOPR_TEST_CLI"); supplied {
		if err := testutil.ValidateExecutable(path); err != nil {
			t.Fatalf("COOPR_TEST_CLI: %v", err)
		}
		return path
	}
	path := filepath.Join(t.TempDir(), "coopr")
	command := exec.CommandContext(ctx, "go", "build", "-tags", "exclude_graphdriver_btrfs,systemd,seccomp", "-o", path, "./cmd/coopr")
	command.Dir = filepath.Join("..", "..")
	command.Env = append(os.Environ(), "CGO_ENABLED=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build Coopr CLI for native storage test: %v: %s", err, output)
	}
	return path
}

func runStorageCLI(ctx context.Context, binary, definition, tag string, extraEnv []string) (string, error) {
	args := []string{"build", definition, "--platform", "linux/" + runtime.GOARCH}
	if tag != "" {
		args = append(args, "--tag", tag)
	}
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = append(os.Environ(), extraEnv...)
	var output, diagnostics bytes.Buffer
	command.Stdout = &output
	command.Stderr = &diagnostics
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("coopr build: %w: %s", err, strings.TrimSpace(diagnostics.String()))
	}
	return strings.TrimSpace(output.String()), nil
}

func loadTestDefinition(t *testing.T, ctx context.Context, work string) string {
	t.Helper()
	fixture, err := storageRunnerFixture()
	if err != nil {
		t.Fatal(err)
	}
	runner := filepath.Join(work, "runner")
	if fixture != "" {
		data, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(runner, data, 0o755); err != nil {
			t.Fatal(err)
		}
	} else {
		command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", runner, "./testdata/storage-runner/main.go")
		command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build static runner: %v: %s", err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "marker"), []byte("ready\n"), 0600); err != nil {
		t.Fatal(err)
	}
	definition := filepath.Join(work, "app.coopr")
	content := "from \"scratch\"\ncopy \"runner\" \"/runner\"\ncopy \"marker\" \"/marker\"\nlabel coopr_test=\"load\"\ncmd { exec \"/runner\" }\n"
	if err := os.WriteFile(definition, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return definition
}

func storageRunnerFixture() (string, error) {
	root, supplied := os.LookupEnv("COOPR_TEST_FIXTURES")
	if !supplied {
		return "", nil
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("COOPR_TEST_FIXTURES must be an absolute directory")
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("COOPR_TEST_FIXTURES: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("COOPR_TEST_FIXTURES must be a directory: %s", root)
	}
	path := filepath.Join(root, "storage-runner")
	if err := testutil.ValidateExecutable(path); err != nil {
		return "", fmt.Errorf("COOPR_TEST_FIXTURES: %w", err)
	}
	return path, nil
}

func TestStorageRunnerFixtureOverride(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"", "fixtures", filepath.Join(root, "missing"), root} {
		t.Run(directory, func(t *testing.T) {
			t.Setenv("COOPR_TEST_FIXTURES", directory)
			if path, err := storageRunnerFixture(); err == nil {
				t.Fatalf("accepted unusable fixture directory %q: %q", directory, path)
			}
		})
	}
	fixture := filepath.Join(root, "storage-runner")
	if err := os.WriteFile(fixture, []byte("prebuilt runner"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOPR_TEST_FIXTURES", root)
	if _, err := storageRunnerFixture(); err == nil {
		t.Fatal("accepted a non-executable fixture")
	}
	if err := os.Chmod(fixture, 0o755); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	loadTestDefinition(t, context.Background(), work)
	data, err := os.ReadFile(filepath.Join(work, "runner"))
	if err != nil || string(data) != "prebuilt runner" {
		t.Fatalf("fixture was not copied: data=%q error=%v", data, err)
	}
}

func loadPodman(t *testing.T, ctx context.Context, args ...string) []byte {
	t.Helper()
	command := exec.CommandContext(ctx, "podman", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("podman %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return output
}

func assertNoStageDirs(t *testing.T, root string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, ".coopr-stage-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("staging directories left behind: %v", matches)
	}
}
