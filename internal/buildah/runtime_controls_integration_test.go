//go:build linux

package buildah

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"coopr/internal/definition"
	"coopr/internal/planner"
)

type liveRuntimeControlsFixture struct {
	ctx   context.Context
	root  string
	store StoreOptions
	base  liveBaseFixture
}

func newLiveRuntimeControlsFixture(t *testing.T) liveRuntimeControlsFixture {
	t.Helper()
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live build-wide runtime control coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	root := t.TempDir()
	store := StoreOptions{
		RunRoot:         filepath.Join(root, "run"),
		GraphRoot:       filepath.Join(root, "graph"),
		GraphDriverName: "vfs",
	}
	return liveRuntimeControlsFixture{ctx: ctx, root: root, store: store, base: newLiveBusyBoxStorage(t, ctx, root, store)}
}

func (fixture liveRuntimeControlsFixture) build(
	t *testing.T,
	name string,
	command string,
	controls RunControls,
	isolation string,
	runtimePath string,
) (string, error) {
	return fixture.buildWithNetwork(t, name, command, controls, isolation, runtimePath, "none")
}

func (fixture liveRuntimeControlsFixture) buildWithNetwork(
	t *testing.T,
	name string,
	command string,
	controls RunControls,
	isolation string,
	runtimePath string,
	network string,
) (string, error) {
	t.Helper()
	def, err := definition.Parse(strings.NewReader(fmt.Sprintf("from %q\nrun %q network=%q\n", fixture.base.reference, command, network)))
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(fixture.root, "layout-"+name)
	_, err = BuildDefinitionSupervised(fixture.ctx, def, planner.Options{
		Mode: planner.Build, Platform: "linux/" + runtime.GOARCH,
	}, SupervisedPlanOptions{
		Store: fixture.store, ContextDir: fixture.root, Isolation: isolation, Runtime: runtimePath, Network: network,
		Output: Output{Path: layout}, RunControls: controls,
		Stdout: os.Stdout, Stderr: os.Stderr,
	})
	if err != nil {
		return "", err
	}
	manifest, _ := readPlanImage(t, layout)
	proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded()), "proof")
	return proof, nil
}

func TestBuildDefinitionRunsSlirp4netnsWithNativeOptionsAndCleansUp(t *testing.T) {
	fixture := newLiveRuntimeControlsFixture(t)
	runtimePath := liveOCIRuntime(t)
	helper, err := exec.LookPath("slirp4netns")
	if err != nil {
		t.Skip("slirp4netns is not installed")
	}
	controls, err := ParseRunControls(RunControlInput{NetworkCmdPath: helper})
	if err != nil {
		t.Fatal(err)
	}
	network := "slirp4netns:mtu=1400,cidr=10.89.0.0/24,enable_ipv6=false"
	command := `set -e
test "$(/bin/busybox cat /sys/class/net/tap0/mtu)" = 1400
/bin/busybox grep -q 'nameserver 10.89.0.3' /etc/resolv.conf
/bin/busybox awk '$1 == "tap0" && $2 == "00000000" && $3 == "0200590A" { found = 1 } END { exit !found }' /proc/net/route
printf slirp-ready >/proof`
	proof, err := fixture.buildWithNetwork(t, "slirp", command, controls, "rootless", runtimePath, network)
	if err != nil {
		t.Fatal(err)
	}
	if proof != "slirp-ready" {
		t.Fatalf("slirp proof = %q", proof)
	}
	assertNoSlirpRuntimeDirectories(t, fixture.store.GraphRoot+"-tmp")
}

