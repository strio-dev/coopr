package buildah

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/buildcontext"
	"coopr/internal/cache"
	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

type fakePackageStore struct {
	lookup func(cache.PackageKey) (*cache.PackageRecord, string, error)
	put    func(cache.PackageKey, cache.PackageRecord, string) error
}

func (f fakePackageStore) LookupPackage(_ context.Context, key cache.PackageKey) (*cache.PackageRecord, string, error) {
	return f.lookup(key)
}

func (f fakePackageStore) PutPackage(_ context.Context, key cache.PackageKey, record cache.PackageRecord, path string) (v1.Descriptor, error) {
	if f.put == nil {
		return v1.Descriptor{}, nil
	}
	return v1.Descriptor{}, f.put(key, record, path)
}

func packageWarningOutput(t *testing.T, action func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = write
	action()
	os.Stderr = previous
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func preflightPackageKey(output string) cache.PackageKey {
	return cache.PackageKey{
		Plan: digest.FromString("plan-" + output), Output: output,
		Platform: v1.Platform{OS: "linux", Architecture: "amd64"},
		Executor: "executor", Frontend: "frontend", Lowering: "lowering",
	}
}

func TestPackageCacheLookupWarnsAndFallsBackOnStoreErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		lookup func(cache.PackageKey) (*cache.PackageRecord, string, error)
		want   string
	}{
		{name: "authorization", lookup: func(cache.PackageKey) (*cache.PackageRecord, string, error) {
			return nil, "", errors.New("registry authorization denied")
		}, want: "registry authorization denied"},
		{name: "malformed", lookup: func(cache.PackageKey) (*cache.PackageRecord, string, error) {
			return nil, "/tmp/untrusted", nil
		}, want: "incomplete package record"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := &packageResultCache{readStores: []cache.PackageStore{fakePackageStore{lookup: test.lookup}}}
			var complete bool
			warning := packageWarningOutput(t, func() {
				_, complete, _ = result.lookupAll(context.Background(), map[string]cache.PackageKey{"stage": preflightPackageKey("output")})
			})
			if complete {
				t.Fatal("broken optional cache was treated as a complete hit")
			}
			if !strings.Contains(warning, test.want) {
				t.Fatalf("warning = %q, want %q", warning, test.want)
			}
		})
	}
}

