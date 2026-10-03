package main

import (
	"path/filepath"
	"strings"
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

func TestBuildCacheDestinations(t *testing.T) {
	local, repository, err := parseBuildCaches([]string{"registry:example.org/coopr/cache", "oci-layout:cache"})
	if err != nil {
		t.Fatal(err)
	}
	wantLocal, err := filepath.Abs("cache")
	if err != nil {
		t.Fatal(err)
	}
	if local != wantLocal || repository != "example.org/coopr/cache" {
		t.Fatalf("cache destinations = %q, %q", local, repository)
	}
	for _, test := range []struct {
		values []string
		want   string
	}{
		{[]string{"registry:"}, "invalid cache"},
		{[]string{"registry:example.org"}, "invalid cache repository"},
		{[]string{"oci-archive:/tmp/cache"}, "unsupported cache transport"},
		{[]string{"oci-layout:/tmp/a", "oci-layout:/tmp/b"}, "only one oci-layout cache"},
		{[]string{"registry:example.org/a", "registry:example.org/b"}, "only one registry cache"},
	} {
		_, _, err := parseBuildCaches(test.values)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("parseBuildCaches(%q) = %v, want %q", test.values, err, test.want)
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
		for _, flag := range []string{"cache", "cache-from", "cache-to"} {
			if test.cmd.Flags().Lookup(flag) == nil {
				t.Errorf("%s missing --%s", test.name, flag)
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
	if _, err := parseCacheSpecs([]string{"registry:example.org"}); err == nil {
		t.Fatal("invalid cache repository accepted")
	}
}
