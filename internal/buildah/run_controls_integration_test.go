package buildah

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/definition"
	"coopr/internal/planner"
)

func TestBuildDefinitionAppliesProxyDNSShmAndUlimitControls(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live RUN control coverage")
	}
	t.Setenv("HTTP_PROXY", "http://host-proxy.example:3128")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	def, err := definition.Parse(strings.NewReader(fmt.Sprintf(`
from %q
run "set -e; test \"$HTTP_PROXY\" = http://host-proxy.example:3128; /bin/busybox grep -q '^nameserver 203.0.113.53$' /etc/resolv.conf; test \"$(ulimit -n)\" = 512; test \"$(/bin/busybox df -k /dev/shm | /bin/busybox awk 'NR == 2 { print $2 }')\" -ge 16384; printf ready >/proof" network="default"
`, base.reference)))
	if err != nil {
		t.Fatal(err)
	}
	controls, err := ParseRunControls(RunControlInput{
		HTTPProxy: true, DNSServers: []string{"203.0.113.53"}, ShmSize: "16m", Ulimits: []string{"nofile=512:512"},
	})
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(root, "layout")
	_, err = BuildDefinitionSupervised(ctx, def, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Network: "none",
		Output: Output{Path: layout}, RunControls: controls,
		Stdout: os.Stdout, Stderr: os.Stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded()), "proof")
	if proof != "ready" {
		t.Fatalf("RUN control proof = %q", proof)
	}
}

func TestBuildDefinitionMemoryCPUControlsEnforceOrRejectHost(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live cgroup control coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	def, err := definition.Parse(strings.NewReader(fmt.Sprintf(`
from %q
run "set -ex; cat /proc/self/cgroup; cgroup=$( /bin/busybox awk -F: '$1 == 0 { print $3 }' /proc/self/cgroup ); test \"$(cat /sys/fs/cgroup$cgroup/memory.max)\" = 67108864; test \"$(cat /sys/fs/cgroup$cgroup/cpu.max)\" = '5000 10000'; printf enforced >/proof" network="none"
`, base.reference)))
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(root, "layout")
	_, err = BuildDefinitionSupervised(ctx, def, planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Network: "none",
		Output:      Output{Path: layout},
		RunControls: RunControls{Memory: 64 << 20, CPUQuota: 5000, CPUPeriod: 10000},
		Stdout:      os.Stdout, Stderr: os.Stderr,
	})
	if err != nil {
		if strings.Contains(err.Error(), "cgroup controller delegated to Coopr") || strings.Contains(err.Error(), "cgroup v2 for rootless builds") || strings.Contains(err.Error(), "systemd user session for rootless builds") || strings.Contains(err.Error(), "current cgroup to be delegated to Coopr") {
			t.Logf("proved unsupported host rejection: %v", err)
			return
		}
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded()), "proof")
	if proof != "enforced" {
		t.Fatalf("cgroup enforcement proof = %q", proof)
	}
}

func TestBuildDefinitionAppliesBuildWideHostVolume(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live RUN control coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(root, "host-input")
	if err := os.Mkdir(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "message"), []byte("mounted"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	def, err := definition.Parse(strings.NewReader(fmt.Sprintf(`
from %q
run "set -e; test \"$(cat /host-input/message)\" = mounted; printf ready >/proof" network="none"
`, base.reference)))
	if err != nil {
		t.Fatal(err)
	}
	controls, err := ParseRunControls(RunControlInput{Volumes: []string{input + ":/host-input:ro,Z"}})
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(root, "layout")
	_, err = BuildDefinitionSupervised(ctx, def, planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Network: "none",
		Output: Output{Path: layout}, RunControls: controls,
		Stdout: os.Stdout, Stderr: os.Stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded()), "proof")
	if proof != "ready" {
		t.Fatalf("build-wide volume proof = %q", proof)
	}
}

func TestBuildDefinitionDisablesGeneratedHostFiles(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live host-file control coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	def, err := definition.Parse(strings.NewReader(fmt.Sprintf(`
from %q
run "set -e; test ! -e /etc/hostname; test ! -e /etc/hosts; printf ready >/proof" network="none"
`, base.reference)))
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(root, "layout")
	_, err = BuildDefinitionSupervised(ctx, def, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Network: "none",
		Output:      Output{Path: layout},
		RunControls: RunControls{NoHostname: true, NoHosts: true},
		Stdout:      os.Stdout, Stderr: os.Stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded()), "proof")
	if proof != "ready" {
		t.Fatalf("host-file control proof = %q", proof)
	}
}

func TestBuildDefinitionUsesContainersConfigRuntimeDefaults(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live containers.conf default coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	configPath := filepath.Join(root, "containers.conf")
	if err := os.WriteFile(configPath, []byte("[containers]\nshm_size = \"12m\"\ndefault_ulimits = [\"nofile=333:333\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_CONF", configPath)
	controls, err := ParseRunControls(RunControlInput{})
	if err != nil {
		t.Fatal(err)
	}
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	def, err := definition.Parse(strings.NewReader(fmt.Sprintf(`
from %q
run "set -e; test \"$(ulimit -n)\" = 333; test \"$(/bin/busybox df -k /dev/shm | /bin/busybox awk 'NR == 2 { print $2 }')\" -ge 12288; printf ready >/proof" network="none"
`, base.reference)))
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(root, "layout")
	_, err = BuildDefinitionSupervised(ctx, def, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Network: "none",
		Output: Output{Path: layout}, RunControls: controls,
		Stdout: os.Stdout, Stderr: os.Stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded()), "proof"); proof != "ready" {
		t.Fatalf("containers.conf defaults proof = %q", proof)
	}
}

func TestBuildDefinitionExecutesNativeBuildNetworkModes(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live build-network coverage")
	}
	for _, test := range []struct {
		network          string
		allowSetnsDenied bool
	}{
		{network: "private"},
		{network: "pasta:--map-gw"},
		{network: "ns:/proc/self/ns/net", allowSetnsDenied: true},
	} {
		t.Run(strings.ReplaceAll(test.network, "/", "_"), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			root := t.TempDir()
			store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
			base := newLiveBusyBoxStorage(t, ctx, root, store)
			def, err := definition.Parse(strings.NewReader(fmt.Sprintf("from %q\nrun \"printf ready >/proof\"\n", base.reference)))
			if err != nil {
				t.Fatal(err)
			}
			layout := filepath.Join(root, "layout")
			var stderr bytes.Buffer
			_, err = BuildDefinitionSupervised(ctx, def, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH}, SupervisedPlanOptions{
				Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Network: test.network,
				Output: Output{Path: layout},
				Stdout: os.Stdout, Stderr: &stderr,
			})
			if err != nil {
				if test.allowSetnsDenied && strings.Contains(stderr.String(), "setns `/proc/self/ns/net`: Operation not permitted") {
					t.Logf("native namespace join reached the runtime and was denied by this host: %v", err)
					return
				}
				t.Fatalf("%v\nstderr:\n%s", err, stderr.String())
			}
			manifest, _ := readPlanImage(t, layout)
			if proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded()), "proof"); proof != "ready" {
				t.Fatalf("network %q proof = %q", test.network, proof)
			}
		})
	}
}
