package buildah

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coopr/internal/cache"
)

func TestCacheRecordFresh(t *testing.T) {
	now := time.Unix(10_000, 0)
	ttl := time.Hour
	if !cache.RecordFresh(now.Add(-time.Minute), &ttl, now) {
		t.Fatal("recent cache record was rejected")
	}
	if cache.RecordFresh(now.Add(-2*time.Hour), &ttl, now) {
		t.Fatal("expired cache record was accepted")
	}
	zero := time.Duration(0)
	if cache.RecordFresh(now, &zero, now) {
		t.Fatal("zero cache TTL accepted a record")
	}
	if !cache.RecordFresh(time.Time{}, nil, now) {
		t.Fatal("unspecified cache TTL rejected legacy record")
	}
}

func TestZeroCacheTTLDisablesReads(t *testing.T) {
	zero := time.Duration(0)
	options, err := normalizePlanOptions(PlanOptions{CacheTTL: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if !options.NoCache {
		t.Fatal("explicit zero cache TTL did not disable cache reads")
	}
}

func TestExplicitWritableCacheInitializationFailureIsFatal(t *testing.T) {
	root := t.TempDir()
	invalid := filepath.Join(root, "cache-file")
	if err := os.WriteFile(invalid, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	constructors := map[string]func(PlanOptions) error{
		"instruction": func(options PlanOptions) error {
			c, err := newPortableInstructionCache(context.Background(), options)
			if c != nil {
				_ = c.close()
			}
			return err
		},
		"component": func(options PlanOptions) error {
			c, err := newComponentCache(context.Background(), options)
			if c != nil {
				_ = c.close()
			}
			return err
		},
		"package": func(options PlanOptions) error {
			c, err := newPackageResultCache(context.Background(), options)
			if c != nil {
				_ = c.close()
			}
			return err
		},
	}
	for kind, construct := range constructors {
		for _, shorthand := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/shorthand=%t", kind, shorthand), func(t *testing.T) {
				options := PlanOptions{Store: StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph")}}
				if shorthand {
					options.CacheLocalDir = invalid
				} else {
					options.CacheTo = []CacheSpec{{Transport: "oci-layout", Reference: invalid}}
				}
				if err := construct(options); err == nil || !strings.Contains(err.Error(), invalid) {
					t.Fatalf("invalid explicit %s output returned success or lost destination: %v", kind, err)
				}
			})
		}
	}
}

func TestCacheBindingsPreserveIndependentReadAndWritePermissions(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	readOnly := filepath.Join(root, "read")
	writeOnly := filepath.Join(root, "write")
	bindings, err := cacheBindings(PlanOptions{
		CacheLocalDir: shared,
		CacheFrom: []CacheSpec{
			{Transport: "oci-layout", Reference: readOnly},
			{Transport: "registry", Reference: "registry.example/cache"},
		},
		CacheTo: []CacheSpec{
			{Transport: "oci-layout", Reference: writeOnly},
			{Transport: "registry", Reference: "registry.example/cache"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]bool{
		"oci-layout:" + shared:            {true, true},
		"oci-layout:" + readOnly:          {true, false},
		"oci-layout:" + writeOnly:         {false, true},
		"registry:registry.example/cache": {true, true},
	}
	if len(bindings) != len(want) {
		t.Fatalf("bindings = %#v", bindings)
	}
	for _, binding := range bindings {
		key := binding.spec.Transport + ":" + binding.spec.Reference
		permissions, ok := want[key]
		if !ok || binding.read != permissions[0] || binding.write != permissions[1] {
			t.Fatalf("binding %q = read %v write %v", key, binding.read, binding.write)
		}
	}
}

func TestNormalizeCacheSpecCanonicalizesLocalPathsAndRejectsInvalidSpecs(t *testing.T) {
	root := t.TempDir()
	spec, err := normalizeCacheSpec(CacheSpec{Transport: "oci-layout", Reference: root + "/nested/../cache"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "cache")
	if spec.Transport != "oci-layout" || spec.Reference != want {
		t.Fatalf("spec = %+v, want oci-layout:%s", spec, want)
	}
	for _, spec := range []CacheSpec{
		{Transport: "registry"},
		{Transport: "oci-layout"},
		{Transport: "oci-layout", Reference: "relative"},
		{Transport: "unknown", Reference: "value"},
	} {
		if _, err := normalizeCacheSpec(spec); err == nil {
			t.Fatalf("normalizeCacheSpec(%+v) succeeded", spec)
		}
	}
}

func TestCacheArtifactPathsIncludeEveryLocalBindingOnce(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cache")
	paths, err := cacheArtifactPaths(PlanOptions{
		CacheFrom: []CacheSpec{{Transport: "oci-layout", Reference: path}},
		CacheTo:   []CacheSpec{{Transport: "oci-layout", Reference: path}, {Transport: "registry", Reference: "registry.example/cache"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != path {
		t.Fatalf("artifact paths = %#v", paths)
	}
}

func TestCacheBindingsNormalizeRegistryRepositories(t *testing.T) {
	bindings, err := cacheBindings(PlanOptions{
		CacheFrom: []CacheSpec{{Transport: "registry", Reference: "example/cache"}},
		CacheTo:   []CacheSpec{{Transport: "registry", Reference: "docker.io/example/cache"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].spec.Reference != "docker.io/example/cache" || !bindings[0].read || !bindings[0].write {
		t.Fatalf("registry aliases did not share a directional binding: %#v", bindings)
	}
	for _, value := range []string{"localhost:5000/team/cache", "build-cache"} {
		if _, err := normalizeCacheSpec(CacheSpec{Transport: "registry", Reference: value}); err != nil {
			t.Fatalf("valid Buildah cache repository %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{"ghcr.io/team/cache:latest", "ghcr.io/team/cache@sha256:" + strings.Repeat("a", 64)} {
		if _, err := normalizeCacheSpec(CacheSpec{Transport: "registry", Reference: value}); err == nil {
			t.Fatalf("tagged or digested cache repository %q accepted", value)
		}
	}
}
