package build

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage/pkg/reexec"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// This test exercises the assembled release container, including an optional
// static release CLI staged into that container by scripts/acceptance/release.sh.
func TestPackagedAcceptance(t *testing.T) {
	if os.Getenv("COOPR_TEST_PACKAGED_ACCEPTANCE") != "1" {
		t.Skip("set COOPR_TEST_PACKAGED_ACCEPTANCE=1 for packaged acceptance")
	}
	if runtime.GOOS != "linux" || os.Getuid() == 0 {
		t.Fatal("packaged acceptance requires a non-root Linux user")
	}
	if got := (packagedAcceptance{t: t, ctx: context.Background()}).podman("info", "--format", "{{.Host.Security.Rootless}}"); got != "true" {
		t.Fatal("rootless Podman is required")
	}
	image := os.Getenv("COOPR_ACCEPTANCE_RUN_IMAGE")
	if image == "" {
		t.Fatal("COOPR_ACCEPTANCE_RUN_IMAGE is required; use just packaged-acceptance")
	}
	handler := os.Getenv("COOPR_TEST_BINFMT_HANDLER")
	if handler == "" {
		switch runtime.GOARCH {
		case "amd64":
			handler = "qemu-aarch64"
		case "arm64":
			handler = "qemu-x86_64"
		default:
			t.Fatal("unsupported host architecture")
		}
	}
	if filepath.Base(handler) != handler || handler == "." || handler == ".." {
		t.Fatal("invalid binfmt handler")
	}
	registration, err := os.ReadFile(filepath.Join("/proc/sys/fs/binfmt_misc", handler))
	if err != nil || !strings.HasPrefix(string(registration), "enabled\n") {
		t.Fatalf("foreign RUN requires enabled binfmt handler %s: %v", handler, err)
	}
	fixed := false
	for _, line := range strings.Split(string(registration), "\n") {
		if strings.HasPrefix(line, "flags:") && strings.Contains(line, "F") {
			fixed = true
		}
	}
	if !fixed {
		t.Fatal("foreign RUN requires binfmt F flag")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 29*time.Minute)
	defer cancel()
	root := t.TempDir()
	t.Cleanup(func() {
		// Storage entries can be owned by subordinate UIDs; use Podman's normal
		// rootless user namespace to remove only this test's temporary directory.
		command := exec.Command("podman", "unshare", "rm", "-rf", "--", root)
		command.Env = hostPodmanEnv()
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("remove packaged acceptance workspace: %v: %s", err, output)
		}
	})
	// Nested container users must be able to traverse and write their bind mounts.
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	a := packagedAcceptance{t: t, ctx: ctx, image: image, workspace: filepath.Join(root, "workspace"), state: filepath.Join(root, "state"), platform: "linux/" + runtime.GOARCH, loaded: make(map[string]*testing.T)}
	for _, dir := range []string{a.workspace, a.state} {
		if err := os.Mkdir(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	if entrypoint := os.Getenv("COOPR_ACCEPTANCE_ENTRYPOINT"); entrypoint != "" {
		a.entrypoint = []string{"--entrypoint=" + entrypoint}
	}
	for _, args := range [][]string{{"--help"}, {"build", "--help"}, {"component", "build", "--help"}} {
		a.podman(append(append([]string{"run", "--rm", "--network=none", "--cap-drop=all", "--security-opt=no-new-privileges"}, a.entrypoint...), append([]string{a.image}, args...)...)...)
	}
	a.copyExecutable("busybox", commandPath(t, "busybox"))
	for _, arch := range []string{"amd64", "arm64"} {
		fixtureRoot, supplied := os.LookupEnv("COOPR_TEST_FIXTURES")
		if supplied {
			if !filepath.IsAbs(fixtureRoot) {
				t.Fatal("COOPR_TEST_FIXTURES must be absolute")
			}
			source := filepath.Join(fixtureRoot, "acceptance-proof-"+arch)
			if err := validateStorageTestCLI(source); err != nil {
				t.Fatal(err)
			}
			a.copyExecutable("proof-"+arch, source)
		} else {
			command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", filepath.Join(a.workspace, "proof-"+arch), filepath.Join(a.repoRoot(), "scripts/acceptance/fixtures/platform-proof/main.go"))
			command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("build %s fixture: %v: %s", arch, err, output)
			}
		}
	}
	const base = "coopr-release-base:acceptance"
	const component = "coopr-release-component-acceptance"
	const multi = "coopr-release-multi:acceptance"
	const multiComponent = "coopr-release-multi-component-acceptance"
	definitions := map[string]string{
		"base": `from "scratch"
copy "busybox" "/bin/sh"
copy "busybox" "/bin/cat"
copy "busybox" "/bin/busybox"
run "printf 'base\\n' >/proof"
`,
		"child": `from "` + base + `" as="source"
from "` + base + `"
run "cat /input >/mounted-proof" { mount "bind" from="source" source="/proof" target="/input" }
run "test -z \"$(/bin/busybox ip route show default)\""
run "printf 'child\\n' >>/proof"
cmd { exec "/bin/cat" "/proof" "/mounted-proof" }
`,
		"component": "extend\nrun \"printf 'component\\\\n' >>/proof\"\n",
		"component-child": `from "` + base + `"
component "local:` + component + `"
cmd { exec "/bin/cat" "/proof" }
`,
		"named-child": `from "base"
copy "context-payload" "/context-payload" from="assets"
cmd { exec "/bin/cat" "/proof" "/context-payload" }
`,
		"multi-platform": `from "scratch"
arg "TARGETARCH"
copy "proof-$TARGETARCH" "/foreign-proof" chmod="0755"
run { exec "/foreign-proof" }
label "dev.strio.coopr.release-acceptance"="multi-platform"
cmd { exec "/foreign-proof" }
`,
		"multi-component": `package as="payload"
arg "TARGETARCH"
copy "proof-$TARGETARCH" "/component-proof" chmod="0755"
extend
copy "/component-proof" "/component-proof" from="payload"
`,
		"multi-component-child": `from "scratch"
component "local:` + multiComponent + `"
run { exec "/component-proof" }
cmd { exec "/component-proof" }
`,
	}
	for name, definition := range definitions {
		a.write(name+".coopr", definition)
	}
	a.write("assets/context-payload", "named-context\n")
	t.Run("ContainerfileParity", func(t *testing.T) { a.t = t; a.conformance() })
	t.Run("MultiPlatformImageAndComponent", func(t *testing.T) {
		a.t = t
		a.coopr(false, "none", "build", "/work/multi-platform.coopr", "--tag", multi, "--platform", "linux/amd64", "--platform", "linux/arm64")
		a.coopr(false, "none", "copy", multi, "oci-archive:/work/multi-platform.oci.tar")
		a.checkIndex(filepath.Join(a.workspace, "multi-platform.oci.tar"))
		a.coopr(false, "none", "component", "build", "/work/multi-component.coopr", "--tag", multiComponent, "--platform", "linux/amd64", "--platform", "linux/arm64")
		for _, arch := range []string{"amd64", "arm64"} {
			a.coopr(false, "none", "copy", multi, "oci-archive:/work/multi-"+arch+".oci.tar", "--platform", "linux/"+arch)
			a.assertImage("multi-"+arch+".oci.tar", arch, "linux/"+arch)
			a.coopr(false, "none", "build", "/work/multi-component-child.coopr", "--tag", "oci-archive:/work/multi-component-"+arch+".oci.tar", "--platform", "linux/"+arch)
			a.assertImage("multi-component-"+arch+".oci.tar", arch, "linux/"+arch)
		}
	})
	t.Run("SharedStoreAndContexts", func(t *testing.T) {
		a.t = t
		a.coopr(false, "slirp4netns", "build", "/work/base.coopr", "--tag", base, "--platform", a.platform)
		a.coopr(false, "none", "build", "/work/child.coopr", "--tag", "oci-archive:/work/child.oci.tar", "--platform", a.platform)
		a.assertImage("child.oci.tar", "base\nchild\nbase", a.platform)
		a.coopr(false, "none", "build", "/work/named-child.coopr", "--build-context", "base=docker-image://"+base, "--build-context", "assets=/work/assets", "--tag", "oci-archive:/work/named-child.oci.tar", "--platform", a.platform)
		a.assertImage("named-child.oci.tar", "base\nnamed-context", a.platform)
		a.coopr(false, "none", "build", "/work/child.coopr", "--format", "docker", "--tag", "oci-archive:/work/child-docker.oci.tar", "--platform", a.platform)
		a.assertImage("child-docker.oci.tar", "base\nchild\nbase", a.platform)
		if _, err := os.Stat(filepath.Join(a.state, "containers/storage/overlay/.has-mount-program")); err != nil {
			t.Fatalf("packaged builds did not use fuse-overlayfs: %v", err)
		}
	})
	t.Run("NetworkRejection", func(t *testing.T) {
		a.t = t
		a.write("network-global.coopr", "from \""+base+"\"\nrun \"printf COOPR_NETWORK_PROOF_EXECUTED\"\n")
		a.write("network-authored.coopr", "from \""+base+"\"\nrun \"printf COOPR_NETWORK_PROOF_EXECUTED\" network=\"none\"\n")
		a.write("network-component.coopr", "extend\nrun \"printf COOPR_NETWORK_PROOF_EXECUTED\" network=\"none\"\n")
		a.write("network-component-child.coopr", "from \""+base+"\"\ncomponent \"./network-component.coopr\"\n")
		for _, name := range []string{"global", "authored", "component"} {
			definition := "/work/network-" + name + ".coopr"
			args := []string{"build", definition}
			switch name {
			case "global":
				args = append(args, "--network=none")
			case "component":
				args[1] = "/work/network-component-child.coopr"
			}
			args = append(args, "--isolation=chroot", "--no-cache", "--platform", a.platform, "--tag", "oci-archive:/work/network-"+name+".oci.tar")
			output, err := a.tryCoopr(false, "slirp4netns", args...)
			if err == nil || !strings.Contains(output, "RUN network=none cannot be used with chroot isolation; use --isolation=oci or --isolation=rootless") {
				t.Fatalf("%s rejection: %v: %s", name, err, output)
			}
			for _, line := range strings.Split(output, "\n") {
				if line == "COOPR_NETWORK_PROOF_EXECUTED" || strings.HasPrefix(line, "COOPR_NETWORK_PROOF_EXECUTEDError:") {
					t.Fatalf("rejected %s RUN executed", name)
				}
			}
			if _, err := os.Stat(filepath.Join(a.workspace, "network-"+name+".oci.tar")); !os.IsNotExist(err) {
				t.Fatalf("rejected %s build produced output: %v", name, err)
			}
		}
	})
	t.Run("RootlessOCIComponentCache", func(t *testing.T) {
		a.t = t
		a.state = filepath.Join(root, "oci-state")
		if err := os.Mkdir(a.state, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(a.state, 0o777); err != nil {
			t.Fatal(err)
		}
		a.coopr(true, "none", "build", "/work/base.coopr", "--isolation=rootless", "--network=none", "--tag", base, "--platform", a.platform)
		a.coopr(true, "none", "component", "build", "/work/component.coopr", "--isolation=rootless", "--network=none", "--tag", component, "--platform", a.platform)
		a.coopr(true, "none", "build", "/work/component-child.coopr", "--isolation=rootless", "--network=none", "--tag", "oci-archive:/work/component-child.oci.tar", "--cache-from", "oci-layout:/var/lib/coopr/component-cache", "--cache-to", "oci-layout:/var/lib/coopr/component-cache", "--platform", a.platform)
		info, err := os.Stat(filepath.Join(a.state, "coopr/component-cache/index.json"))
		if err != nil || info.Size() == 0 {
			t.Fatalf("component cache not populated: %v", err)
		}
		a.assertImage("component-child.oci.tar", "base\ncomponent", a.platform)
		a.coopr(true, "none", "build", "/work/multi-platform.coopr", "--isolation=rootless", "--network=none", "--platform", a.platform, "--tag", "oci-archive:/work/oci-override.oci.tar")
		info, err = os.Stat(filepath.Join(a.workspace, "oci-override.oci.tar"))
		if err != nil || info.Size() == 0 {
			t.Fatalf("OCI override produced no image: %v", err)
		}
	})
}

type packagedAcceptance struct {
	t                                 *testing.T
	ctx                               context.Context
	image, workspace, state, platform string
	entrypoint                        []string
	loaded                            map[string]*testing.T
}

func commandPath(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
func (a packagedAcceptance) repoRoot() string {
	if root := os.Getenv("COOPR_ACCEPTANCE_REPO_ROOT"); root != "" {
		return root
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		a.t.Fatal(err)
	}
	return root
}
func (a packagedAcceptance) podman(args ...string) string {
	a.t.Helper()
	output, diagnostics, err := packagedCommandOutput(exec.CommandContext(a.ctx, "podman", args...))
	if err != nil {
		a.t.Fatalf("podman %v: %v: stdout: %s; stderr: %s", args, err, output, diagnostics)
	}
	if diagnostics != "" {
		a.t.Logf("podman stderr: %s", diagnostics)
	}
	return strings.TrimSpace(output)
}
func (a packagedAcceptance) write(name, data string) {
	a.t.Helper()
	path := filepath.Join(a.workspace, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		a.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o666); err != nil {
		a.t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		a.t.Fatal(err)
	}
}
func (a packagedAcceptance) copyExecutable(name, source string) {
	a.t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		a.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.workspace, name), data, 0o755); err != nil {
		a.t.Fatal(err)
	}
}
func (a packagedAcceptance) tryCoopr(oci bool, network string, args ...string) (string, error) {
	flags := []string{"run", "--rm", "--network=" + network, "--device=/dev/fuse:rw", "--security-opt=seccomp=unconfined", "--security-opt=label=disable"}
	if oci {
		flags = append(flags, "--userns=keep-id:uid=1000,gid=1000", "--user=1000:1000", "--cap-add=SYS_ADMIN", "--security-opt=unmask=ALL", "--env", "HOME=/home/user", "--env", "USER=user", "--env", "XDG_RUNTIME_DIR=/run/user/1000")
	}
	flags = append(flags, "--mount", "type=bind,src="+a.workspace+",dst=/work,rw", "--mount", "type=bind,src="+a.state+",dst=/var/lib,rw")
	flags = append(flags, a.entrypoint...)
	flags = append(flags, a.image)
	flags = append(flags, args...)
	output, err := exec.CommandContext(a.ctx, "podman", flags...).CombinedOutput()
	return string(output), err
}
func (a packagedAcceptance) coopr(oci bool, network string, args ...string) {
	a.t.Helper()
	output, err := a.tryCoopr(oci, network, args...)
	if err != nil {
		a.t.Fatalf("packaged coopr %v: %v: %s", args, err, output)
	}
	a.t.Log(output)
}
func (a packagedAcceptance) loadImage(archive string) string {
	a.t.Helper()
	manifest, _ := readExampleImage(a.t, a.ctx, archive)
	id := manifest.Config.Digest.String()
	a.podman("load", "-i", archive)
	a.podman("image", "exists", id)
	if a.loaded[id] == a.t {
		return id
	}
	a.loaded[id] = a.t
	a.t.Cleanup(func() {
		output, err := exec.Command("podman", "rmi", id).CombinedOutput()
		if err != nil {
			a.t.Errorf("remove packaged test image %s: %v: %s", id, err, output)
		}
	})
	return id
}
func (a packagedAcceptance) assertImage(archive, want, platform string) {
	a.t.Helper()
	id := a.loadImage(filepath.Join(a.workspace, archive))
	if got := a.podman("run", "--rm", "--network=none", "--platform", platform, id); got != want {
		a.t.Fatalf("%s runtime output=%q, want %q", archive, got, want)
	}
}
func (a packagedAcceptance) checkIndex(archive string) {
	a.t.Helper()
	store, err := orasoci.NewFromTar(a.ctx, archive)
	if err != nil {
		a.t.Fatal(err)
	}
	var root v1.Index
	file, err := os.Open(archive)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			a.t.Errorf("close archive: %v", err)
		}
	}()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err != nil {
			a.t.Fatal(err)
		}
		if strings.TrimPrefix(header.Name, "./") == "index.json" {
			if err := json.NewDecoder(reader).Decode(&root); err != nil {
				a.t.Fatal(err)
			}
			break
		}
	}
	if len(root.Manifests) != 1 || root.Manifests[0].MediaType != v1.MediaTypeImageIndex {
		a.t.Fatalf("archive root=%+v", root)
	}
	data, err := content.FetchAll(a.ctx, store, root.Manifests[0])
	if err != nil {
		a.t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		a.t.Fatal(err)
	}
	if index.MediaType != v1.MediaTypeImageIndex || len(index.Manifests) != 2 {
		a.t.Fatalf("multi-platform index=%+v", index)
	}
	platforms := map[string]bool{}
	for _, desc := range index.Manifests {
		if desc.Platform == nil {
			a.t.Fatal("manifest lacks platform")
		}
		platform := desc.Platform.OS + "/" + desc.Platform.Architecture
		platforms[platform] = true
		data, err := content.FetchAll(a.ctx, store, desc)
		if err != nil {
			a.t.Fatal(err)
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			a.t.Fatal(err)
		}
		data, err = content.FetchAll(a.ctx, store, manifest.Config)
		if err != nil {
			a.t.Fatal(err)
		}
		var config v1.Image
		if err := json.Unmarshal(data, &config); err != nil {
			a.t.Fatal(err)
		}
		if got := config.OS + "/" + config.Architecture; got != platform {
			a.t.Fatalf("child platform=%s, want %s", got, platform)
		}
	}
	if !reflect.DeepEqual(platforms, map[string]bool{"linux/amd64": true, "linux/arm64": true}) {
		a.t.Fatalf("index platforms=%v", platforms)
	}
}

