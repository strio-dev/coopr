package buildah

import (
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
	"syscall"
	"testing"
	"time"

	"github.com/opencontainers/runtime-spec/specs-go"
	"go.podman.io/buildah/define"
	"go.podman.io/common/libnetwork/slirp4netns"
	"golang.org/x/sys/unix"
)

func TestSlirpNetworkMode(t *testing.T) {
	for _, test := range []struct {
		path string
		want string
		ok   bool
	}{
		{path: "slirp4netns", want: "slirp4netns", ok: true},
		{path: "slirp4netns:mtu=1400,cidr=10.89.0.0/24", want: "slirp4netns:mtu=1400,cidr=10.89.0.0/24", ok: true},
		{path: "pasta"},
		{path: "/run/netns/example"},
	} {
		options := define.NamespaceOptions{{Name: string(specs.NetworkNamespace), Path: test.path}}
		got, ok := slirpNetworkMode(options)
		if got != test.want || ok != test.ok {
			t.Fatalf("slirpNetworkMode(%q) = %q, %t; want %q, %t", test.path, got, ok, test.want, test.ok)
		}
	}
}

func TestSlirpExtraOptionsPreserveNativeOrdering(t *testing.T) {
	got := slirpExtraOptions("slirp4netns:mtu=1400,cidr=10.89.0.0/24,allow_host_loopback=true")
	want := []string{"mtu=1400", "cidr=10.89.0.0/24", "allow_host_loopback=true"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("slirp options = %v, want %v", got, want)
	}
}

func TestSlirpNetworkConfigSelectsExplicitArbitrarilyNamedHelper(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(root, "network-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(root, "runtime")
	if err := os.Mkdir(temporary, 0o700); err != nil {
		t.Fatal(err)
	}
	config, err := slirpNetworkConfig(RunControls{NetworkCmdPath: helper}, temporary)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := config.FindHelperBinary(slirpNetworkName, false)
	if err != nil {
		t.Fatal(err)
	}
	target, err := filepath.EvalSymlinks(selected)
	if err != nil {
		t.Fatal(err)
	}
	if target != helper {
		t.Fatalf("selected network helper = %q -> %q, want %q", selected, target, helper)
	}
}

func TestSlirpNetworkConfigUsesNativeHelperDirectories(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, slirpNetworkName)
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(dir, "containers.conf")
	if err := os.WriteFile(configFile, []byte("[engine]\nhelper_binaries_dir = [\""+dir+"\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_CONF", configFile)
	t.Setenv("CONTAINERS_CONF_OVERRIDE", "")
	config, err := slirpNetworkConfig(RunControls{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := config.FindHelperBinary(slirpNetworkName, true)
	if err != nil {
		t.Fatal(err)
	}
	if selected != helper {
		t.Fatalf("native helper = %q, want %q", selected, helper)
	}
}

func TestSlirpNetworkCloseReapsNativeHelper(t *testing.T) {
	for _, ignoreExitPipe := range []bool{false, true} {
		t.Run(fmt.Sprintf("ignore-exit-pipe=%t", ignoreExitPipe), func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			command := "exec cat <&4 >/dev/null"
			if ignoreExitPipe {
				command = "exec sleep 300"
			}
			network := startSlirpTestHelper(t, "printf '\\001' >&3\n"+command)
			pid := network.pid
			if err := network.Close(); err != nil {
				t.Fatal(err)
			}
			var status syscall.WaitStatus
			waited, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
			if !errors.Is(err, syscall.ECHILD) {
				t.Fatalf("closed helper was not reaped: wait4=%d status=%v err=%v", waited, status, err)
			}
			if err := network.Close(); err != nil {
				t.Fatalf("repeated close: %v", err)
			}
		})
	}
}

func TestSlirpNetworkClosePreservesPipeErrorAndReapsHelper(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	network := startSlirpTestHelper(t, "printf '\\001' >&3\nexec cat <&4 >/dev/null")
	pid := network.pid
	if err := network.exitWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := network.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed exit pipe error was lost: %v", err)
	}
	if waited, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
		t.Fatalf("pipe error prevented helper reaping: wait4=%d err=%v", waited, err)
	}
}

func TestSlirpNetworkCloseStopsWrapperDescendantsBeforeReaping(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	root := t.TempDir()
	fifo := filepath.Join(root, "descendant")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	}()
	pidFile := filepath.Join(root, "pid")
	body := fmt.Sprintf("exec 5>%q\nsleep 300 &\nprintf '%%s' \"$!\" >%q\nprintf '\\001' >&3\nexec cat <&4 >/dev/null", fifo, pidFile)
	network := startSlirpTestHelper(t, body)
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unix.PidfdSendSignal(pidfd, syscall.SIGKILL, nil, 0)
		_ = unix.Close(pidfd)
	})
	if err := network.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reader.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var proof [1]byte
	if _, err := reader.Read(proof[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("wrapper descendant still holds its inherited descriptor: %v", err)
	}
}

func startSlirpTestHelper(t *testing.T, body string) *slirpRunNetwork {
	t.Helper()
	root := t.TempDir()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, slirpNetworkName)
	script := fmt.Sprintf("#!%s\nif [ \"$1\" = --help ]; then exit 0; fi\n%s\n", shell, body)
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	config, err := slirpNetworkConfig(RunControls{NetworkCmdPath: helper}, root)
	if err != nil {
		t.Fatal(err)
	}
	exitReader, exitWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := exitReader.Close(); err != nil {
			t.Error(err)
		}
	}()
	network := &slirpRunNetwork{exitWriter: exitWriter}
	pid := 0
	t.Cleanup(func() {
		if network.exitWriter != nil {
			_ = network.exitWriter.Close()
		}
		if pid > 0 {
			waited, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil)
			if err == nil && waited == 0 {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				_, _ = syscall.Wait4(pid, nil, 0, nil)
			}
		}
	})
	result, err := slirp4netns.Setup(&slirp4netns.SetupOptions{
		Config: config, ContainerID: "test", Netns: filepath.Join(root, "unused-netns"),
		ExtraOptions: []string{"enable_ipv6=false"}, Slirp4netnsExitPipeR: exitReader,
		Pdeathsig: syscall.SIGKILL,
	})
	if err != nil {
		t.Fatal(err)
	}
	network.pid = result.Pid
	pid = result.Pid
	return network
}
