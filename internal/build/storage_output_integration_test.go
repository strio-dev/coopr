package build

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/imagecatalog"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
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
	if output, err := exec.CommandContext(ctx, "podman", "image", "exists", archiveID.String()).CombinedOutput(); err == nil {
		t.Skipf("image %s already exists in Podman; preserving preexisting image", archiveID)
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("inspect preexisting image: %v: %s", err, output)
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
	if storedName != "podman:"+tag {
		t.Fatalf("stored name %q, want podman:%s", storedName, tag)
	}
	t.Cleanup(func() {
		command := exec.Command("podman", "rmi", tag)
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("remove owned Podman tag %s: %v: %s", tag, err, output)
		}
	})
	loadedID := strings.TrimSpace(string(loadPodman(t, ctx, "image", "inspect", "--format", "{{.Id}}", tag)))
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

func TestDefaultCooprStoreCanCopyToPodmanLater(t *testing.T) {
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
	if !strings.HasPrefix(localRef, "sha256:") {
		t.Fatalf("default build returned %q, want local digest reference", localRef)
	}
	storeDir, err := localstore.DefaultImageDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := imagecatalog.Lookup(ctx, storeDir, localRef, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}); err != nil || !found {
		t.Fatalf("default image missing from Coopr store: found=%t err=%v", found, err)
	}
	if err := os.Remove(filepath.Join(work, "runner")); err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("localhost/coopr-copy-test-%d-%d:acceptance", os.Getpid(), time.Now().UnixNano())
	copy := exec.CommandContext(ctx, cli, "copy", localRef, "podman:"+tag)
	var copyOutput, copyDiagnostics bytes.Buffer
	copy.Stdout = &copyOutput
	copy.Stderr = &copyDiagnostics
	err = copy.Run()
	if err != nil || strings.TrimSpace(copyOutput.String()) != "podman:"+tag {
		t.Fatalf("copy stored image into Podman: %v: stdout=%s stderr=%s", err, copyOutput.String(), copyDiagnostics.String())
	}
	t.Cleanup(func() {
		command := exec.Command("podman", "rmi", tag)
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("remove owned copied image %s: %v: %s", tag, err, output)
		}
	})
	output := string(loadPodman(t, ctx, "run", "--rm", "--network=none", tag))
	if strings.TrimSpace(output) != "loaded:"+marker {
		t.Fatalf("copied image did not run: %q", output)
	}
	assertNoStageDirs(t, work)
}

func TestStorageFailureCleansStagingAndDoesNotTag(t *testing.T) {
	loadTestBackend(t)
	if _, err := exec.LookPath("podman"); err != nil {
		t.Fatalf("Podman is required for native storage acceptance: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	work := t.TempDir()
	t.Setenv("TMPDIR", work)
	cli := storageTestCLI(t, ctx)
	file := filepath.Join(work, "app.coopr")
	if err := os.WriteFile(file, []byte("from \"scratch\"\ncopy \"marker\" \"/marker\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "marker"), []byte("ready\n"), 0600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	policyDir := filepath.Join(home, ".config", "containers")
	if err := os.MkdirAll(policyDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(policyDir, "policy.json"), []byte(`{"default":[{"type":"reject"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("localhost/coopr-storage-failure-%d-%d:test", os.Getpid(), time.Now().UnixNano())
	if output, err := exec.CommandContext(ctx, "podman", "image", "exists", tag).CombinedOutput(); err == nil {
		t.Fatalf("test tag already exists: %s", tag)
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("inspect test tag: %v: %s", err, output)
	}
	t.Cleanup(func() {
		if err := exec.Command("podman", "image", "exists", tag).Run(); err == nil {
			if output, err := exec.Command("podman", "rmi", tag).CombinedOutput(); err != nil {
				t.Errorf("remove unexpected test image: %v: %s", err, output)
			}
		}
	})
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		originalHome, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		dataHome = filepath.Join(originalHome, ".local", "share")
	}
	childEnv := []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "XDG_DATA_HOME=" + dataHome}
	_, err := runStorageCLI(ctx, cli, file, tag, childEnv)
	if err == nil || !strings.Contains(err.Error(), "policy") {
		t.Fatalf("policy rejection after build = %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "coopr.lock")); !os.IsNotExist(err) {
		t.Fatalf("failed native transfer created a project lockfile: %v", err)
	}
	assertNoStageDirs(t, work)
	if _, err := os.Stat(filepath.Join(work, "image.oci.tar")); !os.IsNotExist(err) {
		t.Fatalf("failed native storage build left archive: %v", err)
	}
	if output, err := exec.CommandContext(ctx, "podman", "image", "exists", tag).CombinedOutput(); err == nil {
		t.Fatalf("failed storage transfer created tag %s", tag)
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("inspect failed transfer tag: %v: %s", err, output)
	}
	// Failure during the build itself must precede the storage policy check.
	if err := os.Remove(filepath.Join(work, "marker")); err != nil {
		t.Fatal(err)
	}
	_, err = runStorageCLI(ctx, cli, file, tag, childEnv)
	if err == nil || strings.Contains(err.Error(), "policy") {
		t.Fatalf("expected build failure before storage policy, got %v", err)
	}
	assertNoStageDirs(t, work)
}

func loadTestBackend(t *testing.T) {
	t.Helper()
	if os.Getenv("COOPR_TEST_CONTAINER_STORAGE") != "1" {
		t.Skip("set COOPR_TEST_CONTAINER_STORAGE=1 for live native storage acceptance")
	}
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for live native storage acceptance")
	}
}

func storageTestCLI(t *testing.T, ctx context.Context) string {
	t.Helper()
	if path, supplied := os.LookupEnv("COOPR_TEST_CLI"); supplied {
		if err := validateStorageTestCLI(path); err != nil {
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

func validateStorageTestCLI(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("must be an absolute path to an executable file")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("must be an executable file: %s", path)
	}
	return nil
}

func runStorageCLI(ctx context.Context, binary, definition, tag string, extraEnv []string) (string, error) {
	args := []string{"build", definition, "--platform", "linux/" + runtime.GOARCH}
	if tag != "" {
		args = append(args, "--tag", "podman:"+tag)
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
	source := filepath.Join(work, "runner.go")
	program := "package main\nimport (\"fmt\"; \"os\"; \"strings\")\nfunc main() { data, err := os.ReadFile(\"/marker\"); if err != nil { panic(err) }; fmt.Print(\"loaded:\", strings.TrimSpace(string(data))) }\n"
	if err := os.WriteFile(source, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", filepath.Join(work, "runner"), source)
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build static runner: %v: %s", err, output)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
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
