package buildah

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	commonnetwork "go.podman.io/common/libnetwork/network"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/unshare"
)

type networkConfigurationStore struct {
	storage.Store
	graph, run string
}

func (s networkConfigurationStore) GraphRoot() string { return s.graph }
func (s networkConfigurationStore) RunRoot() string   { return s.run }

func TestNetworkInterfaceUsesNativeConfiguration(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name                                                                          string
		configured, module, explicitDir, missingHelper, invalidBackend, missingConfig bool
	}{
		{name: "native defaults"},
		{name: "containers.conf", configured: true},
		{name: "module", module: true},
		{name: "explicit directory overrides config", configured: true, explicitDir: true},
		{name: "explicit directory overrides module", module: true, explicitDir: true},
		{name: "missing helper", missingHelper: true},
		{name: "unsupported backend", invalidBackend: true},
		{name: "missing config", missingConfig: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			helpers := filepath.Join(root, "helpers")
			if err := os.Mkdir(helpers, 0700); err != nil {
				t.Fatal(err)
			}
			if !test.missingHelper {
				for _, name := range []string{"netavark", "aardvark-dns"} {
					if err := os.WriteFile(filepath.Join(helpers, name), []byte("#!"+shell+"\nprintf '"+name+" 1.0.0\\n'\n"), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			conf := filepath.Join(root, "containers.conf")
			content := fmt.Sprintf("[engine]\nhelper_binaries_dir=[%q]\n", helpers)
			if test.configured {
				content += fmt.Sprintf("[network]\ndefault_network='configured-bridge'\ndefault_subnet='10.93.0.0/24'\nnetwork_config_dir=%q\n", filepath.Join(root, "configured"))
			}
			if test.invalidBackend {
				content += "[network]\nnetwork_backend='unsupported'\n"
			}
			if !test.missingConfig {
				if err := os.WriteFile(conf, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			controls := RunControls{}
			if test.module {
				module := filepath.Join(root, "module.conf")
				if err := os.WriteFile(module, []byte(fmt.Sprintf("[network]\ndefault_network='module-bridge'\ndefault_subnet='10.94.0.0/24'\nnetwork_config_dir=%q\n", filepath.Join(root, "module-networks"))), 0600); err != nil {
					t.Fatal(err)
				}
				controls.ConfigModules = []string{module}
			}
			if test.explicitDir {
				controls.NetworkConfigDir = filepath.Join(root, "explicit")
			}
			t.Setenv("CONTAINERS_CONF", conf)
			t.Setenv("CONTAINERS_CONF_OVERRIDE", "")
			t.Setenv(unshare.UsernsEnvName, "done")
			t.Setenv("PATH", "")
			store := networkConfigurationStore{graph: filepath.Join(root, "graph"), run: filepath.Join(root, "run")}
			config, nativeErr := loadRunContainerConfig(controls.ConfigModules)
			var nativeName, nativePath string
			var nativeSubnets any
			if nativeErr == nil {
				if controls.NetworkConfigDir != "" {
					config.Network.NetworkConfigDir = controls.NetworkConfigDir
				}
				configDir := config.Network.NetworkConfigDir
				if configDir == "" {
					configDir = filepath.Join(store.GraphRoot(), "networks")
				}
				if err := os.MkdirAll(configDir, 0700); err != nil {
					t.Fatal(err)
				}
				seeded := fmt.Sprintf(`{"name":"fixture-network","id":%q,"driver":"bridge","network_interface":"fixture-bridge"}`, strings.Repeat("a", 64))
				if err := os.WriteFile(filepath.Join(configDir, "fixture-network.json"), []byte(seeded), 0600); err != nil {
					t.Fatal(err)
				}
				_, native, err := commonnetwork.NetworkBackend(store, config, false)
				nativeErr = err
				if err == nil {
					nativeName = native.DefaultNetworkName()
					nativePath = native.NetworkInfo().Path
					network, err := native.NetworkInspect(nativeName)
					if err != nil {
						t.Fatal(err)
					}
					nativeSubnets = network.Subnets
				}
			}
			coopr, err := newNetworkInterfaceWithControls(store, controls)
			if (err == nil) != (nativeErr == nil) {
				t.Fatalf("native error=%v Coopr error=%v", nativeErr, err)
			}
			if nativeErr != nil {
				if err.Error() != nativeErr.Error() {
					t.Fatalf("native error=%v Coopr error=%v", nativeErr, err)
				}
				return
			}
			actual, err := coopr.NetworkInspect(coopr.DefaultNetworkName())
			if err != nil {
				t.Fatal(err)
			}
			if coopr.DefaultNetworkName() != nativeName || coopr.NetworkInfo().Path != nativePath || !reflect.DeepEqual(actual.Subnets, nativeSubnets) {
				t.Fatalf("Coopr name=%q helper=%q subnets=%v; native name=%q helper=%q subnets=%v", coopr.DefaultNetworkName(), coopr.NetworkInfo().Path, actual.Subnets, nativeName, nativePath, nativeSubnets)
			}
			if _, err := coopr.NetworkInspect("fixture-network"); err != nil {
				t.Fatalf("effective network configuration directory not consumed: %v", err)
			}

		})
	}
}