func TestBuildDefinitionCancellationCleansSlirp4netns(t *testing.T) {
	fixture := newLiveRuntimeControlsFixture(t)
	runtimePath := liveOCIRuntime(t)
	helper, err := exec.LookPath("slirp4netns")
	if err != nil {
		t.Skip("slirp4netns is not installed")
	}
	controls, err := ParseRunControls(RunControlInput{NetworkCmdPath: helper})
	if err != nil {
		t.Fatal(err)
	}
	def, err := definition.Parse(strings.NewReader(fmt.Sprintf("from %q\nrun %q network=%q\n", fixture.base.reference, "trap 'exit 0' TERM; sleep 60", "slirp4netns:enable_ipv6=false")))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = BuildDefinitionSupervised(ctx, def, planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{
		Store: fixture.store, ContextDir: fixture.root, Isolation: "rootless", Runtime: runtimePath,
		Network: "slirp4netns:enable_ipv6=false", Output: Output{Path: filepath.Join(fixture.root, "layout-cancel")},
		RunControls: controls,
		Stdout:      os.Stdout, Stderr: os.Stderr,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled slirp build error = %v, want deadline exceeded", err)
	}
	assertNoSlirpRuntimeDirectories(t, fixture.store.GraphRoot+"-tmp")
}

func assertNoSlirpRuntimeDirectories(t *testing.T, temporaryRoot string) {
	t.Helper()
	entries, err := os.ReadDir(temporaryRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "coopr-slirp-") {
			t.Fatalf("slirp runtime directory leaked after RUN: %s", entry.Name())
		}
	}
}

