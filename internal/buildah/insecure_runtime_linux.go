//go:build linux

package buildah

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/moby/sys/capability"
	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

const (
	insecureRunRequestedMarker = "coopr.internal.run-security-insecure.v1"
	insecureRuntimeExecutable  = "/proc/self/exe"
	insecureRuntimeMarker      = "coopr-buildah-insecure-runtime-v1"
)

func runInsecureRuntimeWrapper() {
	if err := executeInsecureRuntime(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "coopr: insecure OCI runtime wrapper: %v\n", err)
		os.Exit(125)
	}
}

func executeInsecureRuntime(arguments []string) error {
	if len(arguments) < 3 || arguments[0] != insecureRuntimeMarker {
		return errors.New("invalid private invocation")
	}
	realRuntime := arguments[1]
	runtimeArguments := slices.Clone(arguments[2:])
	commandIndex := runtimeCommandIndex(runtimeArguments)
	if commandIndex < 0 {
		return fmt.Errorf("could not identify OCI runtime command in %q", runtimeArguments)
	}
	if runtimeArguments[commandIndex] == "create" {
		bundle, err := runtimeBundlePath(runtimeArguments[commandIndex+1:])
		if err != nil {
			return err
		}
		if err := makeInsecureRuntimeSpec(filepath.Join(bundle, "config.json")); err != nil {
			return err
		}
	}
	return unix.Exec(realRuntime, append([]string{realRuntime}, runtimeArguments...), os.Environ())
}

func runtimeCommandIndex(arguments []string) int {
	for index, argument := range arguments {
		switch argument {
		case "create", "start", "state", "kill", "delete":
			return index
		}
	}
	return -1
}

func runtimeBundlePath(arguments []string) (string, error) {
	for index, argument := range arguments {
		if argument == "--bundle" || argument == "-b" {
			if index+1 >= len(arguments) || arguments[index+1] == "" {
				return "", errors.New("OCI runtime create has an empty bundle path")
			}
			return arguments[index+1], nil
		}
		if bundle, found := cutRuntimeOption(argument, "--bundle="); found {
			if bundle == "" {
				return "", errors.New("OCI runtime create has an empty bundle path")
			}
			return bundle, nil
		}
	}
	return "", errors.New("OCI runtime create is missing --bundle")
}

func cutRuntimeOption(argument, prefix string) (string, bool) {
	if len(argument) < len(prefix) || argument[:len(prefix)] != prefix {
		return "", false
	}
	return argument[len(prefix):], true
}

func makeInsecureRuntimeSpec(configPath string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read OCI runtime config: %w", err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("decode OCI runtime config: %w", err)
	}
	if spec.Process == nil || spec.Linux == nil || spec.Linux.Resources == nil {
		return errors.New("OCI runtime config lacks Linux process resources")
	}
	if spec.Process.Capabilities == nil {
		spec.Process.Capabilities = &specs.LinuxCapabilities{}
	}
	if err := applyBuildKitInsecureSpec(context.Background(), &spec); err != nil {
		return fmt.Errorf("apply BuildKit insecure security mode: %w", err)
	}
	spec.Process.SelinuxLabel = ""
	spec.Linux.MountLabel = ""
	spec.Linux.Seccomp = nil
	spec.Process.NoNewPrivileges = false
	makeRuntimeMountsWritable(&spec)
	updated, err := json.Marshal(&spec)
	if err != nil {
		return fmt.Errorf("encode insecure OCI runtime config: %w", err)
	}
	if err := atomicWriteRuntimeConfig(configPath, updated); err != nil {
		return fmt.Errorf("write insecure OCI runtime config: %w", err)
	}
	return nil
}

