package build

import (
	"context"
	"errors"
	"path/filepath"

	"coopr/internal/buildah"
	"coopr/internal/imagestore"
	"coopr/internal/oci"
	"github.com/containerd/platforms"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
)

// Every fixture selecting a non-default graph gets its own runtime directory.
func nativeBuildTestStore(root string) buildah.StoreOptions {
	return buildah.StoreOptions{GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"), GraphDriverName: "vfs", GraphDriverOptions: []string{"vfs.ignore_chown_errors=true"}}
}

func testStoredImageIndex(ctx context.Context, store buildah.StoreOptions, reference string) (root v1.Descriptor, data []byte, selections map[string]oci.StoredSelection, found bool, err error) {
	err = buildah.WithStore(store, func(backend storage.Store) error {
		root, data, selections, err = oci.StoredImageSelections(ctx, backend, reference)
		return err
	})
	if errors.Is(err, storage.ErrImageUnknown) {
		return root, nil, nil, false, nil
	}
	return root, data, selections, len(data) != 0, err
}

func testStoredImageSelection(ctx context.Context, store buildah.StoreOptions, reference string, platform v1.Platform) (selection oci.StoredSelection, found bool, err error) {
	err = buildah.WithStore(store, func(backend storage.Store) error {
		resolved, err := oci.ResolveStoredImage(ctx, backend, reference, platform)
		if err != nil {
			return err
		}
		selection = oci.StoredSelection{Root: resolved.Root, Manifest: resolved.Selected, ImageID: resolved.StorageImageID, ConfigData: resolved.ConfigData}
		found = true
		return nil
	})
	if errors.Is(err, storage.ErrImageUnknown) || errors.Is(err, oci.ErrStoredPlatformUnavailable) {
		return selection, false, nil
	}
	return selection, found, err
}

func testStoredSoleImage(ctx context.Context, store buildah.StoreOptions, reference string) (selection oci.StoredSelection, platform v1.Platform, found bool, err error) {
	err = buildah.WithStore(store, func(backend storage.Store) error {
		_, _, selections, err := oci.StoredImageSelections(ctx, backend, reference)
		if err != nil {
			return err
		}
		if len(selections) != 1 {
			return nil
		}
		for key, value := range selections {
			platform, err = platforms.Parse(key)
			if err != nil {
				return err
			}
			selection, found = value, true
		}
		return nil
	})
	return selection, platform, found, err
}

func testNativeNames(store buildah.StoreOptions) (names []string, err error) {
	err = buildah.WithStore(store, func(backend storage.Store) error {
		names, err = imagestore.FromStore(backend).Names()
		return err
	})
	return names, err
}
