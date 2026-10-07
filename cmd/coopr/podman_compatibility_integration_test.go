package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestConfiguredPodmanStoreInvokesCompatibilityListsColdAndWarm(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for configured Podman-store compatibility coverage")
	}
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	dataHome := filepath.Join(root, "data")
	graphRoot := filepath.Join(root, "podman-graph")
	runRoot := filepath.Join(root, "podman-run")
	for _, directory := range []string{filepath.Join(configHome, "coopr"), dataHome} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", dataHome)
	storageConfig := filepath.Join(root, "storage.conf")
	writePodmanCompatibilityFile(t, storageConfig, fmt.Sprintf("[storage]\ndriver = \"vfs\"\ngraphroot = %q\nrunroot = %q\n", graphRoot, runRoot), 0o600)
	t.Setenv("CONTAINERS_STORAGE_CONF", storageConfig)
	t.Cleanup(func() { makePodmanCompatibilityStoreRemovable(t, graphRoot) })

	goodBase := writePodmanCompatibilityBase(t, root, "good-base", "ID=fedora\nVERSION_ID=42\n")
	runPodmanCompatibilityCLI(t, "build", goodBase, "--tag", "compatibility-base", "--platform", "linux/"+runtime.GOARCH, "--quiet")

	componentDir := filepath.Join(root, "component")
	if err := os.MkdirAll(componentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writePodmanCompatibilityFile(t, filepath.Join(componentDir, "component-payload"), "installed through compatible component\n", 0o600)
	componentDefinition := fmt.Sprintf(`package as="payload"
copy "component-payload" "/component-payload"
extend {
  distro "rhel" "fedora"
  distro-version "41" "42"
  package-manager "yum" "testpkg"
  architecture %q %q
}
copy "/component-payload" "/installed" from="payload"
`, otherPodmanCompatibilityArchitecture(), runtime.GOARCH)
	componentFile := filepath.Join(componentDir, "component.coopr")
	writePodmanCompatibilityFile(t, componentFile, componentDefinition, 0o600)
	componentRef := strings.TrimSpace(runPodmanCompatibilityCLI(t, "component", "build", componentFile, "--platform", "linux/"+runtime.GOARCH, "--quiet"))
	if !strings.HasPrefix(componentRef, "sha256:") {
		t.Fatalf("component build returned invalid immutable reference %q", componentRef)
	}

	application := filepath.Join(root, "application.coopr")
	writePodmanCompatibilityFile(t, application, "from \"compatibility-base\"\ncomponent \""+componentRef+"\"\n", 0o600)
	cacheDir := filepath.Join(root, "component-cache")
	for _, name := range []string{"cold", "warm"} {
		output := filepath.Join(root, name+"-output")
		runPodmanCompatibilityCLI(t, "build", application, "--pull=never", "--cache-from", "oci-layout:"+cacheDir, "--cache-to", "oci-layout:"+cacheDir, "--output", output, "--platform", "linux/"+runtime.GOARCH, "--quiet")
		data, err := os.ReadFile(filepath.Join(output, "installed"))
		if err != nil || string(data) != "installed through compatible component\n" {
			t.Fatalf("%s compatibility output = %q, %v", name, data, err)
		}
	}
	cacheIndex := filepath.Join(cacheDir, "index.json")
	cacheBeforeFailure, err := os.ReadFile(cacheIndex)
	if err != nil || len(cacheBeforeFailure) == 0 {
		t.Fatalf("portable component cache index = %q, %v", cacheBeforeFailure, err)
	}

	badBase := writePodmanCompatibilityBase(t, root, "bad-base", "ID=debian\nVERSION_ID=12\n")
	runPodmanCompatibilityCLI(t, "build", badBase, "--tag", "incompatible-base", "--platform", "linux/"+runtime.GOARCH, "--quiet")
	incompatible := filepath.Join(root, "incompatible.coopr")
	writePodmanCompatibilityFile(t, incompatible, "from \"incompatible-base\"\ncomponent \""+componentRef+"\"\n", 0o600)
	var stdout, stderr bytes.Buffer
	status := run([]string{"build", incompatible, "--pull=never", "--cache-from", "oci-layout:" + cacheDir, "--cache-to", "oci-layout:" + cacheDir, "--output", filepath.Join(root, "incompatible-output"), "--platform", "linux/" + runtime.GOARCH, "--quiet"}, &stdout, &stderr)
	if status == 0 || !strings.Contains(stderr.String(), "requires one of distros") {
		t.Fatalf("incompatible caller status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	cacheAfterFailure, err := os.ReadFile(cacheIndex)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cacheBeforeFailure, cacheAfterFailure) {
		t.Fatal("incompatible caller mutated the populated component cache")
	}
}

func writePodmanCompatibilityBase(t *testing.T, root, name, release string) string {
	t.Helper()
	directory := filepath.Join(root, name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writePodmanCompatibilityFile(t, filepath.Join(directory, "os-release"), release, 0o600)
	writePodmanCompatibilityFile(t, filepath.Join(directory, "testpkg"), "test package manager\n", 0o700)
	definition := filepath.Join(directory, "image.coopr")
	writePodmanCompatibilityFile(t, definition, "from \"scratch\"\ncopy \"os-release\" \"/etc/os-release\"\ncopy \"testpkg\" \"/usr/bin/testpkg\" chmod=\"0755\"\n", 0o600)
	return definition
}

func runPodmanCompatibilityCLI(t *testing.T, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if status := run(args, &stdout, &stderr); status != 0 {
		t.Fatalf("coopr %v failed: stdout=%q stderr=%q", args, stdout.String(), stderr.String())
	}
	return stdout.String()
}

func writePodmanCompatibilityFile(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func makePodmanCompatibilityStoreRemovable(t *testing.T, root string) {
	t.Helper()
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.Chmod(path, info.Mode()|0o700)
	}); err != nil && !os.IsNotExist(err) {
		t.Errorf("make Podman test graph removable: %v", err)
	}
}

func otherPodmanCompatibilityArchitecture() string {
	if runtime.GOARCH == "amd64" {
		return "arm64"
	}
	return "amd64"
}