func TestBuildDefinitionAppliesBuildWideSecurityDeviceAndGroupControls(t *testing.T) {
	fixture := newLiveRuntimeControlsFixture(t)
	runtimePath := liveOCIRuntime(t)
	group := liveSupplementaryGroup(t)
	controls, err := ParseRunControls(RunControlInput{
		CapAdd:          []string{"chown", "dac_override"},
		CapDrop:         []string{"all"},
		SecurityOptions: []string{"no-new-privileges"},
		Devices:         []string{"/dev/null:/dev/coopr-null:rw"},
		GroupAdd:        []string{strconv.Itoa(group)},
	})
	if err != nil {
		t.Fatal(err)
	}
	command := fmt.Sprintf(`set -e
/bin/busybox awk '$1 == "NoNewPrivs:" { seen = 1; if ($2 != 1) exit 1 } END { exit !seen }' /proc/self/status
/bin/busybox awk '$1 == "CapEff:" { seen = 1; if (tolower($2) != "0000000000000003") exit 1 } END { exit !seen }' /proc/self/status
/bin/busybox awk '$1 == "Groups:" { for (i = 2; i <= NF; i++) if ($i == %d) seen = 1 } END { exit !seen }' /proc/self/status
test -c /dev/coopr-null
printf discarded >/dev/coopr-null
printf enforced >/proof`, group)
	proof, err := fixture.build(t, "security-device-group", command, controls, "rootless", runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if proof != "enforced" {
		t.Fatalf("security/device/group proof = %q", proof)
	}
}

func TestBuildDefinitionSecurityProfileEnforcesSeccomp(t *testing.T) {
	fixture := newLiveRuntimeControlsFixture(t)
	runtimePath := liveOCIRuntime(t)
	seccomp := filepath.Join(fixture.root, "seccomp.json")
	if err := os.WriteFile(seccomp, []byte(`{"defaultAction":"SCMP_ACT_ALLOW","syscalls":[{"names":["mkdir","mkdirat"],"action":"SCMP_ACT_ERRNO","errnoRet":1}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	controls, err := ParseRunControls(RunControlInput{SecurityOptions: []string{"seccomp=" + seccomp}})
	if err != nil {
		t.Fatal(err)
	}
	unconfined, err := ParseRunControls(RunControlInput{SecurityOptions: []string{"seccomp=unconfined"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.build(t, "seccomp-baseline", "set -e; /bin/busybox mkdir /blocked; printf unfiltered >/proof", unconfined, "rootless", runtimePath); err != nil {
		t.Fatal(err)
	}
	proof, err := fixture.build(t, "seccomp", "set -e; if /bin/busybox mkdir /blocked; then exit 1; fi; printf filtered >/proof", controls, "rootless", runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if proof != "filtered" {
		t.Fatalf("seccomp profile proof = %q", proof)
	}
}

func TestBuildDefinitionAppliesIsolationRuntimeAndRuntimeFlagOverrides(t *testing.T) {
	fixture := newLiveRuntimeControlsFixture(t)
	realRuntime := liveOCIRuntime(t)
	marker := filepath.Join(fixture.root, "runtime-called")
	wrapper := filepath.Join(fixture.root, "runtime-wrapper")
	script := "#!/bin/sh\nprintf called >" + marker + "\nexec " + realRuntime + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	controls, err := ParseRunControls(RunControlInput{
		Isolation: "rootless", Runtime: wrapper, RuntimeFlags: []string{"log-format=json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	proof, err := fixture.build(t, "runtime-override", "printf overridden >/proof", controls, "chroot", "/does/not/exist")
	if err != nil {
		t.Fatal(err)
	}
	if proof != "overridden" {
		t.Fatalf("isolation/runtime override proof = %q", proof)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "called" {
		t.Fatalf("selected runtime marker = %q, %v", data, err)
	}
}

func TestBuildDefinitionRunsConfiguredOCIHook(t *testing.T) {
	fixture := newLiveRuntimeControlsFixture(t)
	runtimePath := liveOCIRuntime(t)
	marker := filepath.Join(fixture.root, "hook-called")
	hookPath := filepath.Join(fixture.root, "record-hook")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nprintf hooked >"+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hooksDir := filepath.Join(fixture.root, "hooks")
	if err := os.Mkdir(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hookConfig := fmt.Sprintf(`{"version":"1.0.0","hook":{"path":%q,"args":[%q]},"when":{"always":true},"stages":["createContainer"]}`, hookPath, hookPath)
	if err := os.WriteFile(filepath.Join(hooksDir, "record.json"), []byte(hookConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	controls, err := ParseRunControls(RunControlInput{HooksDirs: []string{hooksDir}})
	if err != nil {
		t.Fatal(err)
	}
	proof, err := fixture.build(t, "oci-hook", "printf ran >/proof", controls, "rootless", runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if proof != "ran" {
		t.Fatalf("hooked RUN proof = %q", proof)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "hooked" {
		t.Fatalf("OCI hook marker = %q, %v", data, err)
	}
}

func TestBuildDefinitionAppliesProcessNamespaceControls(t *testing.T) {
	fixture := newLiveRuntimeControlsFixture(t)
	runtimePath := liveOCIRuntime(t)
	pidNamespace, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Fatal(err)
	}
	utsNamespace, err := os.Readlink("/proc/self/ns/uts")
	if err != nil {
		t.Fatal(err)
	}
	ipcNamespace, err := os.Readlink("/proc/self/ns/ipc")
	if err != nil {
		t.Fatal(err)
	}
	cgroupNamespace, err := os.Readlink("/proc/self/ns/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	controls, err := ParseRunControls(RunControlInput{PIDNS: "host", IPCNS: "host", UTSNS: "host", CgroupNS: "host"})
	if err != nil {
		t.Fatal(err)
	}
	command := fmt.Sprintf(`set -e
test "$(/bin/busybox readlink /proc/self/ns/pid)" = %q
test "$(/bin/busybox readlink /proc/self/ns/uts)" = %q
test "$(/bin/busybox readlink /proc/self/ns/ipc)" = %q
test "$(/bin/busybox readlink /proc/self/ns/cgroup)" = %q
printf joined >/proof`, pidNamespace, utsNamespace, ipcNamespace, cgroupNamespace)
	proof, err := fixture.build(t, "host-namespaces", command, controls, "rootless", runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if proof != "joined" {
		t.Fatalf("namespace proof = %q", proof)
	}
}

func TestBuildDefinitionAppliesUIDAndGIDMappings(t *testing.T) {
	fixture := newLiveRuntimeControlsFixture(t)
	runtimePath := liveOCIRuntime(t)
	uidStart, uidSize := liveAvailableIDRange(t, "/proc/self/uid_map")
	gidStart, gidSize := liveAvailableIDRange(t, "/proc/self/gid_map")
	size := min(uidSize, gidSize)
	if size < 2 {
		t.Fatalf("live ID-mapping test requires at least two subordinate IDs, found %d", size)
	}
	uidStart++
	gidStart++
	size--
	controls, err := ParseRunControls(RunControlInput{
		UserNS: "private",
		UIDMap: []string{"0:0:1", fmt.Sprintf("1:%d:%d", uidStart, size)},
		GIDMap: []string{"0:0:1", fmt.Sprintf("1:%d:%d", gidStart, size)},
	})
	if err != nil {
		t.Fatal(err)
	}
	command := fmt.Sprintf(`set -e
/bin/busybox awk 'NR == 1 && $1 == 0 && $2 == 0 && $3 == 1 { root = 1 } NR == 2 && $1 == 1 && $2 == %d && $3 == %d { mapped = 1 } END { exit !(root && mapped) }' /proc/self/uid_map
/bin/busybox awk 'NR == 1 && $1 == 0 && $2 == 0 && $3 == 1 { root = 1 } NR == 2 && $1 == 1 && $2 == %d && $3 == %d { mapped = 1 } END { exit !(root && mapped) }' /proc/self/gid_map
printf mapped >/proof`, uidStart, size, gidStart, size)
	proof, err := fixture.build(t, "id-mappings", command, controls, "rootless", runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if proof != "mapped" {
		t.Fatalf("UID/GID mapping proof = %q", proof)
	}
}

func TestBuildDefinitionCPUSetControlsEnforceOrRejectHost(t *testing.T) {
	fixture := newLiveRuntimeControlsFixture(t)
	runtimePath := liveOCIRuntime(t)
	cpu := firstAllowedValue(t, "Cpus_allowed_list")
	mem := firstAllowedValue(t, "Mems_allowed_list")
	controls, err := ParseRunControls(RunControlInput{CPUSetCPUs: cpu, CPUSetMems: mem})
	if err != nil {
		t.Fatal(err)
	}
	command := fmt.Sprintf(`set -e
test "$(/bin/busybox awk '/^Cpus_allowed_list:/ { print $2 }' /proc/self/status)" = %q
test "$(/bin/busybox awk '/^Mems_allowed_list:/ { print $2 }' /proc/self/status)" = %q
printf pinned >/proof`, cpu, mem)
	proof, err := fixture.build(t, "cpuset", command, controls, "rootless", runtimePath)
	if err != nil {
		if unsupportedCPUSetHost(err) {
			t.Logf("proved unsupported host rejection: %v", err)
			return
		}
		t.Fatal(err)
	}
	if proof != "pinned" {
		t.Fatalf("cpuset proof = %q", proof)
	}
}

func liveOCIRuntime(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("crun")
	if err != nil {
		t.Fatalf("live runtime-control tests require crun: %v", err)
	}
	return path
}

func liveSupplementaryGroup(t *testing.T) int {
	t.Helper()
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range groups {
		if group != os.Getegid() {
			return group
		}
	}
	return os.Getegid()
}

func liveAvailableIDRange(t *testing.T, path string) (int, int) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("inspect available ID mappings in %s: %v", path, err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			continue
		}
		start, startErr := strconv.Atoi(fields[0])
		size, sizeErr := strconv.Atoi(fields[2])
		if startErr == nil && sizeErr == nil && start > 0 && size > 0 {
			return start, size
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("live ID-mapping test requires a subordinate range in %s", path)
	return 0, 0
}

func firstAllowedValue(t *testing.T, field string) string {
	t.Helper()
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found || name != field {
			continue
		}
		value = strings.TrimSpace(value)
		value, _, _ = strings.Cut(value, ",")
		value, _, _ = strings.Cut(value, "-")
		if value != "" {
			return value
		}
	}
	t.Fatalf("%s is missing from /proc/self/status", field)
	return ""
}

func unsupportedCPUSetHost(err error) bool {
	message := err.Error()
	for _, fragment := range []string{
		"cpuset cgroup controller delegated to Coopr",
		"cgroup v2 for rootless builds",
		"available systemd user session for rootless builds",
		"current cgroup to be delegated to Coopr",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}
