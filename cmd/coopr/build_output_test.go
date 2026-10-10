package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/oci"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestBuildArchiveCommandLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Buildah tests in short mode")
	}
	file := definitionFile(t, "from \"scratch\"\nlabel from_cli=\"yes\"\n")
	archive := filepath.Join(t.TempDir(), "image.oci.tar")
	var output, stderr bytes.Buffer
	if code := run([]string{"build", file, "--tag", "oci-archive:" + archive, "--platform", "linux/amd64"}, &output, &stderr); code != 0 {
		t.Fatalf("CLI image archive failed: %s", stderr.String())
	}
	config, err := oci.ArchiveImageConfigDigest(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(output.String()); got != config.Encoded() {
		t.Fatalf("CLI image archive printed %q, want native image ID %q", got, config.Encoded())
	}
	if info, err := os.Stat(archive); err != nil || info.Size() == 0 {
		t.Fatalf("image archive is missing or empty: %v", err)
	}
}

func TestBuildCommandResultAndIIDFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Buildah tests in short mode")
	}
	file := definitionFile(t, "from \"scratch\"\nlabel result=\"native\"\n")
	for _, option := range []string{"", "--iidfile", "--iidfile-raw", "--raw-iidfile"} {
		t.Run(option, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := []string{"build", file, "--quiet", "--platform", "linux/amd64"}
			iid := filepath.Join(t.TempDir(), "iid")
			if option != "" {
				args = append(args, option, iid)
			}
			if code := run(args, &stdout, &stderr); code != 0 {
				t.Fatalf("build status=%d stderr=%s", code, &stderr)
			}
			value := strings.TrimSpace(stdout.String())
			if option != "" {
				if stdout.Len() != 0 {
					t.Fatalf("iidfile build printed result %q", &stdout)
				}
				data, err := os.ReadFile(iid)
				if err != nil {
					t.Fatal(err)
				}
				value = strings.TrimPrefix(string(data), "sha256:")
			}
			if decoded, err := hex.DecodeString(value); err != nil || len(decoded) != 32 {
				t.Fatalf("not a native image ID: %q", value)
			}
			var inspected bytes.Buffer
			if code := run([]string{"image", "inspect", value}, &inspected, &stderr); code != 0 {
				t.Fatalf("result is not a stored image: %s", &stderr)
			}
		})
	}
}

func TestBuildCommandTarStdoutKeepsResultOnStderr(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Buildah tests in short mode")
	}
	file := definitionFile(t, "from \"scratch\"\ncopy \"payload\" \"/payload\"\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(file), "payload"), []byte("archive payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"build", file, "--quiet", "--output", "type=tar,dest=-", "--platform", "linux/amd64"}, &stdout, &stderr); code != 0 {
		t.Fatalf("tar build status=%d: %s", code, &stderr)
	}
	reader := tar.NewReader(&stdout)
	found := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimPrefix(header.Name, "./") == "payload" {
			data, err := io.ReadAll(reader)
			if err != nil || string(data) != "archive payload" {
				t.Fatalf("tar payload=%s err=%v", data, err)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("stdout tar omitted payload")
	}
	if decoded, err := hex.DecodeString(strings.TrimSpace(stderr.String())); err != nil || len(decoded) != 32 {
		t.Fatalf("stderr is not raw result ID: %q", &stderr)
	}
}

func TestBuildCommandRunsDefaultNetworkInConcurrentStageWorker(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Buildah tests in short mode")
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
	if testing.Short() {
		t.Skip("skipping _REGISTRY for live Buildah registry tests in short mode")
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
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false), NativeStore: buildah.NativeStoreOptions(maintenanceStoreOptions(t.TempDir()))})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(context.Background(), target, v1.Platform{OS: "linux", Architecture: "amd64"}, oci.Image)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(out.String())
	if got != resolved.Config.Digest.Encoded() {
		t.Fatalf("CLI push result %q, want native image ID %s", got, resolved.Config.Digest.Encoded())
	}
	var inspected bytes.Buffer
	if status := run([]string{"image", "inspect", got}, &inspected, &errOut); status != 0 {
		t.Fatalf("pushed result not retained in native storage: %s", &errOut)
	}
}

