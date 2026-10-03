package buildah

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	upstream "go.podman.io/buildah"
)

func TestParseRunControlsCanonicalizesBuildahSettings(t *testing.T) {
	controls, err := ParseRunControls(RunControlInput{
		HTTPProxy: true, DNSServers: []string{"2001:0db8::1", "2001:db8::1"},
		DNSSearch: []string{"Example.TEST", "example.test"}, DNSOptions: []string{"ndots:2"},
		Memory: "64m", MemorySwap: "128m", CPUPeriod: 10000, CPUQuota: 5000, CPUShares: 256,
		ShmSize: "32m", Ulimits: []string{"nofile=1024:2048"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if controls.Memory != 64<<20 || controls.MemorySwap != 128<<20 || controls.ShmSize != 32<<20 {
		t.Fatalf("canonical sizes = %+v", controls)
	}
	if !reflect.DeepEqual(controls.DNSServers, []string{"2001:db8::1"}) || !reflect.DeepEqual(controls.DNSSearch, []string{"example.test"}) {
		t.Fatalf("canonical DNS controls = %+v", controls)
	}
	common := controls.commonBuildOptions([]string{"host.test:127.0.0.1"})
	if !common.HTTPProxy || common.Memory != controls.Memory || common.ShmSize != "33554432" || !reflect.DeepEqual(common.Ulimit, []string{"nofile=1024:2048"}) {
		t.Fatalf("Buildah common options = %+v", common)
	}
}

func TestParseRunControlsRejectsInvalidValues(t *testing.T) {
	tests := []RunControlInput{
		{Memory: "nope"},
		{Memory: "-1m"},
		{ShmSize: "-1"},
		{DNSServers: []string{"not-an-ip"}},
		{DNSServers: []string{"none", "1.1.1.1"}},
		{DNSSearch: []string{"bad domain"}},
		{DNSOptions: []string{"ndots:2,timeout:1"}},
		{Ulimits: []string{"not-a-limit"}},
	}
	for _, input := range tests {
		if _, err := ParseRunControls(input); err == nil {
			t.Errorf("accepted invalid controls %+v", input)
		}
	}
}

func TestParseRunControlsAcceptsUpstreamResourceValues(t *testing.T) {
	controls, err := ParseRunControls(RunControlInput{Memory: "0", MemorySwap: "0", ShmSize: "0", CPUQuota: -2})
	if err != nil {
		t.Fatalf("zero resource values rejected: %v", err)
	}
	if controls.Memory != 0 || controls.MemorySwap != 0 || controls.ShmSize != 0 {
		t.Fatalf("zero resource values = %+v", controls)
	}
	if common := controls.commonBuildOptions(nil); common.ShmSize != "0" || common.CPUQuota != -2 {
		t.Fatalf("upstream resource values not forwarded: %+v", common)
	}
	if !controls.ShmSizeSet {
		t.Fatal("explicit zero shm-size lost its presence marker")
	}
	encoded, err := json.Marshal(controls)
	if err != nil {
		t.Fatal(err)
	}
	var transferred RunControls
	if err := json.Unmarshal(encoded, &transferred); err != nil {
		t.Fatal(err)
	}
	if !transferred.ShmSizeSet || transferred.commonBuildOptions(nil).ShmSize != "0" {
		t.Fatalf("explicit zero shm-size lost across worker transfer: %+v", transferred)
	}
	explicitDigest, err := cacheRunControlsDigest(controls)
	if err != nil {
		t.Fatal(err)
	}
	defaultDigest, err := cacheRunControlsDigest(RunControls{CPUQuota: -2})
	if err != nil {
		t.Fatal(err)
	}
	if explicitDigest == defaultDigest {
		t.Fatalf("explicit zero shm-size shares default cache identity %s", explicitDigest)
	}

	for _, input := range []RunControlInput{
		{MemorySwap: "1g"},
		{Memory: "2g", MemorySwap: "1g"},
	} {
		controls, err := ParseRunControls(input)
		if err != nil {
			t.Errorf("upstream-valid resource controls %+v rejected: %v", input, err)
			continue
		}
		if err := validateRunControls(controls); err != nil {
			t.Errorf("worker rejected resource controls %+v: %v", controls, err)
		}
	}
}

func TestResourceControlEnvironmentRejectsUnsupportedHostLimits(t *testing.T) {
	controls := RunControls{Memory: 64 << 20, CPUQuota: 5000}
	if err := validateResourceControlEnvironment(controls, true, false, nil, nil); err == nil {
		t.Fatal("rootless cgroup v1 accepted resource limits")
	}
	if err := validateResourceControlEnvironment(controls, true, true, []string{"cpu"}, nil); err == nil {
		t.Fatal("missing memory controller accepted")
	}
	if err := validateResourceControlEnvironment(controls, true, true, []string{"memory"}, nil); err == nil {
		t.Fatal("missing CPU controller accepted")
	}
	if err := validateResourceControlEnvironment(controls, true, true, []string{"cpu", "memory"}, nil); err != nil {
		t.Fatalf("delegated controllers rejected: %v", err)
	}
}

func TestDNSControlsRejectBuildNetworkNone(t *testing.T) {
	if err := validateRunControlsForNetwork(RunControls{DNSServers: []string{"1.1.1.1"}}, "none"); err == nil {
		t.Fatal("DNS server accepted with build network none")
	}
	if err := validateRunControlsForNetwork(RunControls{}, "none"); err != nil {
		t.Fatalf("network none without DNS rejected: %v", err)
	}
}

func TestParseRunControlsCanonicalizesRuntimeControls(t *testing.T) {
	root := t.TempDir()
	hooks := filepath.Join(root, "hooks")
	if err := os.Mkdir(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	seccomp := filepath.Join(root, "seccomp.json")
	if err := os.WriteFile(seccomp, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	controls, err := ParseRunControls(RunControlInput{
		Volumes: []string{root + ":/mnt/input:ro,rprivate"},
		CapAdd:  []string{"net_admin"}, CapDrop: []string{"chown"},
		SecurityOptions: []string{"no-new-privileges", "seccomp=" + seccomp, "mask=/proc/acpi:/proc/keys", "label=disable"},
		Devices:         []string{"/dev/null:/dev/example:r"}, GroupAdd: []string{"keep-groups"},
		UserNS: "private", UIDMap: []string{"0:100000:65536"}, GIDMap: []string{"0:200000:65536"},
		CgroupNS: "private", PIDNS: "host", IPCNS: "private", UTSNS: "host",
		CgroupParent: "build.slice", CPUSetCPUs: "0-1", CPUSetMems: "0", HooksDirs: []string{hooks},
		RuntimeFlags: []string{"log-format=json"},
		Isolation:    "ROOTLESS", Runtime: "/usr/bin/crun",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(controls.CapAdd, []string{"CAP_NET_ADMIN"}) || !reflect.DeepEqual(controls.CapDrop, []string{"CAP_CHOWN"}) {
		t.Fatalf("capabilities = add %v drop %v", controls.CapAdd, controls.CapDrop)
	}
	if controls.IDMappingOptions == nil || len(controls.IDMappingOptions.UIDMap) != 1 || controls.IDMappingOptions.UIDMap[0].HostID != 100000 {
		t.Fatalf("ID mappings = %+v", controls.IDMappingOptions)
	}
	if controls.NamespaceOptions.Find("pid").Host != true || controls.NamespaceOptions.Find("ipc").Host {
		t.Fatalf("namespace options = %+v", controls.NamespaceOptions)
	}
	if controls.SeccompDigest == "" || len(controls.HooksDigests) != 1 || controls.HooksDigests[0] == "" {
		t.Fatalf("static host input digests = seccomp %q hooks %v", controls.SeccompDigest, controls.HooksDigests)
	}
	if !reflect.DeepEqual(controls.RuntimeFlags, []string{"--log-format=json"}) {
		t.Fatalf("runtime flags = %v", controls.RuntimeFlags)
	}
	if controls.Isolation != "rootless" || controls.Runtime != "/usr/bin/crun" {
		t.Fatalf("runtime selection = isolation %q runtime %q", controls.Isolation, controls.Runtime)
	}
	options := upstream.BuilderOptions{Capabilities: []string{"CAP_CHOWN", "CAP_SETUID"}, CommonBuildOpts: controls.commonBuildOptions(nil)}
	controls.BaseCapabilities = []string{"CAP_CHOWN", "CAP_SETUID"}
	if err := controls.applyBuilderOptions(&options); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(options.Capabilities, []string{"CAP_NET_ADMIN", "CAP_SETUID"}) {
		t.Fatalf("merged capabilities = %v", options.Capabilities)
	}
	if !reflect.DeepEqual(options.DeviceSpecs, []string{"/dev/null:/dev/example:r"}) || !reflect.DeepEqual(options.GroupAdd, []string{"keep-groups"}) {
		t.Fatalf("builder runtime controls = devices %v groups %v", options.DeviceSpecs, options.GroupAdd)
	}
	if options.CommonBuildOpts.CgroupParent != "build.slice" || options.CommonBuildOpts.SeccompProfilePath != seccomp || len(options.CommonBuildOpts.Volumes) != 1 {
		t.Fatalf("common runtime controls = %+v", options.CommonBuildOpts)
	}
	if options.Isolation.String() != "rootless" {
		t.Fatalf("builder isolation = %s", options.Isolation)
	}
}

func TestParseRunControlsCanonicalizesNetworkCommandPathAndCacheIdentity(t *testing.T) {
	helper := filepath.Join(t.TempDir(), "custom-slirp")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	controls, err := ParseRunControls(RunControlInput{NetworkCmdPath: helper})
	if err != nil {
		t.Fatal(err)
	}
	if controls.NetworkCmdPath != helper {
		t.Fatalf("network command path = %q, want %q", controls.NetworkCmdPath, helper)
	}
	configured, err := cacheRunControlsDigest(controls)
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := cacheRunControlsDigest(RunControls{})
	if err != nil {
		t.Fatal(err)
	}
	if configured == defaults {
		t.Fatalf("network command path shares default cache identity %s", configured)
	}
}

func TestParseRunControlsAppliesContainersConfigDefaults(t *testing.T) {
	root := t.TempDir()
	volume := filepath.Join(root, "volume")
	cdi := filepath.Join(root, "cdi")
	hooks := filepath.Join(root, "hooks")
	for _, directory := range []string{volume, cdi, hooks} {
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, "containers.conf")
	config := `[containers]
default_capabilities = ["CHOWN", "NET_BIND_SERVICE"]
dns_servers = ["192.0.2.53"]
dns_searches = ["example.test"]
dns_options = ["ndots:2"]
default_ulimits = ["nofile=512:1024"]
shm_size = "32m"
volumes = ["` + volume + `:/configured:ro"]

[engine]
cgroup_manager = "cgroupfs"
cdi_spec_dirs = ["` + cdi + `"]
hooks_dir = ["` + hooks + `"]
`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_CONF", configPath)
	controls, err := ParseRunControls(RunControlInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(controls.BaseCapabilities, []string{"CAP_CHOWN", "CAP_NET_BIND_SERVICE"}) ||
		!reflect.DeepEqual(controls.DNSServers, []string{"192.0.2.53"}) || controls.ShmSize != 32<<20 ||
		!reflect.DeepEqual(controls.Ulimits, []string{"nofile=512:1024"}) || len(controls.Volumes) != 1 ||
		controls.CgroupManager != "cgroupfs" || !reflect.DeepEqual(controls.CDISpecDirs, []string{cdi}) || !reflect.DeepEqual(controls.HooksDirs, []string{hooks}) {
		t.Fatalf("effective containers.conf defaults = %+v", controls)
	}
	common := controls.commonBuildOptions(nil)
	if common.ShmSize != "33554432" || !reflect.DeepEqual(common.DNSSearch, []string{"example.test"}) || !reflect.DeepEqual(common.DNSOptions, []string{"ndots:2"}) {
		t.Fatalf("Buildah options from containers.conf = %+v", common)
	}
	options := upstream.BuilderOptions{Capabilities: []string{"CAP_SYS_ADMIN"}, CommonBuildOpts: common}
	if err := controls.applyBuilderOptions(&options); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(options.Capabilities, []string{"CAP_CHOWN", "CAP_NET_BIND_SERVICE"}) {
		t.Fatalf("configured base capabilities = %v", options.Capabilities)
	}
}

func TestParseRunControlsCanonicalizesRelativeNamespacePaths(t *testing.T) {
	root := t.TempDir()
	namespace := filepath.Join(root, "pidns")
	if err := os.WriteFile(namespace, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	oldWorkingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWorkingDirectory) })

	controls, err := ParseRunControls(RunControlInput{PIDNS: "ns:pidns"})
	if err != nil {
		t.Fatalf("relative namespace path rejected: %v", err)
	}
	if got := controls.NamespaceOptions.Find("pid").Path; got != namespace {
		t.Fatalf("canonical namespace path = %q, want %q", got, namespace)
	}
}

func TestParseRunControlsDefersOptionalDirectoryExistenceToNativeConsumers(t *testing.T) {
	root := t.TempDir()
	missingCDI := filepath.Join(root, "missing-cdi")
	missingNetwork := filepath.Join(root, "missing-networks")
	controls, err := ParseRunControls(RunControlInput{CDISpecDirs: []string{missingCDI}, NetworkConfigDir: missingNetwork})
	if err != nil {
		t.Fatalf("optional native configuration paths were rejected early: %v", err)
	}
	if !reflect.DeepEqual(controls.CDISpecDirs, []string{missingCDI}) || controls.NetworkConfigDir != missingNetwork {
		t.Fatalf("optional native configuration paths = %+v", controls)
	}
}

func TestParseRunControlsRejectsInvalidRuntimeControls(t *testing.T) {
	for _, input := range []RunControlInput{
		{Volumes: []string{"missing-container-path"}},
		{CapAdd: []string{"definitely-not-a-capability"}},
		{CapAdd: []string{"chown"}, CapDrop: []string{"chown"}},
		{SecurityOptions: []string{"unknown=value"}},
		{SecurityOptions: []string{"seccomp=/does/not/exist"}},
		{Devices: []string{"relative-device"}},
		{UserNS: "host", UIDMap: []string{"0:100000:1"}},
		{PIDNS: "not-a-namespace"},
		{PIDNS: "ns:"},
		{HooksDirs: []string{"/does/not/exist"}},
		{RuntimeFlags: []string{"--debug"}},
		{Isolation: "vm"},
	} {
		if _, err := ParseRunControls(input); err == nil {
			t.Errorf("accepted invalid runtime controls: %+v", input)
		}
	}
}

func TestRunControlStaticInputDigestsChangeWithContents(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "seccomp.json")
	if err := os.WriteFile(profile, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := ParseRunControls(RunControlInput{SecurityOptions: []string{"seccomp=" + profile}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profile, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := ParseRunControls(RunControlInput{SecurityOptions: []string{"seccomp=" + profile}})
	if err != nil {
		t.Fatal(err)
	}
	if first.SeccompDigest == second.SeccompDigest {
		t.Fatalf("profile content change did not change digest %q", first.SeccompDigest)
	}
}
