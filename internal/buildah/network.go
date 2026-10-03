package buildah

import (
	"fmt"
	"os"
	"path/filepath"

	"go.podman.io/common/libnetwork/netavark"
	nettypes "go.podman.io/common/libnetwork/types"
	commonconfig "go.podman.io/common/pkg/config"
	"go.podman.io/storage"
)

const cooprDefaultNetwork = "coopr"

// newNetworkInterface creates the network backend explicitly so NewBuilder
// does not load the host's containers.conf or require netavark to be present
// merely to create a builder. Helper binaries are resolved when a RUN actually
// configures networking.
func newNetworkInterface(store storage.Store) (nettypes.ContainerNetwork, error) {
	return newNetworkInterfaceWithControls(store, RunControls{})
}

func newNetworkInterfaceWithControls(store storage.Store, controls RunControls) (nettypes.ContainerNetwork, error) {
	initConfig, err := cooprNetworkConfigWithDirectory(store.GraphRoot(), store.RunRoot(), controls.NetworkConfigDir)
	if err != nil {
		return nil, err
	}
	network, err := netavark.NewNetworkInterface(initConfig)
	if err != nil {
		return nil, fmt.Errorf("create Coopr Buildah network interface: %w", err)
	}
	return network, nil
}

func cooprNetworkConfig(graphRoot, runRoot string) (*netavark.InitConfig, error) {
	return cooprNetworkConfigWithDirectory(graphRoot, runRoot, "")
}

func cooprNetworkConfigWithDirectory(graphRoot, runRoot, requestedConfigDir string) (*netavark.InitConfig, error) {
	configDir := requestedConfigDir
	if configDir == "" {
		configDir = filepath.Join(graphRoot, "networks")
	}
	runDir := filepath.Join(runRoot, "networks")
	directories := []string{runDir}
	if requestedConfigDir == "" {
		directories = append(directories, configDir)
	}
	for _, dir := range directories {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create Coopr Buildah network directory %q: %w", dir, err)
		}
	}

	config := &commonconfig.Config{
		Network: commonconfig.NetworkConfig{
			DefaultNetwork:            cooprDefaultNetwork,
			DefaultSubnet:             commonconfig.DefaultSubnet,
			DefaultSubnetPools:        commonconfig.DefaultSubnetPools,
			DefaultRootlessNetworkCmd: "pasta",
			RootlessPortForwarder:     commonconfig.RootlessPortForwarderPasta,
		},
	}
	return &netavark.InitConfig{
		Config:           config,
		NetworkConfigDir: configDir,
		NetworkRunDir:    runDir,
		NetavarkBinary:   "netavark",
		AardvarkBinary:   "aardvark-dns",
	}, nil
}
