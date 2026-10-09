package buildah

import (
	"context"
	"coopr/internal/imageconfig"
	"encoding/json"
	"errors"
	"fmt"
	registryserver "github.com/google/go-containerregistry/pkg/registry"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"coopr/internal/cache"
	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"coopr/internal/stateidentity"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestComponentCacheRelaysDeferredImageCandidate(t *testing.T) {
	workerRoot := t.TempDir()
	parentRoot := t.TempDir()
	layout := filepath.Join(workerRoot, "candidate", "layout")
	if err := os.MkdirAll(layout, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString("manifest"), Size: 123}
	index, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []v1.Descriptor{manifest}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout, "index.json"), index, 0o600); err != nil {
		t.Fatal(err)
	}
	key := cache.ImageKey{
		Instruction: digest.FromString("instruction"), Parent: digest.FromString("parent"),
		Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Executor: "executor", Format: "oci",
	}
	worker := &componentCache{stagingDir: workerRoot, images: &portableInstructionCache{stagingDir: workerRoot, candidates: []portableInstructionCandidate{{
		key: key, record: cache.ImageRecord{Version: cache.ImageRecordVersion, Key: key, Image: manifest}, layout: layout,
	}}}}
	relays, err := worker.relayCandidates(parentRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.close(); err != nil {
		t.Fatal(err)
	}
	parent := &componentCache{stagingDir: parentRoot}
	if err := parent.acceptRelayedCandidates(relays); err != nil {
		t.Fatal(err)
	}
	if len(parent.images.candidates) != 1 {
		t.Fatalf("accepted %d candidates, want 1", len(parent.images.candidates))
	}
	if _, err := os.Stat(filepath.Join(parent.images.candidates[0].layout, "index.json")); err != nil {
		t.Fatalf("relayed layout did not survive worker cleanup: %v", err)
	}
}

func TestComponentCacheLayoutCannotOverlapBuildState(t *testing.T) {
	root := t.TempDir()
	resolver, err := oci.NewResolver(oci.Options{
		ComponentStoreDir: filepath.Join(root, "components"),
	})
	if err != nil {
		t.Fatal(err)
	}
	base := PlanOptions{
		Store: StoreOptions{
			GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"),
		},
		Output:     Output{Path: filepath.Join(root, "output")},
		ContextDir: filepath.Join(root, "context"),
		Resolver:   resolver,
	}
	for _, test := range []struct {
		name, path, want string
	}{
		{"separate layout", filepath.Join(root, "context", ".coopr-cache"), ""},
		{"relative layout", "cache", "must be absolute"},
		{"graph child", filepath.Join(root, "graph", "cache"), "overlaps execution graph"},
		{"graph parent", root, "overlaps execution graph"},
		{"run child", filepath.Join(root, "run", "cache"), "overlaps execution run root"},
		{"output child", filepath.Join(root, "output", "cache"), "overlaps output"},
		{"component store child", filepath.Join(root, "components", "cache"), "overlaps component store"},
		{"context parent", filepath.Join(root, "context"), "contains the build context"},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := base
			options.CacheLocalDir = test.path
			err := validateComponentCachePath(options)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validate cache path %q = %v, want %q", test.path, err, test.want)
			}
		})
	}
}

func TestComponentCacheEligibilityFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name       string
		properties map[string]string
		children   []definition.Instruction
		want       bool
	}{
		{"arg", nil, nil, true},
		{"env", nil, nil, true},
		{"label", nil, nil, true},
		{"user", nil, nil, true},
		{"cmd", nil, nil, true},
		{"entrypoint", nil, nil, true},
		{"shell", nil, nil, true},
		{"stopsignal", nil, nil, true},
		{"expose", nil, nil, true},
		{"volume", nil, nil, true},
		{"maintainer", nil, nil, true},
		{"workdir", nil, nil, true},
		{"run", map[string]string{"network": "none"}, nil, true},
		{"run", map[string]string{"network": "default"}, nil, true},
		{"run", map[string]string{"network": "host"}, nil, false},
		{"run", map[string]string{"network": "none"}, []definition.Instruction{{Name: "mount", Arguments: []string{"cache"}, Properties: map[string]string{"target": "/cache"}}}, true},
		{"run", map[string]string{"network": "none"}, []definition.Instruction{{Name: "mount", Arguments: []string{"secret"}, Properties: map[string]string{"id": "token"}}}, true},
		{"run", map[string]string{"network": "none"}, []definition.Instruction{{Name: "mount", Arguments: []string{"bind"}, Properties: map[string]string{"from": "pkg", "target": "/input"}}}, true},
		{"run", map[string]string{"network": "none"}, []definition.Instruction{{Name: "mount", Arguments: []string{"bind"}, Properties: map[string]string{"target": "/input"}}}, false},
		{"copy", map[string]string{"from": "pkg"}, nil, true},
		{"add", map[string]string{"from": "pkg"}, nil, true},
		{"copy", nil, nil, false},
		{"component", nil, nil, false},
		{"future-host-input", nil, nil, false},
	} {
		t.Run(test.name+"/"+test.properties["network"]+"/"+test.properties["from"], func(t *testing.T) {
			resolved := &ResolvedComponentPlan{
				Plan: &planner.Plan{Outputs: []string{"output"}, Stages: []planner.Stage{
					{ID: "pkg", Name: "pkg", Kind: "package-input"},
					{ID: "output", Kind: "extend", Operations: []planner.Operation{{Instruction: definition.Instruction{
						Name: test.name, Properties: test.properties, Children: test.children,
					}}}},
				}},
				PackageInputs: map[string]oci.Package{"pkg": {}},
			}
			if got := componentCacheEligible(resolved, RunControls{}); got != test.want {
				t.Fatalf("componentCacheEligible(%s) = %v, want %v", test.name, got, test.want)
			}
			wantWithDevice := test.want && test.name != "run"
			if got := componentCacheEligible(resolved, RunControls{Devices: []string{"/dev/test-device"}}); got != wantWithDevice {
				t.Fatalf("componentCacheEligible(%s, global device) = %v, want %v", test.name, got, wantWithDevice)
			}
		})
	}
}

func TestComponentCacheKeyIncludesSelectedExternalBase(t *testing.T) {
	selectedKey := ResolvedBaseKey{Reference: "registry.example/base:latest", Platform: "linux/amd64"}
	resolved := func(selected digest.Digest) *ResolvedComponentPlan {
		return &ResolvedComponentPlan{
			Identity: digest.FromString("component"),
			SelectedBases: map[ResolvedBaseKey]ResolvedImageSource{
				selectedKey: {Selected: v1.Descriptor{Digest: selected}},
			},
		}
	}
	keyDigest := func(component *ResolvedComponentPlan) digest.Digest {
		key := cache.Key{
			Input: stateidentity.Identity{
				Filesystem: digest.FromString("filesystem"), Configuration: digest.FromString("configuration"), State: digest.FromString("state"),
			},
			Component: component.Identity,
			Inputs:    componentCacheInputs(component),
			Platform:  v1.Platform{OS: "linux", Architecture: "amd64"},
			Executor:  "executor", Frontend: "frontend", Lowering: "lowering",
		}
		got, err := key.Digest()
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	first := keyDigest(resolved(digest.FromString("base-one")))
	second := keyDigest(resolved(digest.FromString("base-two")))
	if first == second {
		t.Fatalf("different selected external bases produced the same cache key %s", first)
	}
}

func TestRunControlsChangePortableCacheIdentity(t *testing.T) {
	base, err := cacheRunControlsDigest(RunControls{})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := cacheRunControlsDigest(RunControls{HTTPProxy: true, Memory: 64 * 1024 * 1024, DNSServers: []string{"127.0.0.53"}})
	if err != nil {
		t.Fatal(err)
	}
	if base == changed {
		t.Fatalf("RUN controls did not change portable cache identity %s", base)
	}
}

func validComponentTestKey() cache.Key {
	return cache.Key{Input: stateidentity.Identity{Filesystem: digest.FromString("fs"), Configuration: digest.FromString("config"), State: digest.FromString("state")}, Component: digest.FromString("component"), Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Executor: "executor", Frontend: "frontend", Lowering: portableCacheLoweringVersion}
}

func TestComponentCacheLookupPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &componentCache{}
	entry, _, err := c.lookup(ctx, nil, validComponentTestKey(), nil, componentCacheCaller{})
	if entry.ImageID != "" || !errors.Is(err, context.Canceled) || c.stats.Misses != 0 {
		t.Fatalf("cancelled lookup: entry=%+v err=%v stats=%+v", entry, err, c.stats)
	}
}

func TestComponentCachePublishPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &componentCache{images: &portableInstructionCache{}}
	if err := c.publish(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestComponentCacheExportReturnsAllFailuresAfterCleanup(t *testing.T) {
	layout := filepath.Join(t.TempDir(), "candidate", "layout")
	if err := os.MkdirAll(layout, 0700); err != nil {
		t.Fatal(err)
	}
	c := &componentCache{images: &portableInstructionCache{writeStores: []instructionCacheStore{failingInstructionCacheStore{err: errors.New("first export failed")}, failingInstructionCacheStore{err: errors.New("second export failed")}}, candidates: []portableInstructionCandidate{{layout: layout}}}}
	err := c.publish(context.Background())
	if err == nil || !strings.Contains(err.Error(), "first export failed") || !strings.Contains(err.Error(), "second export failed") {
		t.Fatal(err)
	}
	if len(c.images.candidates) != 0 || c.stats.Errors != 2 || c.stats.Stored != 0 {
		t.Fatalf("stats %+v", c.stats)
	}
	if _, err := os.Stat(filepath.Dir(layout)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestComponentCacheOptionalReadWarnsWithCause(t *testing.T) {
	failure := errors.New("injected cache read permission failure")
	c := &componentCache{images: &portableInstructionCache{readStores: []instructionCacheStore{unavailableInstructionCacheStore{err: failure}}}}
	warning := packageWarningOutput(t, func() {
		entry, _, err := c.lookup(context.Background(), &graphExecutor{}, validComponentTestKey(), nil, componentCacheCaller{})
		if entry.ImageID != "" || err != nil || c.stats.Errors != 1 || c.stats.Misses != 1 {
			t.Fatalf("entry %+v err %v stats %+v", entry, err, c.stats)
		}
	})
	if !strings.Contains(warning, failure.Error()) {
		t.Fatal(warning)
	}
}

func TestComponentImageCacheKeyRejectsOldLoweringAndCallerGraphChanges(t *testing.T) {
	original := validComponentTestKey()
	original.Inputs = map[string]digest.Digest{"caller-image": digest.FromString("caller-one")}
	a, err := componentImageCacheKey(original, "oci")
	if err != nil {
		t.Fatal(err)
	}
	original.Lowering = "coopr-buildah-component-v2"
	b, err := componentImageCacheKey(original, "oci")
	if err != nil {
		t.Fatal(err)
	}
	original.Lowering = portableCacheLoweringVersion
	original.Inputs["caller-image"] = digest.FromString("caller-two")
	c, err := componentImageCacheKey(original, "oci")
	if err != nil {
		t.Fatal(err)
	}
	if a.Instruction == b.Instruction || a.Instruction == c.Instruction {
		t.Fatal("old snapshot semantics or alternate caller graph reused image key")
	}
}

func TestComponentCacheSeedFailureReachesBuildCaller(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for rootless graph error propagation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	componentResolver := localConfigComponentResolver(t, ctx, root, "extend\nenv CACHE_PROOF=\"yes\"\n", false)
	destination := filepath.Join(root, "seed-destination")
	var inject atomic.Bool
	var injected atomic.Bool
	registry := registryserver.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if inject.Load() && r.Method == http.MethodGet && (strings.Contains(r.URL.Path, "/manifests/") || strings.Contains(r.URL.Path, "/blobs/")) && injected.CompareAndSwap(false, true) {
			lock := filepath.Join(destination, ".coopr-cache.lock")
			if err := os.Remove(lock); err != nil {
				t.Errorf("remove fixture lock: %v", err)
			}
			if err := os.Mkdir(lock, 0700); err != nil {
				t.Errorf("inject seed failure: %v", err)
			}
		}
		registry.ServeHTTP(w, r)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	resolver, err := oci.NewResolver(oci.Options{ComponentStoreDir: componentResolver.ComponentStoreDir(), TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, "from \"scratch\"\ncomponent \"local:config\"\n")
	policy := writeComponentTestPolicy(t, root)
	options := componentTestOptions(root, filepath.Join(root, "cold"), resolver, policy)
	options.CacheRepository = host + "/coopr/cache"
	if _, err := BuildPlan(ctx, plan, options); err != nil {
		t.Fatal(err)
	}
	options = componentTestOptions(root, filepath.Join(root, "warm"), resolver, policy)
	options.CacheFrom = []CacheSpec{{Transport: "registry", Reference: host + "/coopr/cache"}}
	options.CacheTo = []CacheSpec{{Transport: "oci-layout", Reference: destination}}
	inject.Store(true)
	_, err = BuildPlan(ctx, plan, options)
	if !injected.Load() || err == nil || !strings.Contains(err.Error(), "component cache lookup") || !strings.Contains(err.Error(), "seed instruction cache") || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("seed cause did not reach graph caller: injected=%v err=%v", injected.Load(), err)
	}
	if _, err := os.Stat(options.Output.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("seed failure published required output: %v", err)
	}
}

func TestComponentCachedConfigMatchesNativeFieldsButRetainsExtensions(t *testing.T) {
	native := []byte(`{"author":"author","config":{"Env":["name=value"],"User":"1","Cmd":["hello"],"Labels":{"name":"value"}}}`)
	for _, test := range []struct {
		name, logical string
		valid         bool
	}{
		{"extensions", `{"author":"author","config":{"Env":["name=value"],"User":"1","Cmd":["hello"],"Labels":{"name":"value"},"Shell":["custom-shell"],"OnBuild":["ENV inherited=yes"]}}`, true},
		{"runtime-conflict", `{"author":"author","config":{"Env":["name=wrong"],"User":"1","Cmd":["hello"],"Labels":{"name":"value"}}}`, false},
		{"author-conflict", `{"author":"other","config":{"Env":["name=value"],"User":"1","Cmd":["hello"],"Labels":{"name":"value"}}}`, false},
		{"malformed", `{`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := componentCachedConfigMatchesImage([]byte(test.logical), native)
			if (err == nil) != test.valid {
				t.Fatalf("err=%v valid=%v", err, test.valid)
			}
		})
	}
}

type mutatingComponentImageCacheStore struct {
	instructionCacheStore
	mutate func(*cache.ImageRecord)
}

func (s mutatingComponentImageCacheStore) LookupImage(ctx context.Context, key cache.ImageKey) (*cache.ImageRecord, string, error) {
	record, path, err := s.instructionCacheStore.LookupImage(ctx, key)
	if err == nil {
		s.mutate(record)
	}
	return record, path, err
}

func TestComponentImageCacheInvalidMetadataMissesBeforeSeed(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	f.write(t, "one", "one")
	result, _ := f.build(t, "image", fmt.Sprintf("from %q\ncopy \"one\" \"/one\"\n", base.reference))
	lease, err := acquireStore(f.options.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close() //nolint:errcheck
	sourceBase, err := lease.store.Image(base.reference)
	if err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	store, err := cache.NewLocalStore(f.ctx, filepath.Join(f.root, "metadata-cache"), staging, nil)
	if err != nil {
		t.Fatal(err)
	}
	nativeRaw, err := packageImageConfig(f.ctx, lease.store, result.ImageID, f.options.SystemContext)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := imageconfig.Parse(nativeRaw)
	if err != nil {
		t.Fatal(err)
	}
	executor := &graphExecutor{store: lease.store, options: f.options}
	key := validComponentTestKey()
	producer := &componentCache{stagingDir: staging, writeStores: []cache.Store{store}}
	producer.record(f.ctx, executor, key, result.ImageID, digest.Digest(result.ManifestDigest), logical, &PackageRootMetadata{Mode: 0755}, f.options.SystemContext, sourceBase.ID, true)
	if err := producer.publish(f.ctx); err != nil {
		t.Fatal(err)
	}
	imageStore := store
	for _, test := range []struct {
		name       string
		mutate     func(*cache.ImageRecord)
		denyCaller bool
	}{
		{"missing-root", func(r *cache.ImageRecord) { r.RootMetadata = nil }, false},
		{"missing-config", func(r *cache.ImageRecord) { r.LogicalConfig = nil }, false},
		{"missing-lineage", func(r *cache.ImageRecord) { r.RetainsCaller = nil }, false},
		{"missing-base", func(r *cache.ImageRecord) { r.BaseImage = nil }, false},
		{"malformed-config", func(r *cache.ImageRecord) { r.LogicalConfig = json.RawMessage(`{`) }, false},
		{"conflicting-config", func(r *cache.ImageRecord) {
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(r.LogicalConfig, &doc); err != nil {
				t.Fatal(err)
			}
			var config map[string]json.RawMessage
			if err := json.Unmarshal(doc["config"], &config); err != nil {
				t.Fatal(err)
			}
			config["User"] = json.RawMessage(`"999"`)
			raw, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			doc["config"] = raw
			r.LogicalConfig, err = json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
		}, false},
		{"false-caller-ancestry", func(r *cache.ImageRecord) { retained := true; r.RetainsCaller = &retained }, true},
		{"caller-chain-conflict", func(*cache.ImageRecord) {}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			consumer := &componentCache{stagingDir: t.TempDir(), readStores: []cache.Store{}, images: &portableInstructionCache{stagingDir: t.TempDir(), validateHit: validateComponentImageCacheHit, readStores: []instructionCacheStore{mutatingComponentImageCacheStore{instructionCacheStore: imageStore, mutate: test.mutate}}, writeStores: []instructionCacheStore{failingInstructionCacheStore{err: errors.New("invalid metadata must not be seeded")}}}}
			warning := packageWarningOutput(t, func() {
				callerID := sourceBase.ID
				if test.name == "caller-chain-conflict" {
					callerID = result.ImageID
				}
				entry, _, err := consumer.lookup(f.ctx, executor, key, f.options.SystemContext, componentCacheCaller{ImageID: callerID, RetainsCallerAllowed: !test.denyCaller})
				if err != nil || entry.ImageID != "" || consumer.stats.Errors != 1 || consumer.stats.Misses != 1 || consumer.stats.Hits != 0 || consumer.stats.Stored != 0 {
					t.Fatalf("invalid cache promoted: entry=%+v err=%v stats=%+v", entry, err, consumer.stats)
				}
			})
			if !strings.Contains(warning, "cache") {
				t.Fatalf("invalid metadata failure was hidden: %s", warning)
			}
		})
	}
}

func TestComponentCachedExtensionsAgreeWhenNativeDockerRepresentsThem(t *testing.T) {
	for _, name := range []string{"Shell", "OnBuild", "Healthcheck"} {
		t.Run(name, func(t *testing.T) {
			value := json.RawMessage(`["native"]`)
			other := json.RawMessage(`["other"]`)
			if name == "Hostname" {
				value = json.RawMessage(`"native"`)
				other = json.RawMessage(`"other"`)
			}
			if name == "Healthcheck" {
				value = json.RawMessage(`{"Test":["CMD","native"],"Interval":1000000000}`)
				other = json.RawMessage(`{"Test":["CMD","other"],"Interval":1000000000}`)
			}
			native, err := json.Marshal(map[string]any{"config": map[string]json.RawMessage{name: value}})
			if err != nil {
				t.Fatal(err)
			}
			matching := append([]byte(nil), native...)
			conflicting, err := json.Marshal(map[string]any{"config": map[string]json.RawMessage{name: other}})
			if err != nil {
				t.Fatal(err)
			}
			if err := componentCachedConfigMatchesImage(matching, native); err != nil {
				t.Fatal(err)
			}
			if err := componentCachedConfigMatchesImage(conflicting, native); err == nil {
				t.Fatal("accepted conflicting native extension")
			}
		})
	}
}
