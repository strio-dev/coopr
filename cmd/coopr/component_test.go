package main

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/spf13/cobra"
	"go.podman.io/image/v5/pkg/sysregistriesv2"
)

func TestComponentBuildCommandValidation(t *testing.T) {
	file := definitionFile(t, "extend as=\"base\"\n")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"push needs tag", []string{"component", "build", file, "--push"}, "--push requires --tag"},
		{"empty tag", []string{"component", "build", file, "--tag", ""}, "--tag requires a nonempty component name"},
		{"removed output", []string{"component", "build", file, "--output", "out.tar"}, "unknown flag"},
		{"removed output shorthand", []string{"component", "build", file, "-o", "out.tar"}, "unknown shorthand"},
		{"removed arg", []string{"component", "build", file, "--arg", "name=value"}, "unknown flag"},
		{"removed publish command", []string{"component", "publish", file}, "unknown command"},
		{"removed lock flag", []string{"component", "build", file, "--frozen"}, "unknown flag"},
		{"removed builder address", []string{"component", "build", file, "--builder-address", ""}, "unknown flag"},
		{"invalid network", []string{"component", "build", file, "--network", "bad/network"}, "invalid named build network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output, stderr bytes.Buffer
			if code := run(tc.args, &output, &stderr); code == 0 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("status %d, stderr %q; want %q", code, stderr.String(), tc.want)
			}
			if output.Len() != 0 {
				t.Fatalf("failed command wrote stdout %q", output.String())
			}
		})
	}
}

func TestProjectLockFlagsAreAbsent(t *testing.T) {
	for _, cmd := range []*cobra.Command{newBuildCommand(), newComponentBuildCommand()} {
		for _, name := range []string{"lockfile", "frozen", "update-lock"} {
			if cmd.Flags().Lookup(name) != nil {
				t.Fatalf("%s still exposes --%s", cmd.CommandPath(), name)
			}
		}
		if cmd.Flags().Lookup("build-arg") == nil {
			t.Fatalf("%s does not expose --build-arg", cmd.CommandPath())
		}
		for _, removed := range []string{"arg", "builder-address"} {
			if cmd.Flags().Lookup(removed) != nil {
				t.Fatalf("%s still exposes --%s", cmd.CommandPath(), removed)
			}
		}
	}
}

func TestBuildCommandsAcceptPull(t *testing.T) {
	for _, cmd := range []*cobra.Command{newBuildCommand(), newComponentBuildCommand()} {
		flag := cmd.Flags().Lookup("pull")
		if flag == nil {
			t.Fatalf("%s does not expose --pull", cmd.CommandPath())
		}
		if flag.DefValue != "missing" {
			t.Fatalf("%s --pull default = %q, want missing", cmd.CommandPath(), flag.DefValue)
		}
		if err := cmd.ParseFlags([]string{"--pull"}); err != nil {
			t.Fatalf("%s parse --pull: %v", cmd.CommandPath(), err)
		}
		got, err := cmd.Flags().GetString("pull")
		if err != nil {
			t.Fatalf("%s read --pull: %v", cmd.CommandPath(), err)
		}
		if got != "always" {
			t.Fatalf("%s --pull policy = %q, want always", cmd.CommandPath(), got)
		}
	}
}

func TestBuildCommandsAcceptNoCache(t *testing.T) {
	for _, cmd := range []*cobra.Command{newBuildCommand(), newComponentBuildCommand()} {
		flag := cmd.Flags().Lookup("no-cache")
		if flag == nil {
			t.Fatalf("%s does not expose --no-cache", cmd.CommandPath())
		}
		if flag.DefValue != "false" {
			t.Fatalf("%s --no-cache default = %q, want false", cmd.CommandPath(), flag.DefValue)
		}
		if err := cmd.ParseFlags([]string{"--no-cache"}); err != nil {
			t.Fatalf("%s parse --no-cache: %v", cmd.CommandPath(), err)
		}
		got, err := cmd.Flags().GetBool("no-cache")
		if err != nil {
			t.Fatalf("%s read --no-cache: %v", cmd.CommandPath(), err)
		}
		if !got {
			t.Fatalf("%s --no-cache was not enabled", cmd.CommandPath())
		}
	}
}

