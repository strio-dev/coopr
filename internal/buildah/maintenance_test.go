package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"coopr/internal/storeactivity"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage/manifests"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/unshare"
)

func TestPruneRemovesUnusedImagesAndPreservesNames(t *testing.T) {
	options := StoreOptions{RunRoot: filepath.Join(t.TempDir(), "run"), GraphRoot: filepath.Join(t.TempDir(), "graph"), GraphDriverName: "vfs"}
	lease, err := acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	cacheName := instructionCacheName(digest.FromString("tagged"))
	tagged := createMaintenanceTestImage(t, lease.store, "", "localhost/app:latest", cacheName)
	cache := createMaintenanceTestImage(t, lease.store, "", instructionCacheName(digest.FromString("unused")))
	foreign := createMaintenanceTestImage(t, lease.store, "")
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := maintainStore(context.Background(), StoreMaintenanceRequest{Store: options, Mode: StoreMaintenancePrune})
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedImages != 2 {
		t.Fatalf("prune did not remove unused images: %+v", result)
	}
	lease, err = acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	for _, id := range []string{cache, foreign} {
		if _, err := lease.store.Image(id); !errors.Is(err, storage.ErrImageUnknown) {
			t.Fatalf("unused image %s remains: %v", id, err)
		}
	}
	for _, name := range []string{"localhost/app:latest", cacheName} {
		image, err := lease.store.Image(name)
		if err != nil || image.ID != tagged {
			t.Fatalf("protected image name %s changed: %+v, %v", name, image, err)
		}
	}
}

func TestPrunePreservesManifestMembers(t *testing.T) {
	options := StoreOptions{RunRoot: filepath.Join(t.TempDir(), "run"), GraphRoot: filepath.Join(t.TempDir(), "graph"), GraphDriverName: "vfs"}
	lease, err := acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	name := instructionCacheName(digest.FromString("manifest-member"))
	id := createMaintenanceTestImage(t, lease.store, "", name)
	ref, err := imagestorage.Transport.NewStoreReference(lease.store, nil, id)
	if err != nil {
		t.Fatal(err)
	}
	list := manifests.Create()
	if _, err := list.Add(context.Background(), nil, ref, false); err != nil {
		t.Fatal(err)
	}
	if _, err := list.SaveToImage(lease.store, "", []string{"localhost/list:latest"}, v1.MediaTypeImageIndex); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := maintainStore(context.Background(), StoreMaintenanceRequest{Store: options, Mode: StoreMaintenancePrune})
	if err != nil || result.RemovedImages != 0 {
		t.Fatalf("manifest member pruned: %+v, %v", result, err)
	}
	if err := WithStore(options, func(store storage.Store) error {
		_, err := store.Image(name)
		return err
	}); err != nil {
		t.Fatalf("manifest member cache alias lost: %v", err)
	}
}

