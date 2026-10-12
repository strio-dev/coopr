package main

import (
	"reflect"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	buildahcli "go.podman.io/buildah/pkg/cli"
)

func TestBuildListOptionsMatchBuildah(t *testing.T) {
	tests := []struct{ name, first, second string }{
		{"dns", "192.0.2.1,192.0.2.2", "2001:db8::1"},
		{"dns-search", "one.test,two.test", "three.test"},
		{"dns-option", "ndots:2,timeout:3", "attempts:2"},
		{"ulimit", "nofile=1024:2048,nproc=512:1024", "core=0:0"},
		{"add-host", "one.test:192.0.2.1,two.test:192.0.2.2", "three.test:192.0.2.3"},
		{"decryption-key", "one.pem,two.pem", "three.pem"},
		{"unsetenv", "ONE,TWO", "THREE"},
		{"unsetlabel", "one,two", "three"},
		{"unsetannotation", "one,two", "three"},
		{"platform", "linux/amd64,linux/arm64", "linux/386"},
		{"env", "VALUES=one,two", "OTHER=three"},
		{"label", "values=one,two", "other=three"},
		{"annotation", "values=one,two", "other=three"},
		{"build-arg", "VALUES=one,two", "OTHER=three"},
		{"build-arg-file", "one,two.env", "three.env"},
		{"build-context", "source=one,two", "other=three"},
		{"secret", "id=one,src=one.txt", "id=two,src=two.txt"},
		{"ssh", "default=one,two", "other=three"},
		{"cache-from", "one,two", "three"},
		{"cache-to", "one,two", "three"},
		{"mount", "type=bind,src=one,target=/one", "type=bind,src=two,target=/two"},
		{"volume", "/one:/one:ro,z", "/two:/two:ro"},
		{"device", "/dev/one:/dev/one:r,w", "/dev/two"},
		{"security-opt", "label=level:s0:c1,c2", "no-new-privileges"},
		{"hooks-dir", "one,two", "three"},
		{"layer-label", "values=one,two", "other=three"},
		{"os-feature", "one,two", "three"},
		{"tag", "one,two", "three"},
	}
	for commandIndex, factory := range []func() *cobra.Command{newBuildCommand, newComponentBuildCommand} {
		for _, test := range tests {
			t.Run(factory().Use+"/"+test.name, func(t *testing.T) {
				var bud buildahcli.BudResults
				var from buildahcli.FromAndBudResults
				var userns buildahcli.UserNSResults
				var namespaces buildahcli.NameSpaceResults
				upstream := buildahcli.GetBudFlags(&bud)
				shared, err := buildahcli.GetFromAndBudFlags(&from, &userns, &namespaces)
				if err != nil {
					t.Fatal(err)
				}
				upstream.AddFlagSet(&shared)
				cmd := factory()
				if commandIndex == 1 {
					switch test.name {
					case "env", "label", "annotation", "layer-label", "os-feature", "unsetenv", "unsetlabel", "unsetannotation":
						t.Skip("image configuration controls apply only to image builds")
					}
				}
				if cmd.Flags().Lookup(test.name) == nil {
					t.Fatalf("missing --%s", test.name)
				}
				args := []string{"--" + test.name, test.first, "--" + test.name, test.second}
				if err := upstream.Parse(args); err != nil {
					t.Fatal(err)
				}
				if err := cmd.ParseFlags(args); err != nil {
					t.Fatal(err)
				}
				want := upstream.Lookup(test.name).Value.(pflag.SliceValue).GetSlice()
				got := cmd.Flags().Lookup(test.name).Value.(pflag.SliceValue).GetSlice()
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("--%s = %q, Buildah = %q", test.name, got, want)
				}
			})
		}
	}
}

func TestImageUnsetListOptionsReachControls(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var flags imageControlFlags
	flags.addTo(cmd)
	if err := cmd.ParseFlags([]string{"--unsetenv=ONE,TWO", "--unsetenv=THREE", "--unsetlabel=one,two", "--unsetannotation=first,second"}); err != nil {
		t.Fatal(err)
	}
	controls, err := flags.controls(cmd)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		got, want []string
	}{
		{"environment", controls.UnsetEnv, []string{"ONE", "TWO", "THREE"}},
		{"labels", controls.UnsetLabels, []string{"one", "two"}},
		{"annotations", controls.UnsetAnnotations, []string{"first", "second"}},
	} {
		if !reflect.DeepEqual(test.got, test.want) {
			t.Errorf("unset %s = %q, want %q", test.name, test.got, test.want)
		}
	}
}

func TestBuildControlListValuesReachValidation(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var flags buildControlFlags
	flags.addTo(cmd)
	if err := cmd.ParseFlags([]string{"--dns=192.0.2.1,192.0.2.2", "--dns=192.0.2.1", "--dns-search=One.TEST,Two.TEST", "--dns-option=ndots:2,timeout:3", "--ulimit=nofile=1024:2048,nproc=512:1024"}); err != nil {
		t.Fatal(err)
	}
	controls, err := flags.controls()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		got, want []string
	}{
		{"dns", controls.DNSServers, []string{"192.0.2.1", "192.0.2.2"}},
		{"dns-search", controls.DNSSearch, []string{"one.test", "two.test"}},
		{"dns-option", controls.DNSOptions, []string{"ndots:2", "timeout:3"}},
		{"ulimit", controls.Ulimits, []string{"nofile=1024:2048", "nproc=512:1024"}},
	} {
		if !reflect.DeepEqual(test.got, test.want) {
			t.Errorf("--%s controls = %q, want %q", test.name, test.got, test.want)
		}
	}
}

// Podman v5.8.6 image pull/push and manifest create/add/annotate use
// StringArray for keys/annotations; Buildah's standalone commands differ.
func TestManagementListOptionsMatchPodman(t *testing.T) {
	for _, test := range []struct {
		cmd                 *cobra.Command
		name, first, second string
		want                []string
	}{
		{newImagePullCommand(), "decryption-key", "one,two.pem", "three.pem", []string{"one,two.pem", "three.pem"}},
		{newImagePushCommand(), "encryption-key", "jwe:one,two.pem", "jwe:three.pem", []string{"jwe:one,two.pem", "jwe:three.pem"}},
		{newManifestCreateCommand(), "annotation", "one=two,three", "four=five", []string{"one=two,three", "four=five"}},
		{newManifestAddCommand(), "annotation", "one=two,three", "four=five", []string{"one=two,three", "four=five"}},
		{newManifestAnnotateCommand(), "annotation", "one=two,three", "four=five", []string{"one=two,three", "four=five"}},
		{newManifestAnnotateCommand(), "features", "one,two", "three", []string{"one", "two", "three"}},
		{newManifestAnnotateCommand(), "os-features", "one,two", "three", []string{"one", "two", "three"}},
		{newManifestAddCommand(), "features", "one,two", "three", []string{"one", "two", "three"}},
		{newManifestPushCommand(), "add-compression", "gzip,zstd", "zstd:chunked", []string{"gzip", "zstd", "zstd:chunked"}},
	} {
		t.Run(test.cmd.Use+"/"+test.name, func(t *testing.T) {
			if err := test.cmd.ParseFlags([]string{"--" + test.name, test.first, "--" + test.name, test.second}); err != nil {
				t.Fatal(err)
			}
			got := test.cmd.Flags().Lookup(test.name).Value.(pflag.SliceValue).GetSlice()
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("--%s = %q, want %q", test.name, got, test.want)
			}
		})
	}
}
