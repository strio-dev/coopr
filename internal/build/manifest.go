package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/imagestore"
	"github.com/strio-dev/coopr/internal/oci"
	"go.podman.io/common/libimage/manifests"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/storage"
	"oras.land/oras-go/v2/content"
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
			if _, _, err := manifests.LoadFromImage(backend, image.ID); err != nil {
				return fmt.Errorf("manifest name %s is not a native manifest list: %w", name, err)
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
		keys := make([]string, 0, len(selections))
		for key := range selections {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		native := manifests.Create()
		mediaType := v1.MediaTypeImageIndex
		switch format {
		case "", "oci":
		case "docker":
			mediaType = manifest.DockerV2ListMediaType
		default:
			return fmt.Errorf("unsupported image index format %q (want oci or docker)", format)
		}
		if len(result) == 0 {
			return errors.New("image index requires at least one variant")
		}
		// Native mutation emits the requested list
		// format. Existing descriptor occurrences are retained separately: the
		// resident selection map is a lookup, not an ordered descriptor list.
		for _, key := range keys {
			selection := selections[key]
			platform, err := selection.Platform()
			if err != nil {
				return err
			}
			if platform.OS != "linux" || platform.Architecture == "" {
				return fmt.Errorf("unsupported platform %q/%q", platform.OS, platform.Architecture)
			}
			if selection.Manifest.Platform == nil {
				selection.Manifest.Platform = &platform
			}
			result[key] = selection
			if err := native.AddInstance(selection.Manifest.Digest, selection.Manifest.Size, selection.Manifest.MediaType, platform.OS, platform.Architecture, platform.OSVersion, platform.OSFeatures, platform.Variant, nil, nil); err != nil {
				return err
			}
		}
		data, err = native.Serialize(mediaType)
		if err != nil {
			return err
		}
		var combinedIndex v1.Index
		if err := json.Unmarshal(data, &combinedIndex); err != nil {
			return err
		}
		combinedIndex.Manifests = nil
		if previousIndex != nil {
			for _, member := range previousIndex.Manifests {
				replaced := false
				for key := range selections {
					if prior, ok := previous[key]; ok {
						selected := prior.Manifest
						if prior.SourceManifest != nil {
							selected = *prior.SourceManifest
						}
						// Replace the selected index member, preserving distinct runnable
						// siblings even when they share its resident storage image.
						if content.Equal(selected, member) {
							replaced = true
							break
						}
					}
				}
				if !replaced {
					combinedIndex.Manifests = append(combinedIndex.Manifests, member)
				}
			}
			combinedIndex.Annotations = previousIndex.Annotations
			combinedIndex.Subject = previousIndex.Subject
			combinedIndex.ArtifactType = previousIndex.ArtifactType
		}
		for _, key := range keys {
			combinedIndex.Manifests = append(combinedIndex.Manifests, result[key].Manifest)
		}
		data, err = json.Marshal(combinedIndex)
		if err != nil {
			return err
		}
		root = oci.Descriptor(mediaType, data)
		imageIDs := make(map[digest.Digest]string, len(result))
		for key, selection := range result {
			// Keep both runnable and encrypted-origin identities when their exact
			// descriptors survive, and pass the retained identity to the native writer.
			retained := false
			for _, member := range combinedIndex.Manifests {
				if manifestSelectionMatches(selection, member) {
					retained = true
					imageIDs[member.Digest] = selection.ImageID
				}
			}
			if !retained {
				delete(result, key)
			}
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

func manifestSelectionMatches(selection oci.StoredSelection, member v1.Descriptor) bool {
	return content.Equal(selection.Manifest, member) || selection.SourceManifest != nil && content.Equal(*selection.SourceManifest, member)
}