func (a packagedAcceptance) conformance() {
	a.t.Helper()
	dir := filepath.Join(a.workspace, "containerfile-conformance")
	if err := os.CopyFS(dir, os.DirFS(filepath.Join(a.repoRoot(), "scripts/acceptance/fixtures/containerfile-conformance"))); err != nil {
		a.t.Fatal(err)
	}
	for _, name := range []string{"busybox"} {
		data, err := os.ReadFile(commandPath(a.t, name))
		if err != nil {
			a.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
			a.t.Fatal(err)
		}
	}
	file, err := os.Create(filepath.Join(dir, "payload.tar"))
	if err != nil {
		a.t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	data := []byte("archive:static\n")
	if err := writer.WriteHeader(&tar.Header{Name: "archive-data", Mode: 0o644, Size: int64(len(data))}); err != nil {
		a.t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		a.t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		a.t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		a.t.Fatal(err)
	}
	tag := fmt.Sprintf("localhost/coopr-conformance:%d-%d", os.Getpid(), time.Now().UnixNano())
	a.podman("build", "--pull=never", "--format", "docker", "--layers", "--network=none", "--build-arg", "message=release-acceptance", "--file", filepath.Join(dir, "Containerfile"), "--tag", tag, dir)
	a.t.Cleanup(func() {
		output, err := exec.Command("podman", "rmi", tag).CombinedOutput()
		if err != nil {
			a.t.Errorf("cleanup conformance image: %v: %s", err, output)
		}
	})
	a.coopr(false, "none", "build", "--file", "/work/containerfile-conformance/image.coopr", "/work/containerfile-conformance", "--build-arg", "message=release-acceptance", "--format", "docker", "--tag", "oci-archive:/work/containerfile-conformance/coopr.oci.tar", "--platform", a.platform)
	built := a.loadImage(filepath.Join(dir, "coopr.oci.tar"))
	want := "stage:release-acceptance\ncontext:static\nfinal:release-acceptance\nenv:containerfile\narchive:static"
	for _, image := range []string{tag, built} {
		if got := a.podman("run", "--rm", "--network=none", image); got != want {
			a.t.Fatalf("conformance runtime=%q, want %q", got, want)
		}
	}
	configs := make([]map[string]any, 0, 2)
	for i, image := range []string{tag, built} {
		archive := filepath.Join(a.workspace, fmt.Sprintf("config-%d.tar", i))
		a.podman("save", "--format", "docker-archive", "--output", archive, image)
		config, err := packagedDockerConfig(archive)
		if err != nil {
			a.t.Fatal(err)
		}
		configs = append(configs, config)
	}
	if !reflect.DeepEqual(configs[0], configs[1]) {
		left, _ := json.MarshalIndent(configs[0], "", "  ")
		right, _ := json.MarshalIndent(configs[1], "", "  ")
		a.t.Fatalf("Containerfile config:\n%s\nCoopr config:\n%s", left, right)
	}
}

func packagedDockerConfig(path string) (config map[string]any, resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	reader := tar.NewReader(file)
	entries := map[string][]byte{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if header.Name != "manifest.json" && !strings.HasSuffix(header.Name, ".json") {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(reader, 16<<20))
		if err != nil {
			return nil, err
		}
		entries[strings.TrimPrefix(header.Name, "./")] = data
	}
	var manifest []struct{ Config string }
	if err := json.Unmarshal(entries["manifest.json"], &manifest); err != nil {
		return nil, err
	}
	if len(manifest) != 1 {
		return nil, fmt.Errorf("Docker archive has %d images", len(manifest))
	}
	var image struct{ Config map[string]any }
	if err := json.Unmarshal(entries[manifest[0].Config], &image); err != nil {
		return nil, err
	}
	return selectedPackagedConfig(image.Config), nil
}
func selectedPackagedConfig(config map[string]any) map[string]any {
	selected := map[string]any{}
	for _, key := range []string{"Cmd", "Entrypoint", "ExposedPorts", "Healthcheck", "OnBuild", "Shell", "StopSignal", "User", "Volumes", "WorkingDir"} {
		selected[key] = config[key]
	}
	env := []any{}
	if values, ok := config["Env"].([]any); ok {
		for _, value := range values {
			if s, ok := value.(string); ok && strings.HasPrefix(s, "COOPR_PARITY=") {
				env = append(env, s)
			}
		}
	}
	selected["Env"] = env
	labels := map[string]any{}
	if values, ok := config["Labels"].(map[string]any); ok {
		labels["dev.strio.coopr.conformance"] = values["dev.strio.coopr.conformance"]
	}
	selected["Labels"] = labels
	return selected
}
func TestSelectedPackagedConfig(t *testing.T) {
	original := map[string]any{"Cmd": []any{"/bin/cat"}, "Env": []any{"PATH=/bin", "COOPR_PARITY=containerfile"}, "Labels": map[string]any{"dev.strio.coopr.conformance": "ordered", "unrelated": "one"}, "WorkingDir": "/proof"}
	same := map[string]any{"Cmd": []any{"/bin/cat"}, "Env": []any{"PATH=/usr/bin", "COOPR_PARITY=containerfile"}, "Labels": map[string]any{"dev.strio.coopr.conformance": "ordered", "unrelated": "two"}, "WorkingDir": "/proof", "Created": "ignored"}
	want := selectedPackagedConfig(original)
	if !reflect.DeepEqual(want, selectedPackagedConfig(same)) {
		t.Fatal("irrelevant metadata affected parity")
	}
	for _, key := range []string{"Cmd", "Entrypoint", "Env", "ExposedPorts", "Healthcheck", "Labels", "OnBuild", "Shell", "StopSignal", "User", "Volumes", "WorkingDir"} {
		t.Run(key, func(t *testing.T) {
			changed := make(map[string]any, len(original))
			for name, value := range original {
				changed[name] = value
			}
			switch key {
			case "Env":
				changed[key] = []any{"COOPR_PARITY=changed"}
			case "Labels":
				changed[key] = map[string]any{"dev.strio.coopr.conformance": "changed"}
			default:
				changed[key] = "changed"
			}
			if reflect.DeepEqual(want, selectedPackagedConfig(changed)) {
				t.Fatalf("%s ignored", key)
			}
		})
	}
}

// Runtime assertions compare stdout; successful engine warnings remain diagnostics.
func packagedCommandOutput(command *exec.Cmd) (string, string, error) {
	var output, diagnostics bytes.Buffer
	command.Stdout = &output
	command.Stderr = &diagnostics
	err := command.Run()
	return output.String(), diagnostics.String(), err
}

func TestPackagedCommandOutput(t *testing.T) {
	for _, status := range []string{"0", "7"} {
		t.Run(status, func(t *testing.T) {
			output, diagnostics, err := packagedCommandOutput(exec.Command("sh", "-c", "printf 'expected\n'; printf 'engine warning\n' >&2; exit "+status))
			if output != "expected\n" || diagnostics != "engine warning\n" {
				t.Fatalf("stdout=%q, stderr=%q", output, diagnostics)
			}
			if (err == nil) != (status == "0") {
				t.Fatalf("status %s: error=%v", status, err)
			}
		})
	}
}

func TestPackagedCallerEnvironment(t *testing.T) {
	if os.Getenv("COOPR_PACKAGED_ENV_CONTROL") == "1" {
		if got := strconv.Itoa(os.Getuid()); got != os.Getenv("COOPR_PACKAGED_EXPECT_UID") {
			t.Fatalf("caller UID changed: %s", got)
		}
		if got := os.Getenv("XDG_DATA_HOME"); got != os.Getenv("COOPR_PACKAGED_EXPECT_XDG") {
			t.Fatalf("caller data home changed: %s", got)
		}
		return
	}
	dataHome := t.TempDir()
	// Storage's reexec helper addresses the live executable even when the
	// native test harness was reexecuted from an anonymous memfd.
	command := exec.Command(reexec.Self(), "-test.run", "^TestPackagedCallerEnvironment$", "-test.v")
	command.Env = append(os.Environ(),
		"COOPR_TEST_PACKAGED_ACCEPTANCE=1", "COOPR_TEST_BUILDAH=1", "COOPR_TEST_BUILDAH_REGISTRY=1", "COOPR_TEST_CONTAINER_STORAGE=1",
		"COOPR_PACKAGED_ENV_CONTROL=1", "COOPR_PACKAGED_EXPECT_UID="+strconv.Itoa(os.Getuid()),
		"XDG_DATA_HOME="+dataHome, "COOPR_PACKAGED_EXPECT_XDG="+dataHome,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("packaged caller environment: %v: %s", err, output)
	}
}
