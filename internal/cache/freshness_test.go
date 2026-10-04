package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestCacheTTLRejectsBeforeHydration(t *testing.T) {
	hour := time.Hour
	zero := time.Duration(0)
	for _, kind := range []string{"instruction", "component", "package"} {
		t.Run(kind, func(t *testing.T) {
			for _, test := range []struct {
				name string
				ttl  *time.Duration
				age  time.Duration
				hit  bool
			}{
				{"expired", &hour, 2 * time.Hour, false}, {"fresh", &hour, time.Minute, true}, {"unlimited", nil, 2 * time.Hour, true}, {"zero", &zero, 0, false},
			} {
				t.Run(test.name, func(t *testing.T) {
					ctx := context.Background()
					created := time.Now().Add(-test.age)
					var store *OCIStore
					var counted *countedCacheTarget
					var payload digest.Digest
					var lookup func(context.Context) (string, error)
					if kind == "instruction" {
						var key ImageKey
						var descriptors []v1.Descriptor
						store, key, descriptors, counted = imageLookupFixture(t, created)
						payload = descriptors[2].Digest
						lookup = func(ctx context.Context) (string, error) {
							_, path, err := store.LookupImage(ctx, key)
							return path, err
						}
					} else {
						target, err := orasoci.New(t.TempDir())
						if err != nil {
							t.Fatal(err)
						}
						counted = &countedCacheTarget{Target: target, fetches: map[digest.Digest]int{}}
						store = &OCIStore{target: counted, stagingDir: t.TempDir()}
						data := []byte("cache snapshot")
						source := filepath.Join(t.TempDir(), "snapshot.tar")
						if err := os.WriteFile(source, data, 0600); err != nil {
							t.Fatal(err)
						}
						if kind == "package" {
							key := testPackageKey()
							record := testPackageRecord(key, data)
							record.CreatedAt = created
							payload = record.Descriptor.Digest
							if _, err := store.PutPackage(ctx, key, record, source); err != nil {
								t.Fatal(err)
							}
							lookup = func(ctx context.Context) (string, error) {
								_, path, err := store.LookupPackage(ctx, key)
								return path, err
							}
						} else {
							key := testKey()
							record := testRecord(key)
							record.CreatedAt = created
							record.ChangedFS = true
							record.Output.Filesystem = digest.FromString("after")
							record.Output.State = digest.FromString("after-state")
							pkg := testPackageRecord(testPackageKey(), data)
							payload = pkg.Descriptor.Digest
							record.Snapshot = &oci.Package{Stage: "snapshot", Descriptor: pkg.Descriptor, Config: pkg.Config}
							if _, err := store.Put(ctx, key, record, source); err != nil {
								t.Fatal(err)
							}
							lookup = func(ctx context.Context) (string, error) { _, path, err := store.Lookup(ctx, key); return path, err }
						}
					}
					store.ttl = test.ttl
					path, err := lookup(ctx)
					if path != "" {
						t.Cleanup(func() { _ = os.RemoveAll(path) })
					}
					if test.hit {
						if err != nil || path == "" {
							t.Fatalf("fresh record missed: path=%s err=%v", path, err)
						}
					} else {
						if !errors.Is(err, ErrMiss) || path != "" {
							t.Fatalf("expired record hit: path=%s err=%v", path, err)
						}
						if kind == "instruction" {
							// Only cache artifact/config metadata may be fetched.
							if len(counted.fetches) != 2 {
								t.Fatalf("expired image graph fetched beyond cache metadata: %v", counted.fetches)
							}
						}
						if counted.fetches[payload] != 0 {
							t.Fatalf("expired payload fetched %d times", counted.fetches[payload])
						}
						entries, err := os.ReadDir(store.stagingDir)
						if err != nil || len(entries) != 0 {
							t.Fatalf("expired record staged data: %v %v", entries, err)
						}
					}
					canceled, cancel := context.WithCancel(ctx)
					cancel()
					if _, err := lookup(canceled); !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation became expiry/miss: %v", err)
					}
				})
			}
		})
	}
}
