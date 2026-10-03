package buildah

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/cache"
	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"coopr/internal/stateidentity"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestComponentCacheRelaysDeferredCandidate(t *testing.T) {
	workerRoot := t.TempDir()
	parentRoot := t.TempDir()
	payload := []byte("portable component snapshot")
	snapshot := filepath.Join(workerRoot, "snapshot.tar")
	if err := os.WriteFile(snapshot, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := stateidentity.Identity{
		Filesystem: digest.FromString("filesystem"), Configuration: digest.FromString("configuration"), State: digest.FromString("state"),
	}
	key := cache.Key{
		Input: identity, Component: digest.FromString("component"), Platform: v1.Platform{OS: "linux", Architecture: "amd64"},
		Executor: "executor", Frontend: "frontend", Lowering: "lowering",
	}
	record := cache.Record{
		Version: cache.RecordVersion, Key: key, Output: identity, ChangedFS: true,
		Snapshot: &oci.Package{Stage: "output", Descriptor: v1.Descriptor{MediaType: cache.SnapshotMediaType, Digest: digest.FromBytes(payload), Size: int64(len(payload))}},
	}
	worker := &componentCache{stagingDir: workerRoot, candidates: []componentCacheCandidate{{key: key, record: record, path: snapshot}}}
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
	if len(parent.candidates) != 1 {
		t.Fatalf("accepted %d candidates, want 1", len(parent.candidates))
	}
	if got, err := os.ReadFile(parent.candidates[0].path); err != nil || string(got) != string(payload) {
		t.Fatalf("relayed snapshot = %q, %v", got, err)
	}
}

func TestComponentCacheLayoutCannotOverlapBuildState(t *testing.T) {
	root := t.TempDir()
	resolver, err := oci.NewResolver(oci.Options{
		ImageStoreDir: filepath.Join(root, "images"), ComponentStoreDir: filepath.Join(root, "components"),
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
		{"image store child", filepath.Join(root, "images", "cache"), "overlaps image store"},
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

type cancelCacheStore struct {
	lookup func(context.Context) (*cache.Record, string, error)
	put    func(context.Context) (v1.Descriptor, error)
}

func (s cancelCacheStore) Lookup(ctx context.Context, _ cache.Key) (*cache.Record, string, error) {
	return s.lookup(ctx)
}

func (s cancelCacheStore) Put(ctx context.Context, _ cache.Key, _ cache.Record, _ string) (v1.Descriptor, error) {
	return s.put(ctx)
}

func TestComponentCacheLookupPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := cancelCacheStore{lookup: func(context.Context) (*cache.Record, string, error) {
		cancel()
		return nil, "", cache.ErrMiss
	}}
	c := &componentCache{readStores: []cache.Store{store}}
	_, _, hit, err := c.lookup(ctx, nil, cache.Key{}, "", nil, nil, v1.Platform{OS: "linux", Architecture: "amd64"}, nil)
	if hit || !errors.Is(err, context.Canceled) || c.stats.Misses != 0 {
		t.Fatalf("cancelled lookup: hit=%v err=%v stats=%+v", hit, err, c.stats)
	}
}

func TestComponentCachePublishPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := cancelCacheStore{put: func(context.Context) (v1.Descriptor, error) {
		cancel()
		return v1.Descriptor{}, errors.New("optional transport failure")
	}}
	c := &componentCache{writeStores: []cache.Store{store}, candidates: []componentCacheCandidate{{}}}
	if err := c.publish(ctx); !errors.Is(err, context.Canceled) || c.stats.Errors != 0 {
		t.Fatalf("cancelled publish: err=%v stats=%+v", err, c.stats)
	}
}

func TestComponentCacheExportReturnsAllFailuresAfterCleanup(t *testing.T) {
	first := cancelCacheStore{put: func(context.Context) (v1.Descriptor, error) {
		return v1.Descriptor{}, errors.New("first export failed")
	}}
	second := cancelCacheStore{put: func(context.Context) (v1.Descriptor, error) {
		return v1.Descriptor{}, errors.New("second export failed")
	}}
	path := filepath.Join(t.TempDir(), "snapshot.tar")
	if err := os.WriteFile(path, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &componentCache{writeStores: []cache.Store{first, second}, candidates: []componentCacheCandidate{{path: path}}}
	err := c.publish(context.Background())
	if err == nil || !strings.Contains(err.Error(), "first export failed") || !strings.Contains(err.Error(), "second export failed") {
		t.Fatalf("component cache export error = %v", err)
	}
	if len(c.candidates) != 0 || c.stats.Errors != 2 || c.stats.Stored != 0 {
		t.Fatalf("component cache after failed export = candidates %d stats %+v", len(c.candidates), c.stats)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged component cache remains: %v", err)
	}
}
