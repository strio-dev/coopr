package main

import (
	"reflect"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// These list flags are declared by Coopr rather than imported as native flags.
func TestBuildOwnedListOptions(t *testing.T) {
	for _, factory := range []func() *cobra.Command{newBuildCommand, newComponentBuildCommand} {
		for _, test := range []struct {
			name, first, second string
			want                []string
		}{
			{"add-host", "one:192.0.2.1,two:192.0.2.2", "three:192.0.2.3", []string{"one:192.0.2.1", "two:192.0.2.2", "three:192.0.2.3"}},
			{"decryption-key", "one,two.pem", "three.pem", []string{"one", "two.pem", "three.pem"}},
			{"platform", "linux/amd64,linux/arm64", "linux/386", []string{"linux/amd64", "linux/arm64", "linux/386"}},
			{"mount", "type=bind,src=one,target=/one", "type=bind,src=two,target=/two", []string{"type=bind,src=one,target=/one", "type=bind,src=two,target=/two"}},
		} {
			cmd := factory()
			if err := cmd.ParseFlags([]string{"--" + test.name, test.first, "--" + test.name, test.second}); err != nil {
				t.Fatal(err)
			}
			got := cmd.Flags().Lookup(test.name).Value.(pflag.SliceValue).GetSlice()
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("%s --%s = %q, want %q", cmd.Short, test.name, got, test.want)
			}
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
		{"dns", controls.DNSServers, []string{"192.0.2.1", "192.0.2.2", "192.0.2.1"}},
		{"dns-search", controls.DNSSearch, []string{"One.TEST", "Two.TEST"}},
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
