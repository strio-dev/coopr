package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/spf13/cobra"
)

func TestBuildFlagContracts(t *testing.T) {
	t.Setenv("BUILDAH_LAYERS", "")
	t.Setenv("SOURCE_DATE_EPOCH", "")
	for _, command := range []*cobra.Command{newBuildCommand(), newComponentBuildCommand()} {
		for _, test := range []struct{ name, kind, value, shorthand string }{
			{"file", "stringArray", "[]", "f"}, {"network", "string", "default", ""},
			{"force-rm", "bool", "true", ""}, {"layers", "bool", "true", ""},
			{"source-date-epoch", "string", "", ""}, {"jobs", "int", "1", "j"},
		} {
			flag := command.Flags().Lookup(test.name)
			if flag == nil || flag.Value.Type() != test.kind || flag.DefValue != test.value || flag.Shorthand != test.shorthand {
				t.Errorf("%s --%s contract = %+v", command.CommandPath(), test.name, flag)
			}
		}
	}
}

func TestSelectedNativeFlagsAndCompletions(t *testing.T) {
	for _, command := range []*cobra.Command{newBuildCommand(), newComponentBuildCommand()} {
		for _, name := range []string{"cap-add", "userns", "build-arg", "cache-ttl", "tag"} {
			if _, ok := command.GetFlagCompletionFunc(name); !ok {
				t.Errorf("%s --%s missing native completion", command.CommandPath(), name)
			}
		}
		for _, name := range []string{"cpp-flag", "cni-plugin-path", "compress", "disable-content-trust"} {
			if command.Flags().Lookup(name) != nil {
				t.Errorf("%s exposes unsupported --%s", command.CommandPath(), name)
			}
		}
		if err := command.ParseFlags([]string{"-j", "3", "--source-date-epoch", "4"}); err != nil {
			t.Fatal(err)
		}
		if n, _ := command.Flags().GetInt("jobs"); n != 3 {
			t.Fatalf("jobs = %d", n)
		}
	}
}

func TestBuildFormatUsesNativeEnvironmentDefault(t *testing.T) {
	t.Setenv("BUILDAH_FORMAT", "docker")
	if flag := newBuildCommand().Flags().Lookup("format"); flag.DefValue != "docker" {
		t.Fatalf("format = %s", flag.DefValue)
	}
}

func TestNativeRunFlagsDeferConfigurationDefaultsAndRespectOverrides(t *testing.T) {
	root := t.TempDir()
	configured, moduleDir, explicit := filepath.Join(root, "configured"), filepath.Join(root, "module"), filepath.Join(root, "explicit")
	for _, dir := range []string{configured, moduleDir, explicit} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(root, "containers.conf")
	module := filepath.Join(root, "module.conf")
	write := func(path, dir, device string) {
		t.Helper()
		body := fmt.Sprintf("[containers]\nvolumes = [%q]\ndevices = [%q]\n[engine]\nhooks_dir = [%q]\n", dir+":/configured:ro", device, dir)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(config, configured, "/dev/null:/dev/config:r")
	write(module, moduleDir, "/dev/null:/dev/module:r")
	t.Setenv("CONTAINERS_CONF", config)
	for _, test := range []struct {
		name        string
		args        []string
		dir, device string
	}{
		{"configuration", nil, configured, "/dev/null:/dev/config:r"},
		{"module", []string{"--module=" + module}, moduleDir, "/dev/null:/dev/module:r"},
		{"authored options", []string{"--module=" + module, "--volume=" + explicit + ":/configured:ro", "--device=/dev/null:/dev/authored:r", "--hooks-dir=" + explicit}, explicit, "/dev/null:/dev/authored:r"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := &cobra.Command{Use: "test"}
			addGlobalRunFlags(cmd.Flags())
			var flags buildControlFlags
			flags.addTo(cmd)
			if flags.Volumes != nil || flags.Devices != nil || flags.OCIHooksDir != nil {
				t.Fatal("absent native options became explicit empty slices")
			}
			if err := cmd.ParseFlags(test.args); err != nil {
				t.Fatal(err)
			}
			controls, err := flags.controls()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(controls.Volumes, []string{test.dir + ":/configured:ro"}) || !reflect.DeepEqual(controls.Devices, []string{test.device}) || !reflect.DeepEqual(controls.HooksDirs, []string{test.dir}) {
				t.Fatalf("effective controls = %+v", controls)
			}
		})
	}
}
