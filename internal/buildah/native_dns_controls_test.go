package buildah

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	buildahcli "go.podman.io/buildah/pkg/cli"
	buildahparse "go.podman.io/buildah/pkg/parse"
	"go.podman.io/common/libnetwork/resolvconf"
)

func TestDNSControlsMatchNativeParser(t *testing.T) {
	for _, input := range []RunControlInput{
		{}, {DNSSearch: []string{"_service.example"}},
		{DNSServers: []string{"none"}}, {DNSServers: []string{"none"}, DNSSearch: []string{}},
		{DNSServers: []string{"none"}, DNSSearch: []string{"example.com"}},
		{DNSServers: []string{"NONE"}, DNSOptions: []string{"rotate"}},
		{DNSServers: []string{"none", "192.0.2.1"}},
		{DNSServers: []string{"2001:0db8::1", "2001:db8::1"}, DNSSearch: []string{"Example.TEST", "example.test"}, DNSOptions: []string{"rotate"}},
		{DNSServers: []string{"not-an-ip"}}, {DNSSearch: []string{"bad domain"}},
		{DNSOptions: []string{"ndots:2,timeout:1"}},
	} {
		t.Run(strings.Join(append(append(append([]string{}, input.DNSServers...), input.DNSSearch...), input.DNSOptions...), "/"), func(t *testing.T) {
			flags, err := buildahcli.GetFromAndBudFlags(new(buildahcli.FromAndBudResults), new(buildahcli.UserNSResults), new(buildahcli.NameSpaceResults))
			if err != nil {
				t.Fatal(err)
			}
			for name, values := range map[string][]string{"dns": input.DNSServers, "dns-search": input.DNSSearch, "dns-option": input.DNSOptions} {
				if values != nil {
					flag := flags.Lookup(name)
					if err := flag.Value.(pflag.SliceValue).Replace(values); err != nil {
						t.Fatal(err)
					}
					flag.Changed = true
				}
			}
			native, nativeErr := buildahparse.CommonBuildOptionsFromFlagSet(&flags, flags.Lookup)
			controls, err := ParseRunControls(input)
			if (nativeErr == nil) != (err == nil) {
				t.Fatalf("native error=%v Coopr error=%v", nativeErr, err)
			}
			if nativeErr != nil {
				if nativeErr.Error() != err.Error() {
					t.Fatalf("native error=%v Coopr error=%v", nativeErr, err)
				}
				return
			}
			common := controls.commonBuildOptions(nil)
			if !slices.Equal(common.DNSServers, native.DNSServers) || !slices.Equal(common.DNSSearch, native.DNSSearch) || !slices.Equal(common.DNSOptions, native.DNSOptions) {
				t.Fatalf("Coopr DNS=%v/%v/%v native=%v/%v/%v", common.DNSServers, common.DNSSearch, common.DNSOptions, native.DNSServers, native.DNSSearch, native.DNSOptions)
			}
			// Compare actual native resolver files, including accepted underscore and
			// noncanonical spellings. No container/network namespace is configured.
			var files []string
			for _, values := range [][3][]string{{common.DNSServers, common.DNSSearch, common.DNSOptions}, {native.DNSServers, native.DNSSearch, native.DNSOptions}} {
				path := filepath.Join(t.TempDir(), "resolv.conf")
				err := resolvconf.New(&resolvconf.Params{Path: path, Nameservers: values[0], Searches: values[1], Options: values[2]})
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				files = append(files, string(data))
			}
			if files[0] != files[1] {
				t.Fatalf("resolver Coopr=%q native=%q", files[0], files[1])
			}
		})
	}
}

func TestDNSConfiguredDefaultsStayNativeAndInvalidateCache(t *testing.T) {
	root := t.TempDir()
	conf := filepath.Join(root, "containers.conf")
	t.Setenv("CONTAINERS_CONF", conf)
	t.Setenv("CONTAINERS_CONF_OVERRIDE", "")
	input := RunControlInput{DNSServers: []string{"192.0.2.2"}, DNSSearch: []string{"_service.example"}, DNSOptions: []string{"ndots:2"}}
	var identities []string
	for _, server := range []string{"192.0.2.1", "192.0.2.3"} {
		if err := os.WriteFile(conf, []byte("[containers]\ndns_servers=['"+server+"']\ndns_searches=['configured.example']\ndns_options=['rotate']\n"), 0600); err != nil {
			t.Fatal(err)
		}
		config, err := loadRunContainerConfig(nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, explicit := range []bool{false, true} {
			values := RunControlInput{}
			if explicit {
				values = input
			}
			controls, err := ParseRunControls(values)
			if err != nil {
				t.Fatal(err)
			}
			common := controls.commonBuildOptions(nil)
			if !slices.Equal(common.DNSServers, values.DNSServers) || !slices.Equal(common.DNSSearch, values.DNSSearch) || !slices.Equal(common.DNSOptions, values.DNSOptions) {
				t.Fatalf("configured DNS promoted to explicit inputs: %+v", common)
			}
			identity, err := cacheRunControlsDigest(controls)
			if err != nil {
				t.Fatal(err)
			}
			identities = append(identities, identity.String())
			path := filepath.Join(t.TempDir(), "resolv.conf")
			if err := resolvconf.New(&resolvconf.Params{
				Path:        path,
				Nameservers: append(slices.Clone(config.Containers.DNSServers.Get()), common.DNSServers...),
				Searches:    append(slices.Clone(config.Containers.DNSSearches.Get()), common.DNSSearch...),
				Options:     append(slices.Clone(config.Containers.DNSOptions.Get()), common.DNSOptions...),
			}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), "nameserver "+server+"\n") != 1 || strings.Count(string(data), "configured.example") != 1 {
				t.Fatalf("duplicated or missing configured DNS in native resolver: %q", data)
			}
			if explicit && (!strings.Contains(string(data), "search configured.example _service.example\n") || !strings.Contains(string(data), "nameserver "+server+"\nnameserver 192.0.2.2\n") || !strings.Contains(string(data), "options rotate ndots:2\n")) {
				t.Fatalf("native configured-before-explicit order changed: %q", data)
			}
		}
	}
	if identities[0] == identities[2] || identities[1] == identities[3] {
		t.Fatal("changed configured DNS did not invalidate omitted and explicit cache identities")
	}
}