func TestPackageCachePartialMultiOutputHitIsDiscarded(t *testing.T) {
	root := t.TempDir()
	hitPath := filepath.Join(root, "hit.tar")
	if err := os.WriteFile(hitPath, []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := fakePackageStore{lookup: func(key cache.PackageKey) (*cache.PackageRecord, string, error) {
		if key.Output == "first" {
			return &cache.PackageRecord{}, hitPath, nil
		}
		return nil, "", cache.ErrMiss
	}}
	result := &packageResultCache{readStores: []cache.PackageStore{store}}
	_, complete, err := result.lookupAll(context.Background(), map[string]cache.PackageKey{
		"a": preflightPackageKey("first"), "b": preflightPackageKey("second"),
	})
	if err != nil || complete {
		t.Fatalf("partial lookup complete=%v err=%v", complete, err)
	}
	if _, err := os.Stat(hitPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial hit staging file remains: %v", err)
	}
}

func TestPackageCacheExportReturnsAllFailures(t *testing.T) {
	result := &packageResultCache{writeStores: []cache.PackageStore{
		fakePackageStore{put: func(cache.PackageKey, cache.PackageRecord, string) error { return errors.New("first export failed") }},
		fakePackageStore{put: func(cache.PackageKey, cache.PackageRecord, string) error { return errors.New("second export failed") }},
	}}
	key := preflightPackageKey("bundle")
	outputs := map[string]packageOutput{"stage": {key: "bundle", path: "/unused"}}
	packages := map[string]oci.Package{"bundle": {Stage: "bundle"}}
	err := result.store(context.Background(), map[string]cache.PackageKey{"stage": key}, outputs, packages)
	if err == nil || !strings.Contains(err.Error(), "first export failed") || !strings.Contains(err.Error(), "second export failed") {
		t.Fatalf("package cache export error = %v", err)
	}
}

func TestPackageCacheLocalInitializationWarnsAndFallsBack(t *testing.T) {
	root := t.TempDir()
	notDirectory := filepath.Join(root, "cache")
	if err := os.WriteFile(notDirectory, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := PlanOptions{
		Store:     StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph")},
		CacheFrom: []CacheSpec{{Transport: "oci-layout", Reference: notDirectory}},
	}
	var result *packageResultCache
	warning := packageWarningOutput(t, func() {
		var err error
		result, err = newPackageResultCache(context.Background(), options)
		if err != nil {
			t.Fatal(err)
		}
	})
	if result != nil {
		t.Fatal("unavailable optional cache returned a usable store")
	}
	if !strings.Contains(warning, "open local package cache") {
		t.Fatalf("warning = %q", warning)
	}
}

func TestPackageCacheKeyChangesWithImmutableExternalBase(t *testing.T) {
	const reference = "registry.example/coopr/base:latest"
	plan := testPublicationPlan(t, `
from "`+reference+`" as="producer"
package as="bundle"
copy "/payload" "/payload" from="producer"
extend
copy "/payload" "/payload" from="bundle"
`)
	root := t.TempDir()
	stages, outputs, err := validatePublicationGraph(plan, map[string]string{"bundle": filepath.Join(root, "bundle.tar")})
	if err != nil {
		t.Fatal(err)
	}
	options := PlanOptions{
		Store:      StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
		ContextDir: root, Isolation: "rootless", Runtime: "crun",
	}
	key := ResolvedBaseKey{Reference: reference, Platform: plan.Platform}
	digests := make([]digest.Digest, 0, 2)
	for _, identity := range []string{"one", "two"} {
		options.ResolvedBases = map[ResolvedBaseKey]ResolvedImageSource{key: {
			Selected:   v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString(identity), Size: 1},
			ConfigData: []byte(`{"config":{}}`),
		}}
		keys, eligible, err := packageCacheKeys(context.Background(), options, plan, stages, outputs, nil)
		if err != nil || !eligible {
			t.Fatalf("package keys eligible=%v err=%v", eligible, err)
		}
		value, err := keys[plan.Outputs[0]].Digest()
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, value)
	}
	if digests[0] == digests[1] {
		t.Fatal("immutable external-base change did not invalidate package cache key")
	}
}

func TestPackageCacheDoesNotSkipUnplannedOnBuildChecks(t *testing.T) {
	const reference = "fixture.local/coopr/base:latest"
	for _, test := range []struct {
		name     string
		config   string
		planned  bool
		eligible bool
	}{
		{name: "no inherited triggers", config: `{"config":{}}`, eligible: true},
		{name: "unplanned network request", config: `{"config":{"OnBuild":["RUN --network=none true"]}}`},
		{name: "unavailable base metadata"},
		{name: "planned inherited request", config: `{"config":{"OnBuild":["RUN --network=none true"]}}`, planned: true, eligible: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := testPublicationPlan(t, "from \""+reference+"\" as=\"producer\"\npackage as=\"bundle\"\ncopy \"/payload\" \"/payload\" from=\"producer\"\nextend\ncopy \"/payload\" \"/payload\" from=\"bundle\"\n")
			root := t.TempDir()
			stages, outputs, err := validatePublicationGraph(plan, map[string]string{"bundle": filepath.Join(root, "bundle.tar")})
			if err != nil {
				t.Fatal(err)
			}
			stages[0].InheritedOnBuildPlanned = test.planned
			options := PlanOptions{
				Store: StoreOptions{GraphDriverName: "vfs"}, Isolation: "rootless", Runtime: "crun",
				ResolvedBases: map[ResolvedBaseKey]ResolvedImageSource{{Reference: reference, Platform: plan.Platform}: {
					Selected: v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString("base"), Size: 1}, ConfigData: []byte(test.config),
				}},
			}
			keys, eligible, err := packageCacheKeys(context.Background(), options, plan, stages, outputs, nil)
			if err != nil || eligible != test.eligible || (len(keys) != 0) != test.eligible {
				t.Fatalf("package cache eligible=%v keys=%v error=%v", eligible, keys, err)
			}
		})
	}
}

