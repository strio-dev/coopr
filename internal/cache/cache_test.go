package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/stateidentity"

	"github.com/gofrs/flock"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func testKey() Key {
	return Key{Input: stateidentity.Identity{Filesystem: digest.FromString("fs"), Configuration: digest.FromString("config"), State: digest.FromString("state")}, Instruction: "RUN true", Parameters: map[string]string{"b": "2", "a": "1"}, Inputs: map[string]digest.Digest{"context": digest.FromString("context")}, Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Executor: "buildkit-v0.33.0", Frontend: "dockerfile-v1", Lowering: "coopr-v1"}
}

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
	keys := []PackageKey{testPackageKey(), testPackageKey()}
	keys[1].Output = "another"
	data := []byte("package payload")
	source := filepath.Join(stage, "payload.tar")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	stores := []*OCIStore{first, second}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = stores[i].PutPackage(context.Background(), keys[i], testPackageRecord(keys[i], data), source)
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
		got, _, err := first.LookupPackage(context.Background(), key)
		if err != nil || got.Artifact.Digest == "" {
			t.Fatalf("tag lost after concurrent Put: %+v %v", got, err)
		}
	}
	lock := flock.New(filepath.Join(root, ".coopr-cache.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err = second.LookupPackage(ctx, keys[0])
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
