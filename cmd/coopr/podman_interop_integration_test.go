package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestConfiguredStoreInteroperatesWithPodmanNativeNames(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live Podman store interoperability")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Fatal("live Podman store proof requires podman in the Nix development shell")
	}
	root := t.TempDir()
	configHome, dataHome := filepath.Join(root, "config"), filepath.Join(root, "data")
	graphRoot, runRoot := filepath.Join(root, "graph"), filepath.Join(root, "run")
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	podman := func(args ...string) string {
		t.Helper()
		args = append([]string{"--root", graphRoot, "--runroot", runRoot, "--storage-driver", "vfs", "--cgroup-manager", "cgroupfs", "--events-backend", "file", "--tmpdir", filepath.Join(root, "podman-tmp")}, args...)
		output, err := exec.CommandContext(ctx, "podman", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("podman %v: %v\n%s", args, err, output)
		}
		return string(output)
	}
	buildNativeBase := func(name, payload string, options ...string) {
		t.Helper()
		directory := filepath.Join(root, payload)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		writePodmanCompatibilityFile(t, filepath.Join(directory, "payload"), payload, 0o600)
		writePodmanCompatibilityFile(t, filepath.Join(directory, "Containerfile"), "FROM scratch\nCOPY payload /payload\n", 0o600)
		args := []string{"build", "--network=none", "--pull=never", "--layers=false", "--tag", name}
		args = append(args, options...)
		podman(append(args, directory)...)
	}
	const nativeBase = "docker.io/library/external-base:latest"
	buildNativeBase(nativeBase, "first")
	runPodmanCompatibilityCLI(t, "image", "inspect", "external-base")
	definition := filepath.Join(root, "application.coopr")
	writePodmanCompatibilityFile(t, definition, "from \"external-base\"\n", 0o600)
	for _, payload := range []string{"first", "second"} {
		if payload == "second" {
			buildNativeBase("docker.io/library/external-next:latest", payload)
			podman("tag", "docker.io/library/external-next:latest", nativeBase)
		}
		output := filepath.Join(root, payload+"-output")
		runPodmanCompatibilityCLI(t, "build", definition, "--pull=never", "--tag", "coopr-output", "--output", output, "--quiet")
		data, err := os.ReadFile(filepath.Join(output, "payload"))
		if err != nil || string(data) != payload {
			t.Fatalf("offline FROM after native retag: payload=%q error=%v want=%q", data, err, payload)
		}
		if tags := podman("image", "inspect", "coopr-output", "--format", "{{json .RepoTags}}"); !strings.Contains(tags, "localhost/coopr-output:latest") {
			t.Fatalf("Coopr output is missing its Podman-visible native name: %s", tags)
		}
	}
	listing := runPodmanCompatibilityCLI(t, "image", "ls")
	rows := 0
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "localhost/coopr-output:latest" {
			rows++
		}
		if fields[0] == "coopr-output:latest" || fields[0] == "external-base:latest" {
			t.Fatalf("synthetic lookup alias was listed as a native tag:\n%s", listing)
		}
	}
	if rows != 1 {
		t.Fatalf("native Coopr output has %d list rows, want one:\n%s", rows, listing)
	}
	buildNativeBase("localhost/external-base:latest", "local-choice")
	localOutput := filepath.Join(root, "local-choice-output")
	runPodmanCompatibilityCLI(t, "build", definition, "--pull=never", "--output", localOutput, "--quiet")
	if data, err := os.ReadFile(filepath.Join(localOutput, "payload")); err != nil || string(data) != "local-choice" {
		t.Fatalf("short-name collision did not prefer the local Podman name: payload=%q error=%v", data, err)
	}
	buildNativeBase("localhost/foreign-base:latest", "foreign", "--platform", "linux/"+otherPodmanCompatibilityArchitecture())
	podman("manifest", "create", "external-list", nativeBase, "localhost/foreign-base:latest")
	nativeDigest := strings.TrimSpace(podman("image", "inspect", "external-list", "--format", "{{.Digest}}"))
	if name := strings.TrimSpace(runPodmanCompatibilityCLI(t, "copy", "external-list", "external-list-copy")); name != "external-list-copy:latest" {
		t.Fatalf("native index copy returned unexpected destination %q", name)
	}
	copyDigest := strings.TrimSpace(podman("image", "inspect", "external-list-copy", "--format", "{{.Digest}}"))
	if !strings.HasPrefix(nativeDigest, "sha256:") || copyDigest != nativeDigest {
		t.Fatalf("native index copy changed root digest: native=%q copied=%q", nativeDigest, copyDigest)
	}
	nativeID := func(name string) string {
		t.Helper()
		var inspected []struct {
			ID string `json:"Id"`
		}
		if err := json.Unmarshal([]byte(podman("image", "inspect", name)), &inspected); err != nil || len(inspected) != 1 || inspected[0].ID == "" {
			t.Fatalf("native image inspect %q returned invalid identity: %+v, %v", name, inspected, err)
		}
		return inspected[0].ID
	}
	if sourceID, destinationID := nativeID("external-list"), nativeID("external-list-copy"); sourceID != destinationID {
		t.Fatalf("same-store copy created a second native manifest-list record: source=%s destination=%s", sourceID, destinationID)
	}
	var original, copied v1.Index
	if err := json.Unmarshal([]byte(podman("manifest", "inspect", "external-list")), &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(podman("manifest", "inspect", "external-list-copy")), &copied); err != nil {
		t.Fatal(err)
	}
	if len(original.Manifests) != 2 || len(copied.Manifests) != 2 {
		t.Fatalf("native index copy lost a platform: original=%+v copied=%+v", original.Manifests, copied.Manifests)
	}
	for i, descriptor := range original.Manifests {
		if descriptor.Digest != copied.Manifests[i].Digest {
			t.Fatalf("native index copy changed child %d: original=%s copied=%s", i, descriptor.Digest, copied.Manifests[i].Digest)
		}
	}

	podman("tag", nativeBase, "docker.io/library/native-only:latest")
	runPodmanCompatibilityCLI(t, "image", "rm", "native-only")
	args := []string{"--root", graphRoot, "--runroot", runRoot, "--storage-driver", "vfs", "--cgroup-manager", "cgroupfs", "--events-backend", "file", "--tmpdir", filepath.Join(root, "podman-tmp"), "image", "exists", "docker.io/library/native-only:latest"}
	if err := exec.CommandContext(ctx, "podman", args...).Run(); err == nil {
		t.Fatal("coopr image rm succeeded without removing the actual native-only Podman name")
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("Podman removal verification failed unexpectedly: %v", err)
	}
	podman("untag", nativeBase)
	podman("untag", "localhost/external-base:latest")
	var stdout, stderr bytes.Buffer
	if status := run([]string{"build", definition, "--pull=never", "--quiet"}, &stdout, &stderr); status == 0 {
		t.Fatal("offline FROM reused a stale alias after Podman removed the native name")
	}
	t.Log("Podman-built base, offline FROM, external retag/removal, native-only rm, native output inspect, exact native list rows, and complete native index copy passed")
}
