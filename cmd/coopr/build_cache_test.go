package main

import (
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func TestRemovedBuildKitCacheFlagsAreAbsent(t *testing.T) {
	cmd := newBuildCommand()
	for _, name := range []string{"cache-dir", "cache-repository", "no-state-cache"} {
		if cmd.Flags().Lookup(name) != nil {
			t.Fatalf("BuildKit cache flag --%s is still registered", name)
		}
	}
}

func TestBuildAndComponentExposeDirectionalCacheFlags(t *testing.T) {
	for _, test := range []struct {
		name string
		cmd  *cobra.Command
	}{
		{name: "build", cmd: newBuildCommand()},
		{name: "component build", cmd: newComponentBuildCommand()},
	} {
		for _, flag := range []string{"cache-from", "cache-to"} {
			if test.cmd.Flags().Lookup(flag) == nil {
				t.Errorf("%s missing --%s", test.name, flag)
			}
			values := []string{"oci-layout:first", "oci-layout:second", "registry:example.org/one", "registry:example.org/two"}
			for _, value := range values {
				if err := test.cmd.ParseFlags([]string{"--" + flag, value}); err != nil {
					t.Fatal(err)
				}
			}
			parsed, err := test.cmd.Flags().GetStringArray(flag)
			if err != nil || len(parsed) != len(values) {
				t.Fatalf("%s --%s lost repeated values: %v, %v", test.name, flag, parsed, err)
			}
			if _, err := parseCacheSpecs(parsed); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestParseCacheSpecsSupportsRepeatedTransports(t *testing.T) {
	specs, err := parseCacheSpecs([]string{"oci-layout:cache", "registry:example.org/coopr/cache"})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || specs[0].Transport != "oci-layout" || !filepath.IsAbs(specs[0].Reference) || specs[1].Reference != "example.org/coopr/cache" {
		t.Fatalf("cache specs = %#v", specs)
	}
	for _, invalid := range []string{"registry:", "registry:example.org", "oci-archive:/tmp/cache"} {
		if _, err := parseCacheSpecs([]string{invalid}); err == nil {
			t.Fatalf("invalid cache %q accepted", invalid)
		}
	}
}
