package build

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"coopr/internal/buildah"
	"coopr/internal/imagestore"
	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage/manifests"
	"go.podman.io/storage"
)

// appendManifest reads and publishes lists under native locks. Children already
// reside in containers/storage; updating a list copies no filesystem layers.
func appendManifest(ctx context.Context, name, format string, store buildah.StoreOptions, selections map[string]oci.StoredSelection) (root v1.Descriptor, data []byte, result map[string]oci.StoredSelection, imageID string, err error) {
	name, err = imagestore.NormalizeTag(name)
	if err != nil {
		return root, nil, nil, "", err
	}
	err = buildah.WithStore(store, func(backend storage.Store) error {
		// Serialize Coopr appends even before a named list exists. Existing
		// lists also use the native per-image lock used by Podman list updates.
		lock, err := backend.GetDigestLock(digest.FromString("coopr manifest append\x00" + name))
		if err != nil {
			return err
		}
		lock.Lock()
		defer lock.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		var previous map[string]oci.StoredSelection
		var previousIndex *v1.Index
		var existingID string
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			matched, err := oci.StoredImageName(backend, name)
			if errors.Is(err, storage.ErrImageUnknown) {
				break
			}
			if err != nil {
				return err
			}
			image, err := backend.Image(matched)
			if err != nil {
				return err
			}
			imageLock, err := manifests.LockerForImage(backend, image.ID)
			if err != nil {
				return err
			}
			imageLock.Lock()
			current, err := backend.Image(name)
			if err != nil || current.ID != image.ID {
				imageLock.Unlock()
				if err != nil && !errors.Is(err, storage.ErrImageUnknown) {
					return err
				}
				continue
			}
			defer imageLock.Unlock()
			_, indexData, existing, err := oci.StoredImageSelections(ctx, backend, name)
			if err != nil {
				return err
			}
			if len(indexData) == 0 {
				return fmt.Errorf("manifest name %s already identifies a single image", name)
			}
			var index v1.Index
			if err := json.Unmarshal(indexData, &index); err != nil {
				return err
			}
			if len(index.Manifests) != len(existing) {
				return fmt.Errorf("manifest %s has unavailable instances; pull the complete list before appending", name)
			}
			defaultData, err := backend.ImageBigData(image.ID, storage.ImageDigestBigDataKey)
			if err != nil {
				return err
			}
			if !bytes.Equal(defaultData, indexData) {
				return fmt.Errorf("manifest name %s identifies a pulled image, not a native manifest list", name)
			}
			if _, _, err := manifests.LoadFromImage(backend, image.ID); err != nil {
				return err
			}
			existingID = image.ID
			previous = existing
			previousIndex = &index
			break
		}
		result = make(map[string]oci.StoredSelection, len(previous)+len(selections))
		for key, selection := range previous {
			result[key] = selection
		}
		for key, selection := range selections {
			result[key] = selection
		}
		keys := make([]string, 0, len(result))
		for key := range result {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		combined := make([]oci.ImageVariant, 0, len(result))
		imageIDs := make(map[digest.Digest]string, len(result))
		for _, key := range keys {
			platform, err := result[key].Platform()
			if err != nil {
				return err
			}
			selection := result[key]
			if selection.Manifest.Platform == nil {
				selection.Manifest.Platform = &platform
			}
			result[key] = selection
			combined = append(combined, oci.ImageVariant{Manifest: selection.Manifest, Platform: platform})
			imageIDs[selection.Manifest.Digest] = selection.ImageID
		}
		root, data, err = oci.RetainedImageIndexDescriptor(combined, format)
		if err != nil {
			return err
		}
		if previousIndex != nil {
			var combinedIndex v1.Index
			if err := json.Unmarshal(data, &combinedIndex); err != nil {
				return err
			}
			combinedIndex.Annotations = previousIndex.Annotations
			combinedIndex.Subject = previousIndex.Subject
			combinedIndex.ArtifactType = previousIndex.ArtifactType
			data, err = json.Marshal(combinedIndex)
			if err != nil {
				return err
			}
			root = oci.Descriptor(combinedIndex.MediaType, data)
		}
		for key, selection := range result {
			selection.Root = root
			result[key] = selection
		}
		if existingID != "" {
			imageID, err = imagestore.FromStore(backend).UpdateStoredIndex(ctx, existingID, root, data, imageIDs, name)
		} else {
			imageID, err = imagestore.FromStore(backend).WriteStoredIndex(ctx, root, data, imageIDs, name)
		}
		return err
	})
	return root, data, result, imageID, err
}
