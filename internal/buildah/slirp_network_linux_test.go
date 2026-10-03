package buildah

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"go.podman.io/buildah/define"
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
