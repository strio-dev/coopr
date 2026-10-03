package buildah

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"coopr/internal/storeactivity"
	"github.com/opencontainers/go-digest"
	"go.podman.io/storage"
)

func TestCachePruneRemovesAliasesButPreservesProtectedImages(t *testing.T) {
	options := StoreOptions{RunRoot: filepath.Join(t.TempDir(), "run"), GraphRoot: filepath.Join(t.TempDir(), "graph"), GraphDriverName: "vfs"}
	lease, err := acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	protected := createInstructionCacheTestImage(t, lease.store, instructionCacheName(digest.FromString("protected")))
	dangling := createInstructionCacheTestImage(t, lease.store, instructionCacheName(digest.FromString("dangling")))
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := maintainStore(context.Background(), StoreMaintenanceRequest{
		Store: options, Mode: StoreMaintenanceCachePrune, ProtectedImageIDs: []string{protected},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedCacheAliases != 2 || result.RemovedImages != 1 {
		t.Fatalf("cache prune result = %+v", result)
	}
	lease, err = acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	if _, err := lease.store.Image(protected); err != nil {
		t.Fatalf("protected image removed: %v", err)
	}
	if image, err := lease.store.Image(protected); err != nil || len(image.Names) != 0 {
		t.Fatalf("protected cache aliases remain: %+v, %v", image, err)
	}
	if _, err := lease.store.Image(dangling); !errors.Is(err, storage.ErrImageUnknown) {
		t.Fatalf("dangling image lookup = %v", err)
	}
}

func TestSharedStorePruneNeverDeletesUnownedImages(t *testing.T) {
	options := StoreOptions{RunRoot: filepath.Join(t.TempDir(), "run"), GraphRoot: filepath.Join(t.TempDir(), "graph"), GraphDriverName: "vfs", Shared: true}
	lease, err := acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	unowned := createInstructionCacheTestImage(t, lease.store, "")
	cache := createInstructionCacheTestImage(t, lease.store, instructionCacheName(digest.FromString("shared")))
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := maintainStore(context.Background(), StoreMaintenanceRequest{Store: options, Mode: StoreMaintenancePrune})
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedImages != 0 {
		t.Fatalf("shared prune removed images: %+v", result)
	}
	result, err = maintainStore(context.Background(), StoreMaintenanceRequest{Store: options, Mode: StoreMaintenanceCachePrune})
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedImages != 0 || result.RemovedCacheAliases != 1 {
		t.Fatalf("shared cache prune = %+v", result)
	}
	lease, err = acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	for _, id := range []string{unowned, cache} {
		if _, err := lease.store.Image(id); err != nil {
			t.Fatalf("shared image %s removed: %v", id, err)
		}
	}
}

func TestMaintainStoreSupervisedUsesIsolatedStore(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live maintenance worker coverage")
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
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live maintenance worker coverage")
	}
	root := t.TempDir()
	activity, err := storeactivity.AcquireShared(context.Background(), root)
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
	exclusive, err := storeactivity.AcquireExclusive(context.Background(), root)
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
