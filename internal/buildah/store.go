package buildah

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"

	"go.podman.io/storage"
)

// containers/storage caches stores process-wide. Keep ownership process-wide
// too, so one build cannot shut down a store while another build is using it.
var buildStores = struct {
	sync.Mutex
	entries map[string]*buildStoreEntry
}{entries: make(map[string]*buildStoreEntry)}

type buildStoreEntry struct {
	store   storage.Store
	options StoreOptions
	users   int
}

type storeLease struct {
	key      string
	store    storage.Store
	released bool
}

func acquireStore(options StoreOptions) (*storeLease, error) {
	key, normalized, err := storeKey(options)
	if err != nil {
		return nil, err
	}

	buildStores.Lock()
	defer buildStores.Unlock()
	if entry := buildStores.entries[key]; entry != nil {
		if !reflect.DeepEqual(NativeStoreOptions(entry.options), NativeStoreOptions(normalized)) {
			return nil, fmt.Errorf("containers/storage roots are already open with different storage options")
		}
		entry.users++
		return &storeLease{key: key, store: entry.store}, nil
	}

	store, err := storage.GetStore(NativeStoreOptions(normalized))
	if err != nil {
		return nil, err
	}
	buildStores.entries[key] = &buildStoreEntry{store: store, options: normalized, users: 1}
	return &storeLease{key: key, store: store}, nil
}

// WithStore lends the process-shared native store for bounded metadata work.
func WithStore(options StoreOptions, use func(storage.Store) error) error {
	lease, err := acquireStore(options)
	if err != nil {
		return err
	}
	return errors.Join(use(lease.store), lease.Close())
}

func (lease *storeLease) Close() error {
	if lease == nil {
		return nil
	}
	buildStores.Lock()
	defer buildStores.Unlock()
	if lease.released {
		return nil
	}
	lease.released = true

	entry := buildStores.entries[lease.key]
	if entry == nil || entry.store != lease.store {
		return errors.New("release unowned containers/storage lease")
	}
	entry.users--
	if entry.users > 0 {
		return nil
	}
	_, err := entry.store.Shutdown(false)
	if errors.Is(err, storage.ErrLayerUsedByContainer) {
		// The store is still live. Retain its options so another acquisition
		// cannot bypass our compatibility check through storage's global cache.
		return nil
	}
	if err != nil {
		return err
	}
	entry.store.Free()
	delete(buildStores.entries, lease.key)
	return nil
}

func storeKey(options StoreOptions) (string, StoreOptions, error) {
	runRoot, err := canonicalStorePath(options.RunRoot)
	if err != nil {
		return "", StoreOptions{}, fmt.Errorf("resolve containers/storage run root: %w", err)
	}
	graphRoot, err := canonicalStorePath(options.GraphRoot)
	if err != nil {
		return "", StoreOptions{}, fmt.Errorf("resolve containers/storage graph root: %w", err)
	}
	normalized := options
	normalized.RunRoot = runRoot
	normalized.GraphRoot = graphRoot
	if options.ImageStore != "" {
		imageStore, err := canonicalStorePath(options.ImageStore)
		if err != nil {
			return "", StoreOptions{}, fmt.Errorf("resolve containers/storage image store: %w", err)
		}
		normalized.ImageStore = imageStore
		if imageStore == graphRoot {
			return "", StoreOptions{}, errors.New("containers/storage image store and graph root must differ")
		}
	}
	normalized.GraphDriverOptions = append([]string(nil), options.GraphDriverOptions...)
	normalized.Native = NativeStoreOptions(normalized)

	var key strings.Builder
	for _, value := range []string{runRoot, graphRoot} {
		key.WriteString(strconv.Itoa(len(value)))
		key.WriteByte(':')
		key.WriteString(value)
	}
	return key.String(), normalized, nil
}

// NormalizeStoreOptions returns the canonical paths used to identify one
// containers/storage instance without opening it.
func NormalizeStoreOptions(options StoreOptions) (StoreOptions, error) {
	if err := validateStoreOptions(options); err != nil {
		return StoreOptions{}, err
	}
	_, normalized, err := storeKey(options)
	return normalized, err
}

// NativeStoreOptions maps Coopr's serialized store selection to the exact
// containers/storage API options used by builders, importers, and readers.
func NativeStoreOptions(options StoreOptions) storage.StoreOptions {
	native := options.Native
	native.RunRoot, native.GraphRoot, native.ImageStore = options.RunRoot, options.GraphRoot, options.ImageStore
	native.GraphDriverName = options.GraphDriverName
	native.GraphDriverOptions = slices.Clone(options.GraphDriverOptions)
	native.TransientStore = options.TransientStore
	return native
}

// StoreIdentity returns a stable identity for Coopr metadata associated with
// one exact containers/storage configuration. It never includes payload data.
func StoreIdentity(options StoreOptions) (string, error) {
	normalized, err := NormalizeStoreOptions(options)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(NativeStoreOptions(normalized))
	if err != nil {
		return "", fmt.Errorf("encode containers/storage identity: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
