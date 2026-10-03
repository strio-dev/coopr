package buildah

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"sync"
	"testing"
)

func TestStoreLeaseKeepsSharedStoreOpenUntilLastUser(t *testing.T) {
	options := testStoreOptions(t)
	first, err := acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := acquireStore(options)
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	if first.store != second.store {
		t.Fatal("same store options returned different store handles")
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.store.Images(); err != nil {
		t.Fatalf("remaining lease lost its store: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("closing a lease twice: %v", err)
	}

	key, _, err := storeKey(options)
	if err != nil {
		t.Fatal(err)
	}
	buildStores.Lock()
	defer buildStores.Unlock()
	if _, exists := buildStores.entries[key]; exists {
		t.Fatal("store remained registered after its last lease closed")
	}
}

func TestStoreLeaseSerializesConcurrentOwnership(t *testing.T) {
	options := testStoreOptions(t)
	const users = 16
	type acquired struct {
		lease *storeLease
		err   error
	}
	results := make(chan acquired, users)
	release := make(chan struct{})
	var group sync.WaitGroup
	group.Add(users)
	for range users {
		go func() {
			defer group.Done()
			lease, err := acquireStore(options)
			results <- acquired{lease: lease, err: err}
			if err == nil {
				<-release
				results <- acquired{err: lease.Close()}
			}
		}()
	}

	var shared *storeLease
	for range users {
		result := <-results
		if result.err != nil {
			t.Fatalf("acquire store: %v", result.err)
		}
		if shared == nil {
			shared = result.lease
		} else if shared.store != result.lease.store {
			t.Fatal("concurrent users acquired different store handles")
		}
	}

	key, _, err := storeKey(options)
	if err != nil {
		t.Fatal(err)
	}
	buildStores.Lock()
	entry := buildStores.entries[key]
	if entry == nil || entry.users != users {
		buildStores.Unlock()
		t.Fatalf("registered users = %v, want %d", entryUsers(entry), users)
	}
	buildStores.Unlock()

	close(release)
	for range users {
		if result := <-results; result.err != nil {
			t.Errorf("release store: %v", result.err)
		}
	}
	group.Wait()

	buildStores.Lock()
	defer buildStores.Unlock()
	if _, exists := buildStores.entries[key]; exists {
		t.Fatal("store remained registered after concurrent leases closed")
	}
}

func TestStoreLeaseRejectsConflictingOptionsForOpenRoots(t *testing.T) {
	options := testStoreOptions(t)
	lease, err := acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()

	conflict := options
	conflict.Native = NativeStoreOptions(options)
	conflict.Native.PullOptions = map[string]string{"use_hard_links": "true"}
	if _, err := acquireStore(conflict); err == nil {
		t.Fatal("same open roots accepted conflicting effective native options")
	}
}

func TestStoreOptionsWorkerJSONPreservesEffectiveNativeOptions(t *testing.T) {
	options := testStoreOptions(t)
	options.Shared = true
	options.Native = NativeStoreOptions(options)
	options.Native.GraphDriverPriority = []string{"overlay", "vfs"}
	options.Native.PullOptions = map[string]string{"enable_partial_images": "true"}
	options.Native.DisableVolatile = true
	data, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	var decoded StoreOptions
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Shared || !reflect.DeepEqual(NativeStoreOptions(decoded), NativeStoreOptions(options)) {
		t.Fatalf("worker JSON changed store options: got=%+v want=%+v", decoded, options)
	}
}

func TestStoreIdentitySeparatesRootsAndEffectiveOptions(t *testing.T) {
	base := testStoreOptions(t)
	first, err := StoreIdentity(base)
	if err != nil {
		t.Fatal(err)
	}
	changedRoot := base
	changedRoot.GraphRoot += "-other"
	second, err := StoreIdentity(changedRoot)
	if err != nil {
		t.Fatal(err)
	}
	changedOption := base
	changedOption.Native = NativeStoreOptions(base)
	changedOption.Native.PullOptions = map[string]string{"enable_partial_images": "true"}
	third, err := StoreIdentity(changedOption)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first == third || second == third {
		t.Fatalf("store identities collided: %q %q %q", first, second, third)
	}
}

func TestStoreLeaseForwardsSplitAndTransientStoreOptions(t *testing.T) {
	options := testStoreOptions(t)
	options.ImageStore = options.GraphRoot + "-images"
	options.TransientStore = true
	lease, err := acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	if got := lease.store.ImageStore(); got != options.ImageStore {
		t.Fatalf("image store = %q, want %q", got, options.ImageStore)
	}
	if !lease.store.TransientStore() {
		t.Fatal("transient store option was not forwarded")
	}

	conflict := options
	conflict.TransientStore = false
	if _, err := acquireStore(conflict); err == nil {
		t.Fatal("same open roots accepted conflicting transient-store option")
	}
}

func TestNormalizeStoreOptionsCanonicalizesImageStore(t *testing.T) {
	root := t.TempDir()
	real := root + "/real"
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := root + "/alias"
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	options := StoreOptions{RunRoot: root + "/run", GraphRoot: root + "/graph", ImageStore: alias + "/images", GraphDriverName: "vfs"}
	normalized, err := NormalizeStoreOptions(options)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.ImageStore != real+"/images" {
		t.Fatalf("canonical image store = %q, want %q", normalized.ImageStore, real+"/images")
	}
}

func TestStoreLeaseFreesContainersStorageCacheAfterShutdown(t *testing.T) {
	options := testStoreOptions(t)
	first, err := acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	firstStore := first.store
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedOptions := options
	reopenedOptions.GraphDriverOptions = []string{"vfs.ignore_chown_errors=true"}
	second, err := acquireStore(reopenedOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Errorf("close reopened store: %v", err)
		}
	}()
	if second.store == firstStore {
		t.Fatal("containers/storage returned the store that had already shut down")
	}
	if got := second.store.GraphOptions(); !slices.Equal(got, reopenedOptions.GraphDriverOptions) {
		t.Fatalf("reopened graph options = %v, want %v", got, reopenedOptions.GraphDriverOptions)
	}
}

func testStoreOptions(t *testing.T) StoreOptions {
	t.Helper()
	root := t.TempDir()
	return StoreOptions{
		RunRoot:         root + "/run",
		GraphRoot:       root + "/graph",
		GraphDriverName: "vfs",
	}
}

func entryUsers(entry *buildStoreEntry) int {
	if entry == nil {
		return 0
	}
	return entry.users
}
