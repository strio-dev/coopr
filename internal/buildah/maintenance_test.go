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
	"strings"
	"testing"
	"time"

	"coopr/internal/storeactivity"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage"
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

func TestMaintenanceUsageAndReclamationUseNativeUniqueAccounting(t *testing.T) {
	options := StoreOptions{RunRoot: filepath.Join(t.TempDir(), "run"), GraphRoot: filepath.Join(t.TempDir(), "graph"), GraphDriverName: "vfs"}
	var wantBytes, wantReclaimable int64
	if err := WithStore(options, func(backend storage.Store) error {
		createMaintenanceTestImage(t, backend, "", "localhost/one:latest", "localhost/two:latest")
		createMaintenanceTestImage(t, backend, "")
		runtime, err := libimage.RuntimeFromStore(backend, nil)
		if err != nil {
			return err
		}
		usage, total, err := runtime.DiskUsage(context.Background())
		if err != nil {
			return err
		}
		wantBytes = total
		seen := map[string]bool{}
		for _, entry := range usage {
			if !seen[entry.ID] {
				seen[entry.ID] = true
				wantReclaimable += entry.UniqueSize
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := maintainStore(context.Background(), StoreMaintenanceRequest{Store: options, Mode: StoreMaintenanceDF})
	if err != nil {
		t.Fatal(err)
	}
	if before.Images != 2 || before.ActiveImages != 0 || before.Bytes != wantBytes || before.ReclaimableBytes != wantReclaimable {
		t.Fatalf("native usage deduplication: got=%+v wantBytes=%d wantReclaimable=%d", before, wantBytes, wantReclaimable)
	}
	removed, err := maintainStore(context.Background(), StoreMaintenanceRequest{Store: options, Mode: StoreMaintenancePrune})
	if err != nil {
		t.Fatal(err)
	}
	after, err := maintainStore(context.Background(), StoreMaintenanceRequest{Store: options, Mode: StoreMaintenanceDF})
	if err != nil {
		t.Fatal(err)
	}
	if removed.RemovedImages != 1 || len(removed.RemovalReports) != 1 || !removed.RemovalReports[0].Removed || removed.ReclaimedBytes != before.Bytes-after.Bytes || removed.ReclaimedBytes <= 0 {
		t.Fatalf("native reclaimed bytes: before=%+v removed=%+v after=%+v", before, removed, after)
	}
}

func TestPruneDoesNotRequireHealthyUsageMetadata(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		t.Run(fmt.Sprintf("dry-run=%t", dryRun), func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			options := StoreOptions{RunRoot: filepath.Join(t.TempDir(), "run"), GraphRoot: filepath.Join(t.TempDir(), "graph"), GraphDriverName: "vfs"}
			if err := WithStore(options, func(backend storage.Store) error {
				createMaintenanceTestImage(t, backend, "")
				id := createMaintenanceTestImage(t, backend, "")
				return backend.SetImageBigData(id, storage.ImageDigestBigDataKey, []byte("broken manifest"), func(data []byte) (digest.Digest, error) { return digest.FromBytes(data), nil })
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := maintainStore(context.Background(), StoreMaintenanceRequest{Store: options, Mode: StoreMaintenanceDF}); err == nil {
				t.Fatal("df concealed corrupted metadata")
			}
			result, err := maintainStore(context.Background(), StoreMaintenanceRequest{Store: options, Mode: StoreMaintenancePrune, All: true, DryRun: dryRun})
			if err != nil {
				t.Fatalf("usage metadata blocked native prune: %v", err)
			}
			if result.UsageError == "" || result.ReclaimedBytesKnown {
				t.Fatalf("fabricated complete usage: %+v", result)
			}
			if dryRun && result.PrunableImages != 2 {
				t.Fatalf("missing preview candidates: %+v", result)
			}
			if !dryRun && (result.RemovedImages != 2 || len(result.RemovalReports) != 2) {
				t.Fatalf("missing native removals: %+v", result)
			}
		})
	}
}

func TestCacheLayerUsageReportsUnknownMetadataAndMappedRoots(t *testing.T) {
	cacheName := instructionCacheName(digest.FromString("usage"))
	for _, test := range []struct {
		name   string
		images []storage.Image
		layers []storage.Layer
		known  bool
		bytes  int64
	}{
		{"empty", nil, nil, true, 0},
		{"unknown size", []storage.Image{{TopLayer: "a", Names: []string{cacheName}}}, []storage.Layer{{ID: "a", UncompressedSize: -1}}, false, 0},
		{"missing parent", []storage.Image{{TopLayer: "a", Names: []string{cacheName}}}, []storage.Layer{{ID: "a", Parent: "missing", UncompressedSize: 5}}, false, 0},
		{"mapped root deduplicates ancestry", []storage.Image{{TopLayer: "a", MappedTopLayers: []string{"b"}, Names: []string{cacheName}}}, []storage.Layer{{ID: "a", UncompressedSize: 5}, {ID: "b", Parent: "a", UncompressedSize: 7}}, true, 12},
		{"missing mapped root", []storage.Image{{TopLayer: "a", MappedTopLayers: []string{"missing"}, Names: []string{cacheName}}}, []storage.Layer{{ID: "a", UncompressedSize: 5}}, false, 0},
		{"unknown mapped size", []storage.Image{{MappedTopLayers: []string{"a"}, Names: []string{cacheName}}}, []storage.Layer{{ID: "a", UncompressedSize: -1}}, false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := storeUsage(test.images, test.layers, nil)
			if got.CacheBytesKnown != test.known || (test.known && got.CacheBytes != test.bytes) {
				t.Fatalf("cache layer usage=%+v want known=%t bytes=%d", got, test.known, test.bytes)
			}
		})
	}
}

func TestMaintenanceWorkerResponsePreservesPartialResultsAndErrors(t *testing.T) {
	resultPath := filepath.Join(t.TempDir(), "result.json")
	partial := StoreMaintenanceResult{RemovedImages: 1, RemovalReports: []*libimage.RemoveImageReport{{ID: "deleted-image", Removed: true}}, Error: "native prune failed after one removal"}
	if err := writeWorkerJSON(resultPath, partial); err != nil {
		t.Fatal(err)
	}
	processErr := errors.New("worker exited with status 1")
	result, err := readMaintenanceResult(context.Background(), resultPath, processErr)
	if err == nil || err.Error() != partial.Error || result.RemovedImages != 1 || len(result.RemovalReports) != 1 || result.RemovalReports[0].ID != "deleted-image" || result.ReclaimedBytesKnown {
		t.Fatalf("partial worker response lost: result=%+v err=%v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result, err := readMaintenanceResult(ctx, resultPath, processErr); !errors.Is(err, context.Canceled) || result.RemovedImages != 1 || !strings.Contains(err.Error(), partial.Error) {
		t.Fatalf("cancellation lost partial result or error: result=%+v err=%v", result, err)
	}
	if _, err := readMaintenanceResult(context.Background(), filepath.Join(t.TempDir(), "missing"), processErr); !errors.Is(err, processErr) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("startup/read failure masked: %v", err)
	}
	if err := os.WriteFile(resultPath, []byte("invalid response"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readMaintenanceResult(context.Background(), resultPath, processErr); !errors.Is(err, processErr) || !strings.Contains(err.Error(), "decode build worker message") {
		t.Fatalf("malformed response masked process failure: %v", err)
	}
	if err := writeWorkerJSON(resultPath, StoreMaintenanceResult{}); err != nil {
		t.Fatal(err)
	}
	if _, err := readMaintenanceResult(context.Background(), resultPath, processErr); !errors.Is(err, processErr) {
		t.Fatalf("unreported process failure masked: %v", err)
	}
}

func TestPruneNativeFilters(t *testing.T) {
	for _, test := range []struct {
		name     string
		filters  []string
		expected int
	}{
		{"matching-label", []string{"label=fixture"}, 2},
		{"missing-label", []string{"label=absent"}, 0},
		{"negated-label", []string{"label!=fixture"}, 0},
		{"before-epoch", []string{"until=1970-01-01T00:00:00Z"}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := StoreOptions{RunRoot: filepath.Join(t.TempDir(), "run"), GraphRoot: filepath.Join(t.TempDir(), "graph"), GraphDriverName: "vfs"}
			lease, err := acquireStore(options)
			if err != nil {
				t.Fatal(err)
			}
			createMaintenanceTestImage(t, lease.store, "")
			createMaintenanceTestImage(t, lease.store, "")
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			result, err := maintainStore(context.Background(), StoreMaintenanceRequest{Store: options, Mode: StoreMaintenancePrune, DryRun: true, Filters: test.filters})
			if err != nil || result.PrunableImages != test.expected {
				t.Fatalf("filtered prune %+v: %v", result, err)
			}
		})
	}
}
