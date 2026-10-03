package main

import (
	"reflect"
	"testing"

	"coopr/internal/oci"
	"github.com/spf13/cobra"
)

func TestBuildControlFlagsUseConventionalDefaultsAndParsing(t *testing.T) {
	cmd := newBuildCommand()
	for _, setting := range []struct{ name, value string }{
		{"dns", "2001:0db8::1"}, {"dns-search", "Example.TEST"}, {"dns-option", "ndots:2"},
		{"memory", "64m"}, {"memory-swap", "128m"}, {"cpu-period", "10000"}, {"cpu-quota", "5000"},
		{"cpu-shares", "256"}, {"shm-size", "32m"}, {"ulimit", "nofile=1024:2048"}, {"jobs", "3"},
	} {
		if err := cmd.Flags().Set(setting.name, setting.value); err != nil {
			t.Fatalf("set --%s: %v", setting.name, err)
		}
	}
	if flag := cmd.Flags().Lookup("http-proxy"); flag == nil || flag.DefValue != "true" {
		t.Fatalf("--http-proxy default = %#v", flag)
	}
	for _, name := range []string{"authfile", "cert-dir", "tls-verify"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("missing --%s", name)
		}
	}
	if flag := cmd.Flags().Lookup("tls-verify"); flag.DefValue != "true" {
		t.Fatalf("--tls-verify default = %q", flag.DefValue)
	}
}

func TestBuildLifecycleDefaultsMatchPodman(t *testing.T) {
	for _, value := range []string{"", "false", "FALSE", "0", "true", "1", "other"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("BUILDAH_LAYERS", value)
			for _, command := range []*cobra.Command{newBuildCommand(), newComponentBuildCommand()} {
				wantLayers := value != "false" && value != "FALSE" && value != "0"
				if got, _ := command.Flags().GetBool("layers"); got != wantLayers {
					t.Fatalf("layers=%v for BUILDAH_LAYERS=%q", got, value)
				}
				if got, _ := command.Flags().GetBool("force-rm"); !got {
					t.Fatal("force-rm default must match Podman's true")
				}
			}
		})
	}
}

func TestBuildControlParserCanonicalizesRepeatedValues(t *testing.T) {
	flags := buildControlFlags{
		httpProxy: true, dns: []string{"2001:0db8::1", "2001:db8::1"}, dnsSearch: []string{"Example.TEST"},
		memory: "64m", memorySwap: "128m", ulimits: []string{"nofile=1024:2048"},
	}
	controls, err := flags.controls()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(controls.DNSServers, []string{"2001:db8::1"}) || !reflect.DeepEqual(controls.DNSSearch, []string{"example.test"}) {
		t.Fatalf("canonical controls = %+v", controls)
	}
}

func TestBuildCommandsExposePodmanRunControlFlags(t *testing.T) {
	for _, command := range []*cobra.Command{newBuildCommand(), newComponentBuildCommand()} {
		for _, name := range []string{
			"volume", "cap-add", "cap-drop", "security-opt", "device", "group-add",
			"userns", "userns-uid-map", "userns-gid-map", "userns-uid-map-user", "userns-gid-map-group",
			"cgroupns", "cgroup-parent", "cpuset-cpus", "cpuset-mems", "pid", "ipc", "uts",
			"hooks-dir", "runtime-flag",
			"isolation", "runtime",
		} {
			if command.Flags().Lookup(name) == nil {
				t.Errorf("%s missing --%s", command.CommandPath(), name)
			}
		}
	}
}

func TestRegistryFlagsAreSharedByBuildComponentAndCopy(t *testing.T) {
	commands := []struct {
		name string
		cmd  *cobra.Command
	}{
		{"build", newBuildCommand()},
		{"component build", newComponentBuildCommand()},
		{"copy", newCopyCommand(oci.Image)},
	}
	for _, command := range commands {
		for _, name := range []string{"authfile", "cert-dir", "tls-verify"} {
			if command.cmd.Flags().Lookup(name) == nil {
				t.Errorf("%s missing --%s", command.name, name)
			}
		}
	}
}

func TestPullPolicyFlagsUseMissingByDefault(t *testing.T) {
	for _, test := range []struct {
		name string
		cmd  *cobra.Command
	}{
		{name: "build", cmd: newBuildCommand()},
		{name: "component build", cmd: newComponentBuildCommand()},
	} {
		flag := test.cmd.Flags().Lookup("pull-policy")
		if flag == nil || flag.DefValue != string(oci.PullMissing) {
			t.Errorf("%s --pull-policy = %#v", test.name, flag)
		}
		if test.cmd.Flags().Lookup("pull") == nil {
			t.Errorf("%s missing --pull compatibility flag", test.name)
		}
	}
}