func TestPruneMountCacheOptions(t *testing.T) {
	for _, test := range []struct {
		name                    string
		all, buildCache, dryRun bool
	}{
		{name: "default"}, {name: "default-preview", dryRun: true},
		{name: "mounts", buildCache: true}, {name: "mounts-preview", buildCache: true, dryRun: true},
		{name: "all", all: true}, {name: "all-preview", all: true, dryRun: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("TMPDIR", root)
			cacheDir := filepath.Join(root, fmt.Sprintf("buildah-cache-%d", unshare.GetRootlessUID()))
			if err := os.MkdirAll(cacheDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cacheDir, "payload"), []byte("cache"), 0o600); err != nil {
				t.Fatal(err)
			}
			options := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
			lease, err := acquireStore(options)
			if err != nil {
				t.Fatal(err)
			}
			id := createMaintenanceTestImage(t, lease.store, "", "localhost/tagged:latest")
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			result, err := maintainStore(context.Background(), StoreMaintenanceRequest{Store: options, Mode: StoreMaintenancePrune, All: test.all, BuildCache: test.buildCache, DryRun: test.dryRun})
			if err != nil {
				t.Fatal(err)
			}
			_, err = os.Stat(cacheDir)
			if wantRemoved := (test.all || test.buildCache) && !test.dryRun; errors.Is(err, os.ErrNotExist) != wantRemoved {
				t.Fatalf("mount cache removal=%v want=%t", err, wantRemoved)
			}
			if err := WithStore(options, func(store storage.Store) error {
				_, err := store.Image(id)
				if wantRemoved := test.all && !test.dryRun; errors.Is(err, storage.ErrImageUnknown) != wantRemoved {
					t.Fatalf("tagged image removal=%v want=%t", err, wantRemoved)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if test.dryRun && test.all && (result.PrunableImages != 1 || result.RemovedImages != 0) {
				t.Fatalf("all preview changed images or lost candidates: %+v", result)
			}
		})
	}
}

func TestPruneDryRunPreservesCacheAliasesAndImages(t *testing.T) {
	options := StoreOptions{RunRoot: filepath.Join(t.TempDir(), "run"), GraphRoot: filepath.Join(t.TempDir(), "graph"), GraphDriverName: "vfs"}
	lease, err := acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	name := instructionCacheName(digest.FromString("preview"))
	id := createMaintenanceTestImage(t, lease.store, "", name)
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if result, err := maintainStore(context.Background(), StoreMaintenanceRequest{Store: options, Mode: StoreMaintenancePrune, DryRun: true}); err != nil || result.PrunableImages != 1 || result.RemovedImages != 0 {
		t.Fatalf("preview=%+v error=%v", result, err)
	}
	lease, err = acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	image, err := lease.store.Image(name)
	if err != nil || image.ID != id {
		t.Fatalf("dry run changed the cache alias: %+v, %v", image, err)
	}
}

func TestPruneRetainsNeededLayersAndContainers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping native layer and container pruning in short mode")
	}
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprintf("all=%t", all), func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("TMPDIR", root)
			options := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
			lease, err := acquireStore(options)
			if err != nil {
				t.Fatal(err)
			}
			layer := func(parent string) string {
				t.Helper()
				payload := []byte(t.TempDir())
				var data bytes.Buffer
				writer := tar.NewWriter(&data)
				if err := writer.WriteHeader(&tar.Header{Name: "payload", Mode: 0o600, Size: int64(len(payload))}); err != nil {
					t.Fatal(err)
				}
				if _, err := writer.Write(payload); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				created, _, err := lease.store.PutLayer("", parent, nil, "", false, nil, &data)
				if err != nil {
					t.Fatal(err)
				}
				return created.ID
			}
			parentLayer := layer("")
			parentName := instructionCacheName(digest.FromString("needed-parent"))
			parent := createMaintenanceTestImage(t, lease.store, parentLayer, parentName)
			child := createMaintenanceTestImage(t, lease.store, layer(parentLayer), "localhost/child:latest")
			unusedLayer := layer("")
			unused := createMaintenanceTestImage(t, lease.store, unusedLayer, instructionCacheName(digest.FromString("orphan")))
			inUseName := instructionCacheName(digest.FromString("container-base"))
			inUse := createMaintenanceTestImage(t, lease.store, layer(""), inUseName)
			container, err := lease.store.CreateContainer("", nil, inUse, "", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			result, err := maintainStore(context.Background(), StoreMaintenanceRequest{Store: options, Mode: StoreMaintenancePrune, All: all})
			if err != nil || result.RemovedImages == 0 {
				t.Fatalf("prune=%+v error=%v", result, err)
			}
			if err := WithStore(options, func(store storage.Store) error {
				for _, id := range []string{parent, child, unused} {
					_, err := store.Image(id)
					if wantRemoved := all || id == unused; errors.Is(err, storage.ErrImageUnknown) != wantRemoved {
						t.Fatalf("image %s removal=%v want=%t", id, err, wantRemoved)
					}
				}
				if _, err := store.Layer(unusedLayer); !errors.Is(err, storage.ErrLayerUnknown) {
					t.Fatalf("unused image layer remains: %v", err)
				}
				if _, err := store.Image(inUseName); err != nil {
					t.Fatalf("in-use cache name lost: %v", err)
				}
				if !all {
					if _, err := store.Image(parentName); err != nil {
						t.Fatalf("needed parent cache name lost: %v", err)
					}
				}
				_, err := store.Container(container.ID)
				return err
			}); err != nil {
				t.Fatalf("prune changed the protected container: %v", err)
			}
		})
	}
}