func TestBuildCommandsAcceptNetworkAndAddHost(t *testing.T) {
	for _, factory := range []func() *cobra.Command{newBuildCommand, newComponentBuildCommand} {
		for _, mode := range []string{"default", "none", "host"} {
			cmd := factory()
			if got := cmd.Flags().Lookup("network"); got == nil || got.DefValue != "default" {
				t.Fatalf("%s --network flag = %#v", cmd.CommandPath(), got)
			}
			if err := cmd.ParseFlags([]string{"--network", mode, "--add-host", "one.test:127.0.0.1", "--add-host", "two.test:127.0.0.2"}); err != nil {
				t.Fatalf("%s parse network options: %v", cmd.CommandPath(), err)
			}
			gotMode, _ := cmd.Flags().GetString("network")
			gotHosts, _ := cmd.Flags().GetStringArray("add-host")
			if gotMode != mode || strings.Join(gotHosts, ",") != "one.test:127.0.0.1,two.test:127.0.0.2" {
				t.Fatalf("%s network options = %q, %v", cmd.CommandPath(), gotMode, gotHosts)
			}
		}
	}
}

func TestBuildCommandsExposeCredentialAndCacheInputs(t *testing.T) {
	for _, cmd := range []*cobra.Command{newBuildCommand(), newComponentBuildCommand()} {
		for _, name := range []string{"secret", "ssh", "cache-from", "cache-to", "allow", "build-context"} {
			if cmd.Flags().Lookup(name) == nil {
				t.Errorf("%s does not expose --%s", cmd.CommandPath(), name)
			}
		}
	}
}

func TestComponentBuildArgHelpHasNoDefault(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"component", "build", "--help"}, &out, &errOut); code != 0 {
		t.Fatalf("component help failed: %s", errOut.String())
	}
	if strings.Contains(out.String(), "(default name[=value])") {
		t.Fatalf("help shows an invented build argument default: %s", out.String())
	}
	for _, text := range []string{"--pull", "base image pull policy", "--no-cache", "save fresh results to the build cache", "--network", "--add-host", "--secret", "--ssh", "--cache-from", "--cache-to", "--allow", "network.host", "security.insecure"} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("component help missing %q", text)
		}
	}
}

func TestComponentBuildPushCommandLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping _REGISTRY for live Buildah registry tests in short mode")
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	ref := strings.TrimPrefix(server.URL, "http://") + "/coopr/cli:stable"
	file := definitionFile(t, "extend as=\"base\"\nenv FROM_CLI=\"yes\"\n")
	var output, stderr bytes.Buffer
	code := run([]string{"component", "build", file, "--push", "--tag", ref, "--tls-verify=false", "--platform", "linux/amd64"}, &output, &stderr)
	if code != 0 {
		t.Fatalf("CLI publication failed: %s", stderr.String())
	}
	if got := strings.TrimSpace(output.String()); !strings.HasPrefix(got, strings.TrimSuffix(ref, ":stable")+"@sha256:") {
		t.Fatalf("CLI did not print immutable reference: %q", got)
	}
}

func TestComponentBuildRegistryTLSPolicyCommandLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping _REGISTRY for live Buildah registry tests in short mode")
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	config := filepath.Join(t.TempDir(), "registries.conf")
	if err := os.WriteFile(config, []byte("[[registry]]\nlocation = \""+host+"\"\ninsecure = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_REGISTRIES_CONF", config)
	sysregistriesv2.InvalidateCache()
	t.Cleanup(sysregistriesv2.InvalidateCache)
	file := definitionFile(t, "extend\nenv registry_policy=\"native\"\n")
	for _, test := range []struct {
		name        string
		flag        string
		wantSuccess bool
	}{
		{"configured", "", true},
		{"insecure", "--tls-verify=false", true},
		{"strict", "--tls-verify=true", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"component", "build", file, "--push", "--tag", host + "/coopr/policy:" + test.name, "--retry", "0"}
			if test.flag != "" {
				args = append(args, test.flag)
			}
			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr)
			if (code == 0) != test.wantSuccess {
				t.Fatalf("status %d, stderr: %s", code, stderr.String())
			}
		})
	}
}

func TestComponentBuildArchiveCommandLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Buildah tests in short mode")
	}
	file := definitionFile(t, "package as=\"payload\"\ncopy \"source\" \"/payload\"\nextend as=\"base\"\ncopy \"/payload\" \"/installed\" from=\"payload\"\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(file), "source"), []byte("archived\n"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "component.oci.tar")
	var output, stderr bytes.Buffer
	if code := run([]string{"component", "build", file, "--tag", "oci-archive:" + archive, "--platform", "linux/amd64"}, &output, &stderr); code != 0 {
		t.Fatalf("CLI component archive failed: %s", stderr.String())
	}
	if got := strings.TrimSpace(output.String()); got != archive {
		t.Fatalf("CLI component archive printed %q, want %q", got, archive)
	}
	if info, err := os.Stat(archive); err != nil || info.Size() == 0 {
		t.Fatalf("component archive is missing or empty: %v", err)
	}
}

func TestComponentBuildLocalThenInvokeCommandLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Buildah tests in short mode")
	}
	dataDir := t.TempDir()
	t.Cleanup(func() {
		// Committed overlay layer directories can be read-only. Make them
		// removable before TempDir's cleanup runs.
		if err := filepath.WalkDir(dataDir, func(path string, entry os.DirEntry, walkErr error) error {
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
		}); err != nil {
			t.Errorf("make image graph removable: %v", err)
		}
	})
	t.Setenv("XDG_DATA_HOME", dataDir)
	component := definitionFile(t, "package as=\"pkg\"\ncopy \"source\" \"/payload\"\nextend as=\"base\"\ncopy \"/payload\" \"/installed\" from=\"pkg\"\n")
	source := filepath.Join(filepath.Dir(component), "source")
	if err := os.WriteFile(source, []byte("local component\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var output, stderr bytes.Buffer
	if code := run([]string{"component", "build", component, "--platform", "linux/amd64"}, &output, &stderr); code != 0 {
		t.Fatalf("local component build failed: %s", stderr.String())
	}
	ref := strings.TrimSpace(output.String())
	if !strings.HasPrefix(ref, "sha256:") {
		t.Fatalf("component build did not print an immutable local reference: %q", ref)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	app := definitionFile(t, "from \"scratch\"\ncomponent \""+ref+"\"\n")
	archive := filepath.Join(t.TempDir(), "app.oci.tar")
	cacheDir := filepath.Join(dataDir, "component-cache")
	output.Reset()
	stderr.Reset()
	if code := run([]string{"build", app, "--tag", "oci-archive:" + archive, "--cache-from", "oci-layout:" + cacheDir, "--cache-to", "oci-layout:" + cacheDir, "--platform", "linux/amd64"}, &output, &stderr); code != 0 {
		t.Fatalf("local component invocation failed: %s", stderr.String())
	}
	if info, err := os.Stat(archive); err != nil || info.Size() == 0 {
		t.Fatalf("invocation did not produce an OCI archive: %v", err)
	}
	if info, err := os.Stat(filepath.Join(cacheDir, "index.json")); err != nil || info.Size() == 0 {
		t.Fatalf("CLI cache option did not populate the OCI layout: %v", err)
	}
	secondArchive := filepath.Join(t.TempDir(), "cached-app.oci.tar")
	output.Reset()
	stderr.Reset()
	if code := run([]string{"build", app, "--tag", "oci-archive:" + secondArchive, "--cache-from", "oci-layout:" + cacheDir, "--cache-to", "oci-layout:" + cacheDir, "--platform", "linux/amd64"}, &output, &stderr); code != 0 {
		t.Fatalf("second component invocation with CLI cache failed: %s", stderr.String())
	}
	if info, err := os.Stat(secondArchive); err != nil || info.Size() == 0 {
		t.Fatalf("second invocation did not produce an OCI archive: %v", err)
	}
}