func TestPackageCacheKeysExcludeGlobalDeviceRun(t *testing.T) {
	for _, instruction := range []string{`run "true"`, `env proof="metadata"`} {
		plan := testPublicationPlan(t, "package as=\"bundle\"\n"+instruction+"\nextend\ncopy \"/payload\" \"/payload\" from=\"bundle\"\n")
		root := t.TempDir()
		stages, outputs, err := validatePublicationGraph(plan, map[string]string{"bundle": filepath.Join(root, "bundle.tar")})
		if err != nil {
			t.Fatal(err)
		}
		options := PlanOptions{
			Store: StoreOptions{GraphDriverName: "vfs"}, Isolation: "rootless", Runtime: "crun",
			RunControls: RunControls{Devices: []string{"/dev/test-device"}},
		}
		keys, eligible, err := packageCacheKeys(context.Background(), options, plan, stages, outputs, nil)
		want := strings.HasPrefix(instruction, "env")
		if err != nil || eligible != want || (len(keys) != 0) != want {
			t.Fatalf("global device with %s: eligible=%v keys=%v err=%v", instruction, eligible, keys, err)
		}
	}
}

func TestPackageCacheKeyChangesWithImmutableNamedContext(t *testing.T) {
	contextSpec := buildcontext.Spec{Name: "assets", Kind: buildcontext.Local, Path: t.TempDir()}
	plan := testPublicationPlanWithContexts(t, `package as="bundle"
copy "/payload" "/payload" from="assets"
extend
copy "/payload" "/payload" from="bundle"
`, contextSpec)
	root := t.TempDir()
	stages, outputs, err := validatePublicationGraph(plan, map[string]string{"bundle": filepath.Join(root, "bundle.tar")})
	if err != nil {
		t.Fatal(err)
	}
	options := PlanOptions{
		Store:      StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
		ContextDir: root, Isolation: "rootless", Runtime: "crun",
	}
	key := graphNamedContextKey(contextSpec.Name, plan.Platform)
	digests := make([]digest.Digest, 0, 2)
	for _, identity := range []string{"one", "two"} {
		options.ResolvedBases = map[ResolvedBaseKey]ResolvedImageSource{key: {
			Selected: v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString(identity), Size: 1},
		}}
		keys, eligible, err := packageCacheKeys(context.Background(), options, plan, stages, outputs, nil)
		if err != nil || !eligible {
			t.Fatalf("package keys eligible=%v err=%v", eligible, err)
		}
		value, err := keys[plan.Outputs[0]].Digest()
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, value)
	}
	if digests[0] == digests[1] {
		t.Fatal("immutable named-context change did not invalidate package cache key")
	}

	options.ResolvedBases = nil
	keys, eligible, err := packageCacheKeys(context.Background(), options, plan, stages, outputs, nil)
	if err != nil || eligible || keys != nil {
		t.Fatalf("unselected named context cache eligibility=%v keys=%v err=%v", eligible, keys, err)
	}
}

func TestPackageCacheExecutorIncludesGraphDriverOptions(t *testing.T) {
	options := PlanOptions{
		Store: StoreOptions{GraphDriverName: "overlay"}, Isolation: "rootless", Network: "default",
	}
	without, err := packageCacheExecutor(options, "crun")
	if err != nil {
		t.Fatal(err)
	}
	options.Store.GraphDriverOptions = []string{"overlay.force_mask=0700"}
	with, err := packageCacheExecutor(options, "crun")
	if err != nil {
		t.Fatal(err)
	}
	if without == with {
		t.Fatal("graph driver options did not invalidate package cache semantics")
	}
	options.Store.GraphDriverOptions = nil
	options.AddHosts = []string{"example.test:127.0.0.1"}
	withHost, err := packageCacheExecutor(options, "crun")
	if err != nil {
		t.Fatal(err)
	}
	if without == withHost {
		t.Fatalf("add-host execution options did not distinguish executor identities: %q %q", without, withHost)
	}
}