func TestSupervisedPruneCleansBuildWorkerMounts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping supervised cache-mount cleanup in short mode")
	}
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	options := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	workerTemp, err := prepareWorkerTemp(options.GraphRoot)
	if err != nil {
		t.Fatal(err)
	}
	cacheDirName := fmt.Sprintf("buildah-cache-%d", unshare.GetRootlessUID())
	workerCache := filepath.Join(workerTemp, cacheDirName)
	hostCache := filepath.Join(root, cacheDirName)
	for _, directory := range []string{workerCache, hostCache} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "payload"), []byte("cache"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	request := StoreMaintenanceRequest{Store: options, Mode: StoreMaintenancePrune, BuildCache: true, DryRun: true}
	if _, err := MaintainStoreSupervised(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workerCache); err != nil {
		t.Fatalf("preview removed build-worker mounts: %v", err)
	}
	request.DryRun = false
	if _, err := MaintainStoreSupervised(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workerCache); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("supervised prune retained build-worker mounts: %v", err)
	}
	if _, err := os.Stat(hostCache); err != nil {
		t.Fatalf("supervised prune touched unrelated host mounts: %v", err)
	}
}

// Valid OCI metadata lets upstream libimage inspect native fixture images.
func createMaintenanceTestImage(t *testing.T, store storage.Store, topLayer string, names ...string) string {
	t.Helper()
	var diffIDs []digest.Digest
	var layers []v1.Descriptor
	for layerID := topLayer; layerID != ""; {
		layer, err := store.Layer(layerID)
		if err != nil {
			t.Fatal(err)
		}
		diffIDs = append([]digest.Digest{layer.UncompressedDigest}, diffIDs...)
		layers = append([]v1.Descriptor{{MediaType: v1.MediaTypeImageLayer, Digest: layer.UncompressedDigest, Size: layer.UncompressedSize}}, layers...)
		layerID = layer.Parent
	}
	config := v1.Image{
		Platform: v1.Platform{OS: "linux", Architecture: "amd64"},
		RootFS:   v1.RootFS{Type: "layers", DiffIDs: diffIDs},
		Config:   v1.ImageConfig{Labels: map[string]string{"fixture": t.TempDir()}},
	}
	for range diffIDs {
		config.History = append(config.History, v1.History{CreatedBy: "fixture"})
	}
	configData, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configDigest := digest.FromBytes(configData)
	manifest := v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest,
		Config: v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: configDigest, Size: int64(len(configData))},
		Layers: layers,
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	image, err := store.CreateImage(configDigest.Encoded(), names, topLayer, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetImageBigData(image.ID, configDigest.String(), configData, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetImageBigData(image.ID, storage.ImageDigestBigDataKey, manifestData, func(data []byte) (digest.Digest, error) { return digest.FromBytes(data), nil }); err != nil {
		t.Fatal(err)
	}
	return image.ID
}

func TestMaintainStoreSupervisedUsesIsolatedStore(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live maintenance worker coverage in short mode")
	}
	root := t.TempDir()
	result, err := MaintainStoreSupervised(context.Background(), StoreMaintenanceRequest{
		Store: StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
		Mode:  StoreMaintenanceDF,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Images != 0 || result.Containers != 0 || result.Layers != 0 {
		t.Fatalf("empty supervised store usage = %+v", result)
	}
}

func TestMaintainStoreSupervisedWaitsForActiveBuildLease(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live maintenance worker coverage in short mode")
	}
	root := t.TempDir()
	activity, err := storeactivity.AcquireShared(context.Background(), filepath.Join(root, "graph"), filepath.Join(root, "run"))
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, maintainErr := MaintainStoreSupervised(context.Background(), StoreMaintenanceRequest{
			Store: StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
			Mode:  StoreMaintenancePrune, DryRun: true,
		})
		finished <- maintainErr
	}()
	select {
	case err := <-finished:
		t.Fatalf("maintenance completed during active build lease: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	if err := activity.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatalf("maintenance after active build lease: %v", err)
	}
	exclusive, err := storeactivity.AcquireExclusive(context.Background(), filepath.Join(root, "graph"), filepath.Join(root, "run"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = MaintainStoreSupervised(context.Background(), StoreMaintenanceRequest{
		Store: StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
		Mode:  StoreMaintenancePrune, DryRun: true, ActivityLeaseHeld: true,
	})
	if closeErr := exclusive.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("maintenance under caller-held exclusive lease: %v", err)
	}
}
