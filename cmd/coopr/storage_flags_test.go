package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"coopr/internal/oci"
	"github.com/spf13/cobra"
)

func TestGlobalStorageFlagsSelectOneCanonicalGraph(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(rootDir, "config"))
	root := newRootCommand()
	var captured bool
	probe := &cobra.Command{Use: "storage-probe", Annotations: map[string]string{nativeStorageAnnotation: "true"}, RunE: func(cmd *cobra.Command, _ []string) error {
		store, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		if store.GraphRoot != filepath.Join(rootDir, "custom-native") || store.RunRoot != filepath.Join(rootDir, "runtime") || store.GraphDriverName != "vfs" || !store.TransientStore || store.ImageStore != filepath.Join(rootDir, "image-bytes") {
			t.Fatalf("store=%+v", store)
		}
		captured = true
		return nil
	}}
	root.AddCommand(probe)
	root.SetArgs([]string{"--root", filepath.Join(rootDir, "custom-native"), "--runroot", filepath.Join(rootDir, "runtime"), "--storage-driver", "vfs", "--imagestore", filepath.Join(rootDir, "image-bytes"), "--transient-store", "storage-probe"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !captured {
		t.Fatal("storage selection did not reach command")
	}
}

func TestRootHasNoImageStoreModeFlag(t *testing.T) {
	root := newRootCommand()
	if root.PersistentFlags().Lookup("image-store") != nil {
		t.Fatal("obsolete image store selection flag remains")
	}
}

func TestNetworkHelperIsNotACommandFlag(t *testing.T) {
	for _, args := range [][]string{{"--network-cmd-path=/slirp", "build"}, {"build", "--network-cmd-path=/slirp"}, {"component", "build", "--network-cmd-path=/slirp"}} {
		var stdout, stderr bytes.Buffer
		if status := run(args, &stdout, &stderr); status == 0 || !strings.Contains(stderr.String(), "unknown flag: --network-cmd-path") {
			t.Fatalf("%v: status=%d stderr=%s", args, status, &stderr)
		}
	}
	for _, cmd := range []*cobra.Command{newRootCommand(), newBuildCommand(), newComponentBuildCommand()} {
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := cmd.Help(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "--network-cmd-path") {
			t.Fatalf("%s still advertises the network helper flag", cmd.Name())
		}
	}
}

func TestCommandsDefaultToEffectiveNativeStorage(t *testing.T) {
	rootDir := t.TempDir()
	configHome := filepath.Join(rootDir, "config")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	storageConfig := filepath.Join(rootDir, "storage.conf")
	graph, run := filepath.Join(rootDir, "podman-graph"), filepath.Join(rootDir, "podman-run")
	data := []byte("[storage]\ndriver = \"vfs\"\ngraphroot = \"" + graph + "\"\nrunroot = \"" + run + "\"\n")
	if err := os.WriteFile(storageConfig, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_STORAGE_CONF", storageConfig)
	if err := os.MkdirAll(filepath.Join(configHome, "coopr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "coopr", "config.toml"), []byte("obsolete invalid config"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := newRootCommand()

	probe := &cobra.Command{Use: "storage-config-probe", Annotations: map[string]string{nativeStorageAnnotation: "true"}, RunE: func(cmd *cobra.Command, _ []string) error {
		store, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		if store.GraphRoot != graph || store.RunRoot != run || store.GraphDriverName != "vfs" {
			t.Fatalf("podman store = %+v", store)
		}
		return nil
	}}
	root.AddCommand(probe)
	root.SetArgs([]string{"storage-config-probe"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

}

func TestBuildConsumesLocalImageWithGlobalStorageOverrides(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping native CLI storage builds in short mode")
	}
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	base := filepath.Join(dir, "base.coopr")
	child := filepath.Join(dir, "child.coopr")
	if err := os.WriteFile(base, []byte("from \"scratch\"\ncopy \"payload\" \"/payload\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(child, []byte("from \"local-base\"\nlabel inherited=\"yes\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "payload"), []byte("shared store"), 0600); err != nil {
		t.Fatal(err)
	}
	flags := []string{"--root", filepath.Join(dir, "native-arbitrary-name"), "--runroot", filepath.Join(dir, "run"), "--storage-driver", "vfs"}
	for _, args := range [][]string{{"build", base, "--tag", "local-base"}, {"build", child, "--tag", "local-child", "--pull=never"}, {"image", "inspect", "local-child"}} {
		var stdout, stderr bytes.Buffer
		if code := run(append(append([]string(nil), flags...), args...), &stdout, &stderr); code != 0 {
			t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, &stdout, &stderr)
		}
		if args[0] == "image" && !strings.Contains(stdout.String(), "local-child:latest") {
			t.Fatalf("inspect=%s", &stdout)
		}
	}
}

func TestRootBuildFlagsUseCobraInheritance(t *testing.T) {
	root := newRootCommand()
	for _, path := range [][]string{{"build"}, {"component", "build"}, {"copy"}, {"component", "copy"}} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"cgroup-manager", "module", "cdi-spec-dir", "network-config-dir"} {
			if cmd.LocalNonPersistentFlags().Lookup(name) != nil {
				t.Fatalf("%v shadows root --%s", path, name)
			}
			if cmd.InheritedFlags().Lookup(name) != root.PersistentFlags().Lookup(name) {
				t.Fatalf("%v has independent --%s", path, name)
			}
		}
	}
}

func TestInheritedBuildFlagsPreserveDefaultsAndOrderedOverrides(t *testing.T) {
	for _, path := range [][]string{{"build"}, {"component", "build"}} {
		for _, before := range []bool{false, true} {
			root := newRootCommand()
			config := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", config)
			modules := []string{filepath.Join(config, "default.conf"), filepath.Join(config, "one.conf"), filepath.Join(config, "two.conf")}
			for _, module := range modules {
				if err := os.WriteFile(module, []byte("[engine]\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := root.PersistentFlags().Set("cgroup-manager", "systemd"); err != nil {
				t.Fatal(err)
			}
			if err := root.PersistentFlags().Set("module", modules[0]); err != nil {
				t.Fatal(err)
			}
			cmd, _, err := root.Find(path)
			if err != nil {
				t.Fatal(err)
			}
			cmd.RunE = func(cmd *cobra.Command, _ []string) error {
				flags := buildControlFlags{command: cmd}
				controls, err := flags.controls()
				if err != nil {
					return err
				}
				if controls.CgroupManager != "cgroupfs" || controls.NetworkConfigDir != "/config" || !reflect.DeepEqual(controls.ConfigModules, modules) || !reflect.DeepEqual(controls.CDISpecDirs, []string{"/cdi-one", "/cdi-two"}) {
					t.Fatalf("%v before=%v: controls=%+v", path, before, controls)
				}
				return nil
			}
			globals := []string{"--cgroup-manager=cgroupfs", "--module=" + modules[1], "--module=" + modules[2], "--cdi-spec-dir=/cdi-one", "--cdi-spec-dir=/cdi-two", "--network-config-dir=/config"}
			args := append(append([]string(nil), path...), globals...)
			if before {
				args = append(globals, path...)
			}
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestStandaloneCommandsKeepSharedFlagDefaults(t *testing.T) {
	for _, cmd := range []*cobra.Command{newBuildCommand(), newComponentBuildCommand(), newCopyCommand(oci.Image), newCopyCommand(oci.Component)} {
		policy := cmd.Flags().Lookup("signature-policy")
		isComponentCopy := cmd.Name() == "copy" && strings.Contains(cmd.Long, "stored component")
		if !isComponentCopy && (policy == nil || !policy.Hidden || commandSignaturePolicy(cmd) != "") {
			t.Fatalf("%s lost signature policy default", cmd.Name())
		}
		if isComponentCopy && policy != nil {
			t.Fatal("component copy exposes image signature policy")
		}
		if cmd.Name() == "build" {
			flags := buildControlFlags{command: cmd}
			controls, err := flags.controls()
			if err != nil {
				t.Fatal(err)
			}
			if controls.CgroupManagerSet || len(controls.ConfigModules) != 0 || len(controls.CDISpecDirs) != 0 || controls.NetworkConfigDir != "" || controls.NetworkCmdPath != "" {
				t.Fatalf("changed defaults: %+v", controls)
			}
			if err := cmd.ParseFlags([]string{"--cgroup-manager=invalid"}); err != nil {
				t.Fatal(err)
			}
			if _, err := flags.controls(); err == nil {
				t.Fatal("invalid cgroup manager was accepted")
			}
		}
	}
}