func TestBuildCommandQuietLogfileContainsOnlyResult(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Buildah tests in short mode")
	}
	file := definitionFile(t, "from \"scratch\"\nlabel result=\"logfile\"\n")
	for _, iidOption := range []string{"", "--iidfile", "--iidfile-raw"} {
		t.Run(iidOption, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "build.log")
			var stdout, stderr bytes.Buffer
			args := []string{"build", file, "--quiet", "--logfile", log}
			if iidOption != "" {
				args = append(args, iidOption, filepath.Join(t.TempDir(), "iid"))
			}
			if code := run(args, &stdout, &stderr); code != 0 {
				t.Fatalf("status=%d stderr=%s", code, &stderr)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("logfile build leaked terminal output: stdout=%q stderr=%q", &stdout, &stderr)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if iidOption != "" {
				if len(data) != 0 {
					t.Fatalf("IID build logged final ID: %q", data)
				}
			} else if decoded, err := hex.DecodeString(strings.TrimSpace(string(data))); err != nil || len(decoded) != 32 {
				t.Fatalf("logfile is not final image ID only: %q", data)
			}
		})
	}
}

func TestBuildCommandPreservesRUNFailureExitAndSingleDiagnostic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Buildah tests in short mode")
	}
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatal(err)
	}
	file := definitionFile(t, "from \"scratch\"\ncopy \"busybox\" \"/busybox\"\nrun { exec \"/busybox\" \"sh\" \"-c\" \"exit 42\" }\n")
	data, err := os.ReadFile(busybox)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(file), "busybox"), data, 0755); err != nil {
		t.Fatal(err)
	}
	for _, jobs := range []string{"1", "2"} {
		t.Run("jobs="+jobs, func(t *testing.T) {
			if jobs == "2" {
				source := `from "scratch" as="failure"
copy "busybox" "/busybox"
run { exec "/busybox" "sh" "-c" "exit 42" }
from "scratch" as="sibling"
copy "busybox" "/busybox"
run { exec "/busybox" "sleep" "2" }
from "scratch"
copy "/busybox" "/failure" from="failure"
copy "/busybox" "/sibling" from="sibling"
`
				if err := os.WriteFile(file, []byte(source), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			code := run([]string{"build", file, "--quiet", "--no-cache", "--network=none", "--isolation=rootless", "--jobs", jobs}, &stdout, &stderr)
			if code != 42 {
				t.Fatalf("status=%d stdout=%q stderr=%q", code, &stdout, &stderr)
			}
			if stdout.Len() != 0 || strings.Count(stderr.String(), "Error: ") != 1 || strings.Count(stderr.String(), "exit status 42") != 1 {
				t.Fatalf("duplicated failure diagnostic: stdout=%q stderr=%q", &stdout, &stderr)
			}
		})
	}
}

func TestBuildCommandUnusedArgumentsFollowQuietAndLogRouting(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Buildah tests in short mode")
	}
	file := definitionFile(t, "arg \"used\"\nfrom \"scratch\"\nlabel result=\"arguments\"\n")
	for _, mode := range []string{"terminal", "log", "quiet", "split"} {
		t.Run(mode, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "build.log")
			args := []string{"build", file, "--build-arg", "used=private", "--build-arg", "unused=private", "--build-arg", "HTTP_PROXY=private"}
			if mode == "log" || mode == "split" {
				args = append(args, "--logfile", log)
			}
			if mode == "quiet" {
				args = append(args, "--quiet")
			}
			if mode == "split" {
				args = append(args, "--logsplit", "--platform", "linux/amd64")
			}
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 0 {
				t.Fatalf("status=%d stderr=%s", code, &stderr)
			}
			output := stdout.String() + stderr.String()
			if mode == "log" || mode == "split" {
				if output != "" {
					t.Fatalf("logfile build leaked terminal output: %q", output)
				}
				if mode == "split" {
					log += "_linux_amd64"
				}
				data, err := os.ReadFile(log)
				if err != nil {
					t.Fatal(err)
				}
				output = string(data)
			}
			want := mode != "quiet"
			if strings.Contains(output, "[Warning] one or more build args were not consumed: [unused]") != want || strings.Contains(output, "private") {
				t.Fatalf("warning routing or value exposure: %q", output)
			}
		})
	}
}

func TestComponentBuildCommandQuietLogfileContainsReference(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Buildah tests in short mode")
	}
	file := definitionFile(t, "extend\nenv proof=\"component\"\n")
	log := filepath.Join(t.TempDir(), "component.log")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"component", "build", file, "--quiet", "--logfile", log}, &stdout, &stderr); code != 0 {
		t.Fatalf("status=%d stderr=%s", code, &stderr)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("component logfile leaked terminal output: stdout=%q stderr=%q", &stdout, &stderr)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "sha256:") || strings.Count(string(data), "\n") != 1 {
		t.Fatalf("component reference not retained: %q", data)
	}
}
