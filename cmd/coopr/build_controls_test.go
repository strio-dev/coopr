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

func TestRegistryTLSVerifyPreservesExplicitPolicy(t *testing.T) {
	for _, args := range [][]string{nil, {"--tls-verify=true"}, {"--tls-verify=false"}} {
		command := &cobra.Command{Use: "test"}
		var flags registryFlags
		flags.addTo(command)
		if err := command.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		verify := flags.tlsPolicy(command)
		if len(args) == 0 {
			if verify != nil {
				t.Fatal("omitted --tls-verify must inherit native registry configuration")
			}
		} else if verify == nil || *verify != (args[0] == "--tls-verify=true") {
			t.Fatalf("TLS policy for %v = %v", args, verify)
		}
	}
	for _, command := range []*cobra.Command{newBuildCommand(), newComponentBuildCommand(), newCopyCommand(oci.Image), newCopyCommand(oci.Component)} {
		for _, name := range []string{"plain-http", "plain-http-registry"} {
			if command.Flags().Lookup(name) != nil {
				t.Errorf("%s retains --%s", command.Use, name)
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
		flag := test.cmd.Flags().Lookup("pull")
		if flag == nil || flag.DefValue != string(oci.PullMissing) {
			t.Fatalf("%s --pull = %#v", test.name, flag)
		}
		if flag.NoOptDefVal != "always" {
			t.Fatalf("%s bare --pull = %q", test.name, flag.NoOptDefVal)
		}
		for _, policy := range []string{"always", "missing", "never", "newer"} {
			if err := test.cmd.ParseFlags([]string{"--pull=" + policy}); err != nil {
				t.Fatalf("%s --pull=%s: %v", test.name, policy, err)
			}
			if flag.Value.String() != policy {
				t.Fatalf("%s --pull=%s became %q", test.name, policy, flag.Value.String())
			}
		}
		if err := test.cmd.ParseFlags([]string{"--pull=invalid"}); err == nil {
			t.Fatalf("%s accepted invalid pull policy", test.name)
		}
	}
}

func TestBuildAndCopyFlagsHaveCommandLocalScope(t *testing.T) {
	root := newRootCommand()
	if root.PersistentFlags().Lookup("signature-policy") != nil {
		t.Fatal("signature policy must not be global")
	}
	for _, path := range [][]string{{"build"}, {"component", "build"}, {"copy"}, {"component", "copy"}} {
		command, _, err := root.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		isBuild := command.Name() == "build"
		for _, name := range []string{"pull-policy", "context", "cache"} {
			if command.Flags().Lookup(name) != nil {
				t.Fatalf("%v retains removed --%s", path, name)
			}
		}
		if got := command.Flags().Lookup("decryption-key") != nil; got != isBuild {
			t.Fatalf("%v decryption-key present=%v, want %v", path, got, isBuild)
		}
		policy := command.LocalNonPersistentFlags().Lookup("signature-policy")
		if len(path) == 2 && path[1] == "copy" {
			if policy != nil || command.InheritedFlags().Lookup("signature-policy") != nil {
				t.Fatal("component copy must not have image signature policy")
			}
			continue
		}
		if policy == nil || !policy.Hidden {
			t.Fatalf("%v signature policy must be hidden and local", path)
		}
		if err := command.ParseFlags([]string{"--signature-policy=/policy.json"}); err != nil {
			t.Fatal(err)
		}
		if commandSignaturePolicy(command) != "/policy.json" {
			t.Fatalf("%v signature policy was not applied", path)
		}
	}
}
