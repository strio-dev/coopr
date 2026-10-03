//go:build linux

package buildah

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
)

func TestInsecureRunRejectsChrootIsolation(t *testing.T) {
	builder := nativeBuilder{isolation: define.IsolationChroot}
	err := builder.run([]string{"true"}, upstream.RunOptions{Args: []string{insecureRunRequestedMarker}})
	if err == nil || !strings.Contains(err.Error(), "requires OCI or rootless isolation") {
		t.Fatalf("insecure chroot RUN error = %v", err)
	}
}

func TestResolveInsecureRuntimeFromPATH(t *testing.T) {
	runtime := filepath.Join(t.TempDir(), "coopr-test-oci-runtime")
	if err := os.WriteFile(runtime, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(runtime))
	got, err := resolveInsecureRuntime(filepath.Base(runtime))
	if err != nil {
		t.Fatal(err)
	}
	if got != runtime {
		t.Fatalf("resolved runtime = %q, want %q", got, runtime)
	}
}

func TestMakeInsecureRuntimeSpecMatchesBuildKitSecurityMode(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	spec := specs.Spec{
		Process: &specs.Process{
			ApparmorProfile: "container-default",
			SelinuxLabel:    "process-label",
			NoNewPrivileges: true,
			Capabilities: &specs.LinuxCapabilities{
				Bounding: []string{"CAP_CHOWN"}, Ambient: []string{"CAP_KILL"}, Effective: []string{"CAP_FOWNER"}, Inheritable: []string{"CAP_SETUID"}, Permitted: []string{"CAP_SETGID"},
			},
		},
		Linux: &specs.Linux{
			MountLabel:    "mount-label",
			MaskedPaths:   []string{"/proc/kcore"},
			ReadonlyPaths: []string{"/proc/sys"},
			Resources:     &specs.LinuxResources{Devices: []specs.LinuxDeviceCgroup{{Allow: false, Access: "rwm"}}},
			Seccomp:       &specs.LinuxSeccomp{DefaultAction: specs.ActErrno},
		},
		Mounts: []specs.Mount{
			{Type: "sysfs", Options: []string{"nosuid", "ro"}},
			{Type: "cgroup", Options: []string{"ro", "nosuid"}},
			{Type: "bind", Options: []string{"ro"}},
		},
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := makeInsecureRuntimeSpec(configPath); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
	updatedData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var updated specs.Spec
	if err := json.Unmarshal(updatedData, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Process.ApparmorProfile != "" || updated.Process.SelinuxLabel != "" || updated.Linux.MountLabel != "" || updated.Linux.Seccomp != nil || updated.Process.NoNewPrivileges {
		t.Fatalf("security profiles remain: process=%+v linux=%+v", updated.Process, updated.Linux)
	}
	if len(updated.Linux.MaskedPaths) != 0 || len(updated.Linux.ReadonlyPaths) != 0 {
		t.Fatalf("masked=%v readonly=%v", updated.Linux.MaskedPaths, updated.Linux.ReadonlyPaths)
	}
	wantDeviceRules := []specs.LinuxDeviceCgroup{{Allow: true, Type: "c", Access: "rwm"}, {Allow: true, Type: "b", Access: "rwm"}}
	if !reflect.DeepEqual(updated.Linux.Resources.Devices, wantDeviceRules) {
		t.Fatalf("device rules = %#v, want %#v", updated.Linux.Resources.Devices, wantDeviceRules)
	}
	current, err := currentCapabilityNames()
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct{ got, initial []string }{
		{updated.Process.Capabilities.Bounding, []string{"CAP_CHOWN"}},
		{updated.Process.Capabilities.Ambient, []string{"CAP_KILL"}},
		{updated.Process.Capabilities.Effective, []string{"CAP_FOWNER"}},
		{updated.Process.Capabilities.Inheritable, []string{"CAP_SETUID"}},
		{updated.Process.Capabilities.Permitted, []string{"CAP_SETGID"}},
	} {
		if want := append(slices.Clone(check.initial), current...); !reflect.DeepEqual(check.got, want) {
			t.Fatalf("capability set = %v, want %v", check.got, want)
		}
	}
	if !reflect.DeepEqual(updated.Mounts[0].Options, []string{"nosuid", "rw"}) || !reflect.DeepEqual(updated.Mounts[1].Options, []string{"rw", "nosuid"}) || !reflect.DeepEqual(updated.Mounts[2].Options, []string{"ro"}) {
		t.Fatalf("mount options = %#v", updated.Mounts)
	}
}

func TestInsecureRuntimeReexecDispatchesNonCreateCommands(t *testing.T) {
	echo, err := exec.LookPath("echo")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(testWorkerBinary(t), insecureRuntimeMarker, echo, "state", "wrapper-ok")
	// Dispatch uses the private first argument even with this synthetic argv[0].
	command.Args[0] = insecureRuntimeExecutable
	command.Env = append(os.Environ(), "COOPR_TEST_BUILDAH=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("runtime wrapper: %v: %s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != "state wrapper-ok" {
		t.Fatalf("runtime output = %q", got)
	}
}

func TestRuntimeBundlePath(t *testing.T) {
	for _, arguments := range [][]string{{"--bundle", "/bundle", "id"}, {"--bundle=/bundle", "id"}, {"-b", "/bundle", "id"}} {
		got, err := runtimeBundlePath(arguments)
		if err != nil || got != "/bundle" {
			t.Fatalf("runtimeBundlePath(%q) = %q, %v", arguments, got, err)
		}
	}
	if _, err := runtimeBundlePath([]string{"id"}); err == nil {
		t.Fatal("missing bundle accepted")
	}
}

func TestRuntimeCommandIndexAllowsGlobalRuntimeFlags(t *testing.T) {
	if got := runtimeCommandIndex([]string{"--systemd-cgroup", "create", "--bundle", "/bundle", "id"}); got != 1 {
		t.Fatalf("command index = %d, want 1", got)
	}
}
