package buildah

import (
	"go.podman.io/common/libnetwork/network"
	nettypes "go.podman.io/common/libnetwork/types"
	"go.podman.io/storage"
)

func newNetworkInterface(store storage.Store) (nettypes.ContainerNetwork, error) {
	return newNetworkInterfaceWithControls(store, RunControls{})
}

func newNetworkInterfaceWithControls(store storage.Store, controls RunControls) (nettypes.ContainerNetwork, error) {
	config, err := loadRunContainerConfig(controls.ConfigModules)
	if err != nil {
		return nil, err
	}
	if controls.NetworkConfigDir != "" {
		config.Network.NetworkConfigDir = controls.NetworkConfigDir
	}
	_, backend, err := network.NetworkBackend(store, config, false)
	return backend, err
}