// applyBuildKitInsecureSpec mirrors BuildKit v0.33's Linux insecure
// entitlement without importing containerd's executor packages. Keep this in
// sync with util/entitlements/security.WithInsecureSpec.
func applyBuildKitInsecureSpec(_ context.Context, spec *specs.Spec) error {
	capabilities, err := currentCapabilityNames()
	if err != nil {
		return err
	}
	spec.Process.Capabilities.Bounding = append(spec.Process.Capabilities.Bounding, capabilities...)
	spec.Process.Capabilities.Ambient = append(spec.Process.Capabilities.Ambient, capabilities...)
	spec.Process.Capabilities.Effective = append(spec.Process.Capabilities.Effective, capabilities...)
	spec.Process.Capabilities.Inheritable = append(spec.Process.Capabilities.Inheritable, capabilities...)
	spec.Process.Capabilities.Permitted = append(spec.Process.Capabilities.Permitted, capabilities...)
	spec.Linux.ReadonlyPaths = []string{}
	spec.Linux.MaskedPaths = []string{}
	spec.Process.ApparmorProfile = ""
	spec.Linux.Resources.Devices = []specs.LinuxDeviceCgroup{
		{Allow: true, Type: "c", Access: "rwm"},
		{Allow: true, Type: "b", Access: "rwm"},
	}
	if !runningInUserNamespace() {
		spec.Linux.Devices = append(spec.Linux.Devices,
			specs.LinuxDevice{Path: "/dev/kmsg", Type: "c", Major: 1, Minor: 11},
			specs.LinuxDevice{Path: "/dev/cuse", Type: "c", Major: 10, Minor: 203},
			specs.LinuxDevice{Path: "/dev/fuse", Type: "c", Major: 10, Minor: 229},
			specs.LinuxDevice{Path: "/dev/kvm", Type: "c", Major: 10, Minor: 232},
			specs.LinuxDevice{Path: "/dev/net/tun", Type: "c", Major: 10, Minor: 200},
			specs.LinuxDevice{Path: "/dev/loop-control", Type: "c", Major: 10, Minor: 237},
		)
		loopID := freeLoopDeviceID()
		for index := 0; index <= loopID+7; index++ {
			spec.Linux.Devices = append(spec.Linux.Devices, specs.LinuxDevice{Path: fmt.Sprintf("/dev/loop%d", index), Type: "b", Major: 7, Minor: int64(index)})
		}
	}
	return nil
}

func currentCapabilityNames() ([]string, error) {
	status, err := os.Open("/proc/self/status")
	if err != nil {
		return nil, fmt.Errorf("read current capabilities: %w", err)
	}
	defer func() { _ = status.Close() }()
	var effective uint64
	found := false
	scanner := bufio.NewScanner(status)
	for scanner.Scan() {
		name, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok || strings.TrimSpace(name) != "CapEff" {
			continue
		}
		effective, err = strconv.ParseUint(strings.TrimSpace(value), 16, 64)
		if err != nil {
			return nil, fmt.Errorf("parse current capabilities: %w", err)
		}
		found = true
		break
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read current capabilities: %w", err)
	}
	if !found {
		return nil, errors.New("read current capabilities: CapEff is missing from /proc/self/status")
	}
	known := capability.ListKnown()
	capabilities := make([]string, 0, len(known))
	for _, item := range known {
		if uint(item) < 64 && effective&(uint64(1)<<uint(item)) != 0 {
			capabilities = append(capabilities, "CAP_"+strings.ToUpper(item.String()))
		}
	}
	return capabilities, nil
}

func runningInUserNamespace() bool {
	const initialUserNamespaceInode = 0xEFFFFFFD
	var stat syscall.Stat_t
	if err := syscall.Stat("/proc/self/ns/user", &stat); err != nil {
		return false
	}
	return stat.Ino != initialUserNamespaceInode
}

func freeLoopDeviceID() int {
	file, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return 0
	}
	defer func() { _ = file.Close() }()
	const loopControlGetFree = 0x4C82
	value, _, errno := unix.Syscall(unix.SYS_IOCTL, file.Fd(), loopControlGetFree, 0)
	if errno != 0 {
		return 0
	}
	return int(value)
}

func makeRuntimeMountsWritable(spec *specs.Spec) {
	for index := range spec.Mounts {
		mount := &spec.Mounts[index]
		if mount.Type != "sysfs" && mount.Type != "cgroup" {
			continue
		}
		for optionIndex, option := range mount.Options {
			if option == "ro" {
				mount.Options[optionIndex] = "rw"
			}
		}
	}
}

func atomicWriteRuntimeConfig(path string, data []byte) (retErr error) {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".coopr-insecure-config-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := temporary.Close(); closeErr != nil && retErr == nil {
				retErr = closeErr
			}
		}
		if removeErr := os.Remove(temporaryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && retErr == nil {
			retErr = removeErr
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	closed = true
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return nil
}
