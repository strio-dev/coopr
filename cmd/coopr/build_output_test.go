package main

import (
	"bytes"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
)

func TestBuildArchiveCommandLive(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for live Buildah tests")
	}
	file := definitionFile(t, "from \"scratch\"\nlabel from_cli=\"yes\"\n")
	archive := filepath.Join(t.TempDir(), "image.oci.tar")
	var output, stderr bytes.Buffer
	if code := run([]string{"build", file, "--tag", "oci-archive:" + archive, "--platform", "linux/amd64"}, &output, &stderr); code != 0 {
		t.Fatalf("CLI image archive failed: %s", stderr.String())
	}
	if got := strings.TrimSpace(output.String()); got != archive {
		t.Fatalf("CLI image archive printed %q, want %q", got, archive)
	}
	if info, err := os.Stat(archive); err != nil || info.Size() == 0 {
		t.Fatalf("image archive is missing or empty: %v", err)
	}
}

func TestBuildCommandRunsDefaultNetworkInConcurrentStageWorker(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for live Buildah tests")
	}
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatal(err)
	}
	file := definitionFile(t, "from \"scratch\"\ncopy \"busybox\" \"/busybox\"\nrun { exec \"/busybox\" \"touch\" \"/proof\" }\n")
	data, err := os.ReadFile(busybox)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(file), "busybox"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "networked.oci.tar")
	var output, stderr bytes.Buffer
	if code := run([]string{"build", file, "--tag", "oci-archive:" + archive, "--platform", "linux/amd64"}, &output, &stderr); code != 0 {
		t.Fatalf("CLI default-network RUN failed: %s", stderr.String())
	}
	if info, err := os.Stat(archive); err != nil || info.Size() == 0 {
		t.Fatalf("CLI default-network output is missing: %v", err)
	}
}

func TestBuildOutputFlags(t *testing.T) {
	file := definitionFile(t, `from "scratch"`)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"removed load flag", []string{"--load"}, "unknown flag"},
		{"invalid filesystem output", []string{"--output", "type=oci,dest=out"}, "output requires"},
		{"invalid output shorthand", []string{"-o", "type=local,dest=-"}, "output requires"},
		{"removed arg flag", []string{"--arg", "name=value"}, "unknown flag"},
		{"empty tag", []string{"--tag", ""}, "--tag requires a nonempty image name"},
		{"empty archive destination", []string{"--tag", "oci-archive:"}, "oci-archive destination requires a name"},
		{"push without tag", []string{"--push"}, "--push requires --tag"},
		{"malformed tag", []string{"--tag", "bad tag"}, "invalid local image"},
		{"digest as tag", []string{"-t", "localhost/app@sha256:" + strings.Repeat("a", 64)}, "digest"},
		{"invalid format", []string{"--format", "made-up"}, "unsupported image format"},
		{"invalid network", []string{"--network", "bad/network"}, "invalid named build network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			args := append([]string{"build", file}, tc.args...)
			if status := run(args, &out, &errOut); status == 0 || !strings.Contains(errOut.String(), tc.want) {
				t.Fatalf("status %d stderr %q; want %q", status, errOut.String(), tc.want)
			}
			if out.Len() != 0 {
				t.Fatalf("failed build printed success: %q", out.String())
			}
		})
	}
}

func TestBuildUsesConventionalBuildArgumentFlag(t *testing.T) {
	cmd := newBuildCommand()
	if cmd.Flags().Lookup("build-arg") == nil {
		t.Fatal("missing --build-arg")
	}
	for _, removed := range []string{"arg", "builder-address"} {
		if cmd.Flags().Lookup(removed) != nil {
			t.Fatalf("removed flag --%s is still registered", removed)
		}
	}
}

func TestBuildPlatformFlagAcceptsRepeatedTargets(t *testing.T) {
	cmd := newBuildCommand()
	for _, value := range []string{"linux/amd64", "linux/arm64"} {
		if err := cmd.Flags().Set("platform", value); err != nil {
			t.Fatal(err)
		}
	}
	values, err := cmd.Flags().GetStringArray("platform")
	if err != nil || strings.Join(values, ",") != "linux/amd64,linux/arm64" {
		t.Fatalf("repeated --platform = %v, %v", values, err)
	}
}

func TestBuildHelpExplainsOutputModes(t *testing.T) {
	var out, errOut bytes.Buffer
	if status := run([]string{"build", "--help"}, &out, &errOut); status != 0 {
		t.Fatalf("help failed: %s", errOut.String())
	}
	for _, text := range []string{"--tag", "--build-arg", "--format", "--push", "--pull", "--no-cache", "--network", "--add-host", "--allow", "--output", "--squash", "--squash-all", "--sbom", "--sign-by", "--sign-by-sigstore-private-key", "--manifest", "--all-platforms", "--cache-ttl", "--timestamp", "--unsetenv", "network.host", "security.insecure", "save fresh results to the build cache", "base image pull policy", "native container storage", "docker:", "oci-archive:", "embedded Buildah"} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("help missing %q", text)
		}
	}
	if strings.Contains(out.String(), "--load") || strings.Contains(out.String(), "--arg") || strings.Contains(out.String(), "nerdctl:") || strings.Contains(out.String(), "CONTAINER_HOST") {
		t.Fatalf("help still advertises the removed runtime load mode: %s", out.String())
	}
	if strings.Contains(out.String(), "(default name[=value])") {
		t.Fatalf("help shows an invented build argument default: %s", out.String())
	}
}

func TestBuildPushCommandLive(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH_REGISTRY") == "" {
		t.Skip("set COOPR_TEST_BUILDAH_REGISTRY for live Buildah registry tests")
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	target := strings.TrimPrefix(server.URL, "http://") + "/coopr/image:stable"
	file := definitionFile(t, "from \"scratch\"\nlabel from_cli=\"yes\"\n")
	var out, errOut bytes.Buffer
	args := []string{"build", file, "--push", "--tag", target, "--tls-verify=false", "--platform", "linux/amd64"}
	if status := run(args, &out, &errOut); status != 0 {
		t.Fatalf("CLI image push failed: %s", errOut.String())
	}
	if got := strings.TrimSpace(out.String()); !strings.HasPrefix(got, strings.TrimSuffix(target, ":stable")+"@sha256:") {
		t.Fatalf("CLI image push did not print an immutable reference: %q", got)
	}
}
