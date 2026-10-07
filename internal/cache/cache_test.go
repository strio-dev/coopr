package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"coopr/internal/oci"
	"coopr/internal/stateidentity"
	"github.com/gofrs/flock"
	"github.com/google/go-containerregistry/pkg/registry"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func testKey() Key {
	return Key{Input: stateidentity.Identity{Filesystem: digest.FromString("fs"), Configuration: digest.FromString("config"), State: digest.FromString("state")}, Instruction: "RUN true", Parameters: map[string]string{"b": "2", "a": "1"}, Inputs: map[string]digest.Digest{"context": digest.FromString("context")}, Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Executor: "buildkit-v0.33.0", Frontend: "dockerfile-v1", Lowering: "coopr-v1"}
}

func testConfig() json.RawMessage {
	return json.RawMessage(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
}

func testRecord(key Key) Record { return Record{Key: key, Output: key.Input, Config: testConfig()} }

func TestKeyCanonicalAndInvalidation(t *testing.T) {
	base := testKey()
	other := base
	other.Parameters = map[string]string{"a": "1", "b": "2"}
	other.Platform.OSFeatures = []string{"b", "a"}
	base.Platform.OSFeatures = []string{"a", "b"}
	first, err := base.Digest()
	if err != nil {
		t.Fatal(err)
	}
	second, err := other.Digest()
	if err != nil || first != second {
		t.Fatalf("map/feature order changed key: %s %s %v", first, second, err)
	}
	changes := []func(*Key){func(k *Key) { k.Input.Filesystem = digest.FromString("different") }, func(k *Key) { k.Parameters["a"] = "changed" }, func(k *Key) { k.Inputs["context"] = digest.FromString("changed") }, func(k *Key) { k.Platform.Architecture = "arm64" }, func(k *Key) { k.Platform.OSVersion = "1" }, func(k *Key) { k.Platform.OSFeatures = []string{"feature"} }, func(k *Key) {
		k.Packages = map[string]v1.Descriptor{"pkg": oci.Descriptor(oci.ComponentPackageType, []byte("payload"))}
	}, func(k *Key) { k.Executor = "new" }, func(k *Key) { k.Frontend = "new" }, func(k *Key) { k.Lowering = "new" }, func(k *Key) { k.Component = digest.FromString("component"); k.Instruction = "" }}
	for i, change := range changes {
		modified := base
		modified.Parameters = map[string]string{"a": "1", "b": "2"}
		modified.Inputs = map[string]digest.Digest{"context": base.Inputs["context"]}
		change(&modified)
		got, err := modified.Digest()
		if err != nil || got == first {
			t.Fatalf("change %d did not invalidate key: %s %v", i, got, err)
		}
	}
	invalid := base
	invalid.Parameters = map[string]string{"x": "\xff"}
	if _, err := invalid.Digest(); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
	invalid = base
	invalid.Input.State = digest.Digest("sha512:" + strings.Repeat("a", 128))
	if _, err := invalid.Digest(); err == nil {
		t.Fatal("non-SHA256 identity accepted")
	}
	invalid = base
	invalid.Packages = map[string]v1.Descriptor{"pkg": {MediaType: oci.ComponentPackageType, Digest: digest.FromString("pkg"), Size: 1, URLs: []string{"https://example.invalid"}}}
	if _, err := invalid.Digest(); err == nil {
		t.Fatal("package descriptor URL accepted")
	}
}

func TestStoreLocalAndRegistryRoundTrip(t *testing.T) {
	ctx := context.Background()
	key := testKey()
	data := []byte("portable snapshot bytes")
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "registry"}[remote], func(t *testing.T) {
			stage := t.TempDir()
			var store *OCIStore
			var err error
			if remote {
				server := httptest.NewServer(registry.New())
				t.Cleanup(server.Close)
				resolver, e := oci.NewResolver(oci.Options{TLSVerify: new(false)})
				if e != nil {
					t.Fatal(e)
				}
				store, err = NewRegistryStore(resolver, strings.TrimPrefix(server.URL, "http://")+"/coopr/cache", stage, nil)
			} else {
				store, err = NewLocalStore(ctx, filepath.Join(t.TempDir(), "layout"), stage, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Lookup(ctx, key); !errors.Is(err, ErrMiss) {
				t.Fatalf("expected miss, got %v", err)
			}
			path := filepath.Join(stage, "snapshot.tar")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			desc := oci.Descriptor(SnapshotMediaType, data)
			snapshotConfig := json.RawMessage(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["` + desc.Digest.String() + `"]}}`)
			record := testRecord(key)
			record.ChangedFS = true
			record.Output.Filesystem = digest.FromString("after")
			record.Output.State = digest.FromString("after-state")
			record.Snapshot = &oci.Package{Stage: "snapshot", Descriptor: desc, Config: snapshotConfig}
			root, err := store.Put(ctx, key, record, path)
			if err != nil {
				t.Fatal(err)
			}
			got, snapshotPath, err := store.Lookup(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if got.Output != record.Output || snapshotPath == "" {
				t.Fatalf("bad record: %+v %q", got, snapshotPath)
			}
			if !sameDescriptor(got.Descriptor, root) {
				t.Fatalf("verified descriptor missing: %+v, want %+v", got.Descriptor, root)
			}
			read, err := os.ReadFile(snapshotPath)
			if err != nil || !bytes.Equal(read, data) {
				t.Fatalf("bad snapshot: %q %v", read, err)
			}
			_ = os.Remove(snapshotPath)
			replacement := record
			replacement.Output.Configuration = digest.FromString("refreshed-config")
			replacement.Output.State = digest.FromString("refreshed-state")
			replacement.Config = json.RawMessage(`{"architecture":"amd64","config":{"Env":["FRESH=1"]},"os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
			if _, err := store.Put(ctx, key, replacement, path); err != nil {
				t.Fatalf("replace existing cache key: %v", err)
			}
			got, snapshotPath, err = store.Lookup(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			_ = os.Remove(snapshotPath)
			if got.Output != replacement.Output || !bytes.Equal(got.Config, replacement.Config) {
				t.Fatalf("cache key retained stale record: got=%+v want=%+v", got, replacement)
			}
			changed := key
			changed.Instruction = "RUN changed"
			if _, _, err := store.Lookup(ctx, changed); !errors.Is(err, ErrMiss) {
				t.Fatalf("changed key was not a miss: %v", err)
			}
		})
	}
}

func TestLocalStoreConcurrentInstancesRetainBothTags(t *testing.T) {
	root, stage := filepath.Join(t.TempDir(), "layout"), t.TempDir()
	first, err := NewLocalStore(context.Background(), root, stage, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewLocalStore(context.Background(), root, stage, nil)
	if err != nil {
		t.Fatal(err)
	}
	keys := []Key{testKey(), testKey()}
	keys[1].Instruction = "RUN another"
	stores := []*OCIStore{first, second}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = stores[i].Put(context.Background(), keys[i], testRecord(keys[i]), "")
		}(i)
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range keys {
		got, _, err := first.Lookup(context.Background(), key)
		if err != nil || got.Descriptor.Digest == "" {
			t.Fatalf("tag lost after concurrent Put: %+v %v", got, err)
		}
	}
	lock := flock.New(filepath.Join(root, ".coopr-cache.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err = second.Lookup(ctx, keys[0])
	constructorCtx, constructorCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer constructorCancel()
	_, constructorErr := NewLocalStore(constructorCtx, root, stage, nil)
	_ = lock.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait did not respect cancellation: %v", err)
	}
	if !errors.Is(constructorErr, context.DeadlineExceeded) {
		t.Fatalf("constructor lock wait did not respect cancellation: %v", constructorErr)
	}
}

func TestStoreRejectsCorruptMetadataAndMissingBlob(t *testing.T) {
	ctx := context.Background()
	key := testKey()
	stage := t.TempDir()
	store, err := NewLocalStore(ctx, filepath.Join(t.TempDir(), "layout"), stage, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := testRecord(key)
	wrongPlatform := record
	wrongPlatform.Config = json.RawMessage(`{"architecture":"amd64","os":"linux","os.version":"different","rootfs":{"type":"layers","diff_ids":[]}}`)
	if _, err := store.Put(ctx, key, wrongPlatform, ""); err == nil {
		t.Fatal("mismatched full platform accepted")
	}
	_, err = store.Put(ctx, key, record, "")
	if err != nil {
		t.Fatal(err)
	}
	// A cache manifest whose declared config blob is absent is never a hit.
	manifest := oci.VersionedManifest(oci.Descriptor(ConfigMediaType, []byte("missing")), nil, ArtifactType)
	manifestData, _ := json.Marshal(manifest)
	missingRoot := oci.Descriptor(v1.MediaTypeImageManifest, manifestData)
	if err := store.target.Push(ctx, missingRoot, bytes.NewReader(manifestData)); err != nil {
		t.Fatal(err)
	}
	tagTestCacheRoot(t, store, key, missingRoot)
	if _, _, err := store.Lookup(ctx, key); err == nil {
		t.Fatal("missing config blob accepted")
	}
	// A malformed but content-addressed config is rejected after verification.
	badData := []byte(`{"version":"wrong"}`)
	badConfig := oci.Descriptor(ConfigMediaType, badData)
	badManifestData, _ := json.Marshal(oci.VersionedManifest(badConfig, nil, ArtifactType))
	badRoot := oci.Descriptor(v1.MediaTypeImageManifest, badManifestData)
	if err := store.target.Push(ctx, badConfig, bytes.NewReader(badData)); err != nil {
		t.Fatal(err)
	}
	if err := store.target.Push(ctx, badRoot, bytes.NewReader(badManifestData)); err != nil {
		t.Fatal(err)
	}
	tagTestCacheRoot(t, store, key, badRoot)
	if _, _, err := store.Lookup(ctx, key); err == nil {
		t.Fatal("invalid record accepted")
	}
}

func tagTestCacheRoot(t *testing.T, store *OCIStore, key Key, root v1.Descriptor) {
	t.Helper()
	target, release, err := store.operationTarget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	keyDigest, err := key.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Tag(context.Background(), root, cacheTag(keyDigest)); err != nil {
		t.Fatal(err)
	}
}
