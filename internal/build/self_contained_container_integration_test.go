package build

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain changes XDG_DATA_HOME for Coopr tests. Podman must retain the
// invoking user's environment to find the image loaded before the test.
var hostPodmanEnvironment = os.Environ()

// This opt-in test uses the shipped scratch image. Separate CLI invocations
// share only Coopr's persisted image and component stores.
func TestSelfContainedContainerBuildsAndReusesLocalState(t *testing.T) {
	image := os.Getenv("COOPR_TEST_SELF_CONTAINED_IMAGE")
	if image == "" {
		t.Skip("set COOPR_TEST_SELF_CONTAINED_IMAGE for the self-contained container profile")
	}
	if os.Geteuid() == 0 {
		t.Fatal("the self-contained container profile requires a rootless host user")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	state := filepath.Join(root, "state")
	for _, path := range []string{workspace, state} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		// Image layers contain subordinate UID/GID ownership. Remove this
		// test's store in Podman's user namespace before TempDir cleanup.
		command := exec.Command("podman", "unshare", "rm", "-rf", "--", state)
		command.Env = hostPodmanEnv()
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("remove test-owned container state: %v: %s", err, output)
		}
	})
	component := "extend\nenv SELF_CONTAINED_PROOF=ready\n"
	if err := os.WriteFile(filepath.Join(workspace, "component.coopr"), []byte(component), 0600); err != nil {
		t.Fatal(err)
	}
	definition := fmt.Sprintf("from %q\ncomponent \"local:proof\"\nrun \"test \\\"$SELF_CONTAINED_PROOF\\\" = ready && printf self-contained-ok > /proof\"\ncmd { exec \"/bin/sh\" \"-c\" \"cat /proof\" }\n", exampleBase)
	if err := os.WriteFile(filepath.Join(workspace, "app.coopr"), []byte(definition), 0600); err != nil {
		t.Fatal(err)
	}

	if output := runSelfContainedCLI(t, ctx, image, workspace, state, "none",
		"component", "build", "/work/component.coopr", "--tag", "proof", "--platform", "linux/amd64"); strings.TrimSpace(output) != "proof" {
		t.Fatalf("component was not stored locally: %s", output)
	}
	runSelfContainedCLI(t, ctx, image, workspace, state, "",
		"build", "/work/app.coopr", "--tag", "oci-archive:/work/online.oci.tar", "--platform", "linux/amd64")
	// Restart Coopr with no network. The base image and component must come
	// from the mounted local stores.
	runSelfContainedCLI(t, ctx, image, workspace, state, "none",
		"build", "/work/app.coopr", "--tag", "oci-archive:/work/offline.oci.tar", "--platform", "linux/amd64")
	// Chroot reuses the outer network. It must not launch a nested slirp
	// helper or replace DNS with an address only that helper can serve.
	runSelfContainedCLI(t, ctx, image, workspace, state, "none",
		"build", "/work/app.coopr", "--network=slirp4netns", "--no-cache",
		"--tag", "oci-archive:/work/chroot.oci.tar", "--platform", "linux/amd64")
	for _, path := range []string{
		filepath.Join(state, "containers", "storage"),
		filepath.Join(state, "coopr", "components"),
	} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("persistent state directory %s: %v", path, err)
		}
	}
	archive := filepath.Join(workspace, "offline.oci.tar")
	manifest, config := readExampleImage(t, ctx, archive)
	if len(manifest.Layers) == 0 || !hasEnv(config.Config.Env, "SELF_CONTAINED_PROOF=ready") {
		t.Fatalf("offline result lost filesystem or component configuration: layers=%d env=%v", len(manifest.Layers), config.Config.Env)
	}
	imageID := manifest.Config.Digest.String()
	exists := exec.CommandContext(ctx, "podman", "image", "exists", imageID)
	exists.Env = hostPodmanEnv()
	preexisting := exists.Run() == nil
	load := exec.CommandContext(ctx, "podman", "load", "-i", archive)
	load.Env = hostPodmanEnv()
	if output, err := load.CombinedOutput(); err != nil {
		t.Fatalf("load built image into rootless Podman: %v: %s", err, output)
	}
	if !preexisting {
		t.Cleanup(func() {
			command := exec.Command("podman", "rmi", imageID)
			command.Env = hostPodmanEnv()
			if output, err := command.CombinedOutput(); err != nil {
				t.Errorf("remove test-owned image: %v: %s", err, output)
			}
		})
	}
	run := exec.CommandContext(ctx, "podman", "run", "--rm", "--network=none", imageID)
	run.Env = hostPodmanEnv()
	if output, err := run.CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "self-contained-ok" {
		t.Fatalf("run built image offline: %v: %s", err, output)
	}
}

func runSelfContainedCLI(t *testing.T, ctx context.Context, image, workspace, state, network string, args ...string) string {
	t.Helper()
	command := []string{"run", "--rm"}
	if network != "" {
		command = append(command, "--network="+network)
	}
	command = append(command,
		"--device=/dev/fuse:rw", "--security-opt=seccomp=unconfined",
		"--security-opt=label=disable",
		"--mount", "type=bind,src="+workspace+",dst=/work,rw",
		"--mount", "type=bind,src="+state+",dst=/var/lib,rw",
		image,
	)
	command = append(command, args...)
	cmd := exec.CommandContext(ctx, "podman", command...)
	cmd.Env = hostPodmanEnv()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("self-contained Coopr %q: %v: %s", args, err, output)
	}
	return string(output)
}

func hostPodmanEnv() []string { return hostPodmanEnvironment }
