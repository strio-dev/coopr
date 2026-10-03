package buildah

import (
	"os"
	"path/filepath"
	"testing"

	"go.podman.io/common/libnetwork/netavark"
	"go.podman.io/common/pkg/config"
	"go.podman.io/storage/pkg/unshare"
)

func TestCooprNetworkConfigUsesOnlyPrivateStorePaths(t *testing.T) {
	t.Setenv("CONTAINERS_CONF", filepath.Join(t.TempDir(), "missing-containers.conf"))
	root := t.TempDir()
	graphRoot := filepath.Join(root, "graph")
	runRoot := filepath.Join(root, "run")

	got, err := cooprNetworkConfig(graphRoot, runRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got.NetworkConfigDir != filepath.Join(graphRoot, "networks") {
		t.Fatalf("network config dir = %q", got.NetworkConfigDir)
	}
	if got.NetworkRunDir != filepath.Join(runRoot, "networks") {
		t.Fatalf("network run dir = %q", got.NetworkRunDir)
	}
	if got.Config.Network.DefaultNetwork != cooprDefaultNetwork {
		t.Fatalf("default network = %q", got.Config.Network.DefaultNetwork)
	}
	if got.Config.Network.DefaultRootlessNetworkCmd != "pasta" {
		t.Fatalf("rootless network command = %q", got.Config.Network.DefaultRootlessNetworkCmd)
	}
	if got.Config.Network.RootlessPortForwarder != config.RootlessPortForwarderPasta {
		t.Fatalf("rootless port forwarder = %q", got.Config.Network.RootlessPortForwarder)
	}
	if got.NetavarkBinary != "netavark" || got.AardvarkBinary != "aardvark-dns" {
		t.Fatalf("network helpers = %q, %q", got.NetavarkBinary, got.AardvarkBinary)
	}
}

func TestNetworkInterfaceCreationDoesNotResolveHelperBinaries(t *testing.T) {
	t.Setenv(unshare.UsernsEnvName, "done")
	t.Setenv("PATH", "")
	root := t.TempDir()
	initConfig, err := cooprNetworkConfig(filepath.Join(root, "graph"), filepath.Join(root, "run"))
	if err != nil {
		t.Fatal(err)
	}

	network, err := netavark.NewNetworkInterface(initConfig)
	if err != nil {
		t.Fatalf("create network interface without helpers installed: %v", err)
	}
	if got := network.DefaultNetworkName(); got != cooprDefaultNetwork {
		t.Fatalf("default network = %q", got)
	}
}

func TestCooprNetworkConfigHonorsExplicitNativeConfigurationDirectory(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "shared-networks")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := cooprNetworkConfigWithDirectory(filepath.Join(root, "graph"), filepath.Join(root, "run"), configDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.NetworkConfigDir != configDir {
		t.Fatalf("network config dir = %q, want %q", got.NetworkConfigDir, configDir)
	}
}
