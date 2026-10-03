package buildah

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCacheRecordFresh(t *testing.T) {
	now := time.Unix(10_000, 0)
	ttl := time.Hour
	if !cacheRecordFresh(now.Add(-time.Minute), &ttl, now) {
		t.Fatal("recent cache record was rejected")
	}
	if cacheRecordFresh(now.Add(-2*time.Hour), &ttl, now) {
		t.Fatal("expired cache record was accepted")
	}
	zero := time.Duration(0)
	if cacheRecordFresh(now, &zero, now) {
		t.Fatal("zero cache TTL accepted a record")
	}
	if !cacheRecordFresh(time.Time{}, nil, now) {
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

func TestParseCacheSpecCanonicalizesLocalPathsAndRejectsInvalidSpecs(t *testing.T) {
	spec, err := ParseCacheSpec("oci-layout:cache")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs("cache")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Transport != "oci-layout" || spec.Reference != want {
		t.Fatalf("spec = %+v, want oci-layout:%s", spec, want)
	}
	for _, value := range []string{"registry:", "registry:example.org", "unknown:value"} {
		if _, err := ParseCacheSpec(value); err == nil {
			t.Fatalf("ParseCacheSpec(%q) succeeded", value)
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
