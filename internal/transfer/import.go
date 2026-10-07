package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"coopr/internal/buildah"
	"coopr/internal/enginecopy"
	"coopr/internal/imagestore"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func copyEngineImage(ctx context.Context, engine, name string, destination Destination, opts Options) (string, error) {
	if name == "" {
		return "", errors.New("engine image source requires a name or image ID")
	}
	stageDir, err := os.MkdirTemp("", "coopr-image-import-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(stageDir) }()
	layout := filepath.Join(stageDir, "layout")
	var platform v1.Platform
	if opts.PlatformExplicit {
		platform = opts.Platform
	}
	root, err := enginecopy.Read(ctx, engine, name, layout, platform)
	if err != nil {
		return "", err
	}
	storeOptions, err := nativeStoreOptions(opts)
	if err != nil {
		return "", err
	}
	if err := importImageLayout(ctx, storeOptions, layout, root); err != nil {
		return "", fmt.Errorf("retain imported %s image: %w", engine, err)
	}
	return copyStoredImage(ctx, root.Digest.String(), destination, opts)
}

// importImageLayout imports native children before publishing a native manifest list.
func importImageLayout(ctx context.Context, store buildah.StoreOptions, layout string, root v1.Descriptor) error {
	source, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		return err
	}
	children := []v1.Descriptor{root}
	var indexData []byte
	indexed := root.MediaType == v1.MediaTypeImageIndex || root.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
	if indexed {
		indexData, err = content.FetchAll(ctx, source, root)
		if err != nil {
			return err
		}
		var index v1.Index
		if err := json.Unmarshal(indexData, &index); err != nil {
			return err
		}
		if err := validateImportedImageIndex(index); err != nil {
			return err
		}
		children = index.Manifests
	}
	selections := make(map[string]oci.StoredSelection, len(children))
	var selectedPlatform v1.Platform
	for i, child := range children {
		childLayout := layout
		if indexed {
			childLayout = filepath.Join(filepath.Dir(layout), fmt.Sprintf("instance-%d", i))
			if err := localstore.CopyGraphLayout(ctx, childLayout, source, child); err != nil {
				return err
			}
		}
		raw, err := oci.ReadImageConfigLayout(ctx, childLayout)
		if err != nil {
			return err
		}
		var image v1.Image
		if err := json.Unmarshal(raw, &image); err != nil {
			return err
		}
		if image.OS != "linux" || image.Architecture == "" {
			return errors.New("imported image must specify a Linux platform")
		}
		selectedPlatform = platforms.Normalize(image.Platform)
		key := platforms.Format(selectedPlatform)
		if _, duplicate := selections[key]; duplicate {
			return fmt.Errorf("imported index has multiple images for platform %s", key)
		}
		if child.Platform != nil && platforms.Format(platforms.Normalize(*child.Platform)) != key {
			return fmt.Errorf("imported index platform differs from image configuration for %s", child.Digest)
		}
		imageID, err := buildah.ImportLayoutSupervised(ctx, store, childLayout, child)
		if err != nil {
			return err
		}
		selections[key] = oci.StoredSelection{Root: root, Manifest: child, ImageID: imageID, ConfigData: raw}
	}
	if indexed {
		imageIDs := make(map[digest.Digest]string, len(selections))
		for _, selection := range selections {
			imageIDs[selection.Manifest.Digest] = selection.ImageID
		}
		return buildah.WithStore(store, func(backend storage.Store) error {
			_, err := imagestore.FromStore(backend).WriteStoredIndex(ctx, root, indexData, imageIDs, "")
			return err
		})
	}
	return nil
}

// Engine import accepts a runnable image graph: one Linux image manifest per
// platform. Auxiliary descriptors such as BuildKit/Docker attestations are not
// runnable images and must be selected or removed by the source engine before
// Coopr publishes the index in native storage.
func validateImportedImageIndex(index v1.Index) error {
	if index.SchemaVersion != 2 || len(index.Manifests) == 0 {
		return errors.New("imported index has no image instances")
	}
	seen := make(map[string]bool, len(index.Manifests))
	for _, manifest := range index.Manifests {
		if manifest.Annotations["vnd.docker.reference.type"] != "" {
			return fmt.Errorf("imported index contains unsupported auxiliary descriptor %s", manifest.Digest)
		}
		if manifest.MediaType != v1.MediaTypeImageManifest && manifest.MediaType != "application/vnd.docker.distribution.manifest.v2+json" {
			return fmt.Errorf("imported index contains unsupported non-image manifest %s with media type %q", manifest.Digest, manifest.MediaType)
		}
		if manifest.Platform == nil || manifest.Platform.OS != "linux" || manifest.Platform.Architecture == "" {
			return fmt.Errorf("imported image manifest %s must specify a Linux platform", manifest.Digest)
		}
		key := platforms.Format(platforms.Normalize(*manifest.Platform))
		if seen[key] {
			return fmt.Errorf("imported index has multiple images for platform %s", key)
		}
		seen[key] = true
	}
	return nil
}
