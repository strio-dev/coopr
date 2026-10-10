package main

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
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
			pathKey := "src"
			if flag == "cache-to" {
				pathKey = "dest"
			}
			values := []string{"type=local," + pathKey + "=first", "type=local," + pathKey + "=second", "example.org/one", "example.org/two"}
			for _, value := range values {
				if err := test.cmd.ParseFlags([]string{"--" + flag, value}); err != nil {
					t.Fatal(err)
				}
			}
			parsed, err := test.cmd.Flags().GetStringArray(flag)
			if err != nil || len(parsed) != len(values) {
				t.Fatalf("%s --%s lost repeated values: %v, %v", test.name, flag, parsed, err)
			}
			if _, err := parseCacheSpecs(parsed, pathKey); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestParseCacheSpecsUsesBuildahRepositoryNames(t *testing.T) {
	for _, test := range []struct{ value, want string }{
		{"ghcr.io/example/cache", "ghcr.io/example/cache"},
		{"localhost:5000/team/cache", "localhost:5000/team/cache"},
		{"example/cache", "docker.io/example/cache"},
		{"build-cache", "docker.io/library/build-cache"},
	} {
		t.Run(test.value, func(t *testing.T) {
			specs, err := parseCacheSpecs([]string{test.value}, "src")
			if err != nil {
				t.Fatal(err)
			}
			if len(specs) != 1 || specs[0].Transport != "registry" || specs[0].Reference != test.want {
				t.Fatalf("cache specs = %#v, want registry repository %q", specs, test.want)
			}
		})
	}
}

func TestParseCacheSpecsPreservesQuotedLocalPaths(t *testing.T) {
	for _, key := range []string{"src", "dest"} {
		for _, test := range []struct{ field, path string }{
			{key + "=cache", "cache"},
			{`"` + key + `=cache,with,commas"`, "cache,with,commas"},
			{key + "=cache=with=equals", "cache=with=equals"},
			{key + "=cache with spaces", "cache with spaces"},
			{strings.ToUpper(key) + "=first," + key + "=last", "last"},
		} {
			t.Run(key+"/"+test.path, func(t *testing.T) {
				specs, err := parseCacheSpecs([]string{"type=local," + test.field, "example.org/cache"}, key)
				if err != nil {
					t.Fatal(err)
				}
				want, err := filepath.Abs(test.path)
				if err != nil {
					t.Fatal(err)
				}
				if len(specs) != 2 || specs[0].Transport != "oci-layout" || specs[0].Reference != want || specs[1].Reference != "example.org/cache" {
					t.Fatalf("cache specs = %#v, want local path %q and registry", specs, want)
				}
			})
		}
	}
}

func TestParseCacheSpecsRejectsInvalidLocalOptions(t *testing.T) {
	for _, key := range []string{"src", "dest"} {
		wrongKey := "dest"
		if key == "dest" {
			wrongKey = "src"
		}
		for _, value := range []string{
			"type=local", "type=local," + key + "=", key + "=cache", "type=registry," + key + "=cache",
			"type=local," + wrongKey + "=cache", "type=local," + key + "=cache,mode=max",
			"type=local," + key + "=cache,invalid", "type=local,\"" + key + "=unclosed",
			"type=local," + key + "=cache\nignored=value", "type=local," + key + "=cache,",
		} {
			t.Run(key+"/"+value, func(t *testing.T) {
				if _, err := parseCacheSpecs([]string{value}, key); err == nil {
					t.Fatalf("invalid local cache %q accepted", value)
				}
			})
		}
	}
}

func TestBuildCacheRejectsInvalidSpecsBeforeExecution(t *testing.T) {
	for _, newCommand := range []func() *cobra.Command{newBuildCommand, newComponentBuildCommand} {
		for _, flag := range []string{"cache-from", "cache-to"} {
			cmd := newCommand()
			cmd.SetArgs([]string{definitionFile(t, "invalid definition that must not execute"), "--" + flag, "example.org/cache:latest"})
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), flag+": repository must contain neither a tag nor digest") {
				t.Fatalf("%s --%s returned %v, want cache validation before execution", cmd.Name(), flag, err)
			}
		}
	}
}

func TestBuildCacheRepositoryCLIReusesResultsAcrossStores(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Buildah cache tests in short mode")
	}
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	repository := strings.TrimPrefix(server.URL, "http://") + "/coopr/cache"
	file := definitionFile(t, "from \"scratch\"\ncopy \"payload\" \"/payload\"\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(file), "payload"), []byte("portable CLI cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"cold", "warm"} {
		root := t.TempDir()
		output := filepath.Join(root, "output")
		args := []string{"--root", filepath.Join(root, "graph"), "--runroot", filepath.Join(root, "run"), "--storage-driver=vfs", "build", file, "--tls-verify=false", "--output", output}
		if phase == "cold" {
			args = append(args, "--cache-to", repository)
		} else {
			args = append(args, "--cache-from", repository)
		}
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%s registry cache build failed: %s", phase, stderr.String())
		}
		if phase == "warm" && !strings.Contains(stderr.String(), "--> Using cache") {
			t.Fatalf("fresh store did not restore registry cache: %s", stderr.String())
		}
		if data, err := os.ReadFile(filepath.Join(output, "payload")); err != nil || string(data) != "portable CLI cache\n" {
			t.Fatalf("%s cache build output = %q, %v", phase, data, err)
		}
	}
}

func TestComponentBuildUsesRegistryCacheWithoutImageInputs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Buildah cache tests in short mode")
	}
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	repository := strings.TrimPrefix(server.URL, "http://") + "/coopr/package-cache"
	file := definitionFile(t, "package as=\"payload\"\ncopy \"payload\" \"/payload\"\nextend\ncopy \"/payload\" \"/installed\" from=\"payload\"\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(file), "payload"), []byte("component registry cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"cold", "warm"} {
		root := t.TempDir()
		archive := filepath.Join(root, "component.oci.tar")
		args := []string{"--root", filepath.Join(root, "graph"), "--runroot", filepath.Join(root, "run"), "--storage-driver=vfs", "component", "build", file, "--tls-verify=false", "--tag", "oci-archive:" + archive}
		if phase == "cold" {
			args = append(args, "--cache-to", repository)
		} else {
			args = append(args, "--cache-from", repository)
		}
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%s component registry cache build failed: %s", phase, stderr.String())
		}
		if info, err := os.Stat(archive); err != nil || info.Size() == 0 {
			t.Fatalf("%s component cache build produced no archive: %v", phase, err)
		}
	}
}

func TestParseCacheSpecsRejectsInvalidRepositoriesAndOldPrefixes(t *testing.T) {
	for _, value := range []string{
		"", "registry:", "registry:example.org/cache", "oci-layout:cache", "oci-layout:/tmp/cache", "oci-archive:/tmp/cache",
		"ghcr.io/example/cache:latest", "ghcr.io/example/cache@sha256:" + strings.Repeat("a", 64),
		"https://ghcr.io/example/cache", "./cache", "/tmp/cache",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseCacheSpecs([]string{value}, "src"); err == nil {
				t.Fatalf("invalid cache %q accepted", value)
			}
		})
	}
}
