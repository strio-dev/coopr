package buildah

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/opencontainers/runtime-spec/specs-go"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/common/libnetwork/slirp4netns"
	commonconfig "go.podman.io/common/pkg/config"
	"go.podman.io/common/pkg/netns"
)

const slirpNetworkName = "slirp4netns"

func (b nativeBuilder) runWithSlirpNetwork(command []string, options upstream.RunOptions) (retErr error) {
	mode, found := slirpNetworkMode(options.NamespaceOptions)
	if options.Isolation == define.IsolationChroot || !found {
		return b.Run(command, options)
	}

	network, err := prepareSlirpRunNetwork(b.runControls, b.ContainerID, mode)
	if err != nil {
		return err
	}
	defer func() {
		retErr = errors.Join(retErr, network.Close())
	}()

	options.NamespaceOptions = slices.Clone(options.NamespaceOptions)
	options.NamespaceOptions.AddOrReplace(define.NamespaceOption{
		Name: string(specs.NetworkNamespace), Path: network.namespacePath,
	})

	originalCommonBuildOptions := b.CommonBuildOpts
	commonBuildOptions := originalCommonBuildOptions
	if commonBuildOptions == nil {
		commonBuildOptions = &upstream.CommonBuildOptions{}
	}
	updatedCommonBuildOptions := *commonBuildOptions
	updatedCommonBuildOptions.DNSServers = slices.Clone(commonBuildOptions.DNSServers)
	if len(updatedCommonBuildOptions.DNSServers) == 0 {
		updatedCommonBuildOptions.DNSServers = []string{network.dns}
	}
	b.CommonBuildOpts = &updatedCommonBuildOptions
	defer func() { b.CommonBuildOpts = originalCommonBuildOptions }()

	return b.Run(command, options)
}

func slirpNetworkMode(options define.NamespaceOptions) (string, bool) {
	network := options.Find(string(specs.NetworkNamespace))
	if network.Host || (network.Path != slirpNetworkName && !strings.HasPrefix(network.Path, slirpNetworkName+":")) {
		return "", false
	}
	return network.Path, true
}

type slirpRunNetwork struct {
	namespacePath string
	dns           string
	exitWriter    *os.File
	pid           int
	tempDir       string
}

func prepareSlirpRunNetwork(controls RunControls, containerID, mode string) (_ *slirpRunNetwork, retErr error) {
	tempDir, err := os.MkdirTemp("", "coopr-slirp-")
	if err != nil {
		return nil, fmt.Errorf("create slirp4netns runtime directory: %w", err)
	}
	network := &slirpRunNetwork{tempDir: tempDir, namespacePath: filepath.Join(tempDir, "netns")}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, network.Close())
		}
	}()

	namespace, err := netns.NewNSAtPath(network.namespacePath)
	if err != nil {
		return nil, fmt.Errorf("create slirp4netns network namespace: %w", err)
	}
	if err := namespace.Close(); err != nil {
		return nil, fmt.Errorf("close slirp4netns network namespace handle: %w", err)
	}

	config, err := slirpNetworkConfig(controls, tempDir)
	if err != nil {
		return nil, err
	}
	exitReader, exitWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create slirp4netns exit pipe: %w", err)
	}
	network.exitWriter = exitWriter

	if containerID == "" {
		containerID = filepath.Base(tempDir)
	}
	result, setupErr := slirp4netns.Setup(&slirp4netns.SetupOptions{
		Config: config, ContainerID: containerID, Netns: network.namespacePath,
		ExtraOptions: slirpExtraOptions(mode), Slirp4netnsExitPipeR: exitReader,
		Pdeathsig: syscall.SIGKILL,
	})
	if result != nil {
		network.pid = result.Pid
	}
	closeErr := exitReader.Close()
	if setupErr != nil || closeErr != nil {
		return nil, fmt.Errorf("configure slirp4netns: %w", errors.Join(setupErr, closeErr))
	}
	dns, err := slirp4netns.GetDNS(result.Subnet)
	if err != nil {
		return nil, fmt.Errorf("determine slirp4netns DNS server: %w", err)
	}
	network.dns = dns.String()
	return network, nil
}

func slirpNetworkConfig(controls RunControls, tempDir string) (*commonconfig.Config, error) {
	config, err := loadRunContainerConfig(controls.ConfigModules)
	if err != nil {
		return nil, err
	}
	config.Engine.TmpDir = tempDir
	if controls.NetworkCmdPath == "" {
		return config, nil
	}

	helperDir := filepath.Dir(controls.NetworkCmdPath)
	if filepath.Base(controls.NetworkCmdPath) != slirpNetworkName {
		helperDir = filepath.Join(tempDir, "helpers")
		if err := os.Mkdir(helperDir, 0o700); err != nil {
			return nil, fmt.Errorf("create network helper directory: %w", err)
		}
		if err := os.Symlink(controls.NetworkCmdPath, filepath.Join(helperDir, slirpNetworkName)); err != nil {
			return nil, fmt.Errorf("select network helper %q: %w", controls.NetworkCmdPath, err)
		}
	}
	config.Engine.HelperBinariesDir.Set([]string{helperDir})
	return config, nil
}

func slirpExtraOptions(mode string) []string {
	_, options, found := strings.Cut(mode, ":")
	if !found || options == "" {
		return nil
	}
	return strings.Split(options, ",")
}

func (network *slirpRunNetwork) Close() error {
	if network == nil {
		return nil
	}
	var errs []error
	if network.exitWriter != nil {
		if err := network.exitWriter.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close slirp4netns exit pipe: %w", err))
		}
		network.exitWriter = nil
	}
	if network.pid > 0 {
		deadline := time.Now().Add(time.Second)
		for processAlive(network.pid) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if processAlive(network.pid) {
			if err := syscall.Kill(-network.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				errs = append(errs, fmt.Errorf("stop slirp4netns process %d: %w", network.pid, err))
			}
		}
		network.pid = 0
	}
	if network.namespacePath != "" {
		if err := netns.UnmountNS(network.namespacePath); err != nil {
			errs = append(errs, err)
		}
		network.namespacePath = ""
	}
	if network.tempDir != "" {
		if err := os.RemoveAll(network.tempDir); err != nil {
			errs = append(errs, fmt.Errorf("remove slirp4netns runtime directory: %w", err))
		}
		network.tempDir = ""
	}
	return errors.Join(errs...)
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
