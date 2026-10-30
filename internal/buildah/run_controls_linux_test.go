package buildah

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/opencontainers/selinux/go-selinux"
	buildahcli "go.podman.io/buildah/pkg/cli"
	buildahparse "go.podman.io/buildah/pkg/parse"
	commonconfig "go.podman.io/common/pkg/config"
)

func TestRunSecurityOptionsMatchNativeParser(t *testing.T) {
	config, err := commonconfig.Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, explicit := range [][]string{
		nil,
		{"seccomp=unconfined", "no-new-privileges"},
		{"label=user:chosen_u", "label=role:chosen_r", "mask=/proc/acpi:/proc/kcore", "unmask=/audit/missing", "apparmor=example", "seccomp=unconfined"},
	} {
		flags, err := buildahcli.GetFromAndBudFlags(new(buildahcli.FromAndBudResults), new(buildahcli.UserNSResults), new(buildahcli.NameSpaceResults))
		if err != nil {
			t.Fatal(err)
		}
		for _, option := range append(config.SecurityOptions(), explicit...) {
			if err := flags.Set("security-opt", option); err != nil {
				t.Fatal(err)
			}
		}
		native, nativeErr := buildahparse.CommonBuildOptionsFromFlagSet(&flags, flags.Lookup)
		options, err := runSecurityOptions(config, explicit, selinux.CurrentLabel)
		if (nativeErr == nil) != (err == nil) {
			t.Fatalf("security defaults: native %v, Coopr %v", nativeErr, err)
		}
		if err != nil {
			continue
		}
		var controls RunControls
		if err := controls.parseSecurityOptions(options); err != nil {
			t.Fatal(err)
		}
		got := controls.commonBuildOptions(nil)
		if got.NoNewPrivileges != native.NoNewPrivileges || got.SeccompProfilePath != native.SeccompProfilePath || got.ApparmorProfile != native.ApparmorProfile ||
			!reflect.DeepEqual(got.LabelOpts, native.LabelOpts) || !reflect.DeepEqual(got.Masks, native.Masks) || !reflect.DeepEqual(got.Unmasks, native.Unmasks) {
			t.Fatalf("security controls differ from native parser: got %+v, native %+v", got, native)
		}
	}
}

func TestRunSecurityOptionsPreservesLabeledUserPolicy(t *testing.T) {
	config := &commonconfig.Config{}
	config.Containers.EnableLabeling = true
	config.Containers.EnableLabeledUsers = true
	options, err := runSecurityOptions(config, []string{"seccomp=unconfined", "label=user:override_u"}, func() (string, error) {
		return "caller_u:caller_r:caller_t:s0", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"label=user:caller_u", "label=role:caller_r", "seccomp=unconfined", "label=user:override_u"}
	if !reflect.DeepEqual(options, want) {
		t.Fatalf("labeled-user security policy = %v, want %v", options, want)
	}
}

func TestRunSecurityOptionsPropagatesLabelFailure(t *testing.T) {
	config := &commonconfig.Config{}
	config.Containers.EnableLabeledUsers = true
	failure := errors.New("read caller SELinux context")
	if _, err := runSecurityOptions(config, []string{"seccomp=unconfined"}, func() (string, error) {
		return "", failure
	}); !errors.Is(err, failure) {
		t.Fatalf("current-label failure = %v, want %v", err, failure)
	}
	config.Containers.EnableLabeledUsers = false
	if _, err := runSecurityOptions(config, []string{"seccomp=unconfined"}, func() (string, error) {
		t.Fatal("disabled labeled-user policy read the caller context")
		return "", nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultSeccompProfilePreservesNativePathErrors(t *testing.T) {
	root := t.TempDir()
	brokenParent := filepath.Join(root, "not-a-directory")
	fallback := filepath.Join(root, "distribution.json")
	for _, path := range []string{brokenParent, fallback} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if profile, err := defaultSeccompProfile([]string{filepath.Join(brokenParent, "seccomp.json"), fallback}); err == nil {
		t.Fatalf("broken administrator override silently selected %q", profile)
	}
	if profile, err := defaultSeccompProfile([]string{filepath.Join(root, "absent.json"), fallback}); err != nil || profile != fallback {
		t.Fatalf("absent administrator override = %q, %v", profile, err)
	}
	if profile, err := defaultSeccompProfile([]string{root, fallback}); err != nil || profile != root {
		t.Fatalf("existing non-file override was skipped: %q, %v", profile, err)
	}
}

func TestRunControlCgroupManagerHonorsRequestConfiguration(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "containers.conf")
	modulePath := filepath.Join(root, "request.conf")
	if err := os.WriteFile(configPath, []byte("[engine]\ncgroup_manager = \"systemd\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modulePath, []byte("[engine]\ncgroup_manager = \"cgroupfs\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_CONF", configPath)
	t.Setenv("CONTAINERS_CONF_OVERRIDE", "")
	global, err := commonconfig.Default()
	if err != nil {
		t.Fatal(err)
	}
	before := global.Engine.CgroupManager
	for _, controls := range []RunControls{
		{CgroupManager: "cgroupfs", CgroupManagerSet: true},
		{ConfigModules: []string{modulePath}},
	} {
		manager, unavailableSession, err := runControlCgroupManager(controls)
		if err != nil || manager != "cgroupfs" || unavailableSession {
			t.Fatalf("request manager for %+v = %q, unavailable session %t, %v", controls, manager, unavailableSession, err)
		}
	}
	manager, adjusted, err := runControlCgroupManager(RunControls{CgroupManager: "systemd", CgroupManagerSet: true})
	if err != nil || manager != "systemd" || adjusted {
		t.Fatalf("explicit manager was adjusted unlike native execution: %q %t %v", manager, adjusted, err)
	}
	if global.Engine.CgroupManager != before {
		t.Fatal("request parsing changed the process-global container configuration")
	}
}
