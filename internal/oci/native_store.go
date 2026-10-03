package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage/manifests"
	imagecopy "go.podman.io/image/v5/copy"
	dockerreference "go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/pkg/shortnames"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

var ErrStoredPlatformUnavailable = errors.New("stored image platform unavailable")

// ResolveStoredImage reads an existing containers/storage name without moving
// or adding that name. It supports Podman-created single images and manifest
// lists and returns the exact selected manifest/config for Coopr planning.
func ResolveStoredImage(ctx context.Context, backend storage.Store, name string, platform v1.Platform) (*Resolved, error) {
	if backend == nil {
		return nil, errors.New("nil containers/storage store")
	}
	image, err := findStoredImage(backend, name)
	if err != nil {
		return nil, err
	}
	ref, err := imagestorage.Transport.NewStoreReference(backend, nil, image.ID)
	if err != nil {
		return nil, err
	}
	system := &types.SystemContext{OSChoice: platform.OS, ArchitectureChoice: platform.Architecture, VariantChoice: platform.Variant}
	source, err := ref.NewImageSource(ctx, system)
	if err != nil {
		return nil, fmt.Errorf("open stored image %q: %w", name, err)
	}
	defer func() { _ = source.Close() }()
	rootData, rootType, err := source.GetManifest(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("read stored image %q manifest: %w", name, err)
	}
	root := Descriptor(rootType, rootData)
	selected := root
	manifestData := rootData
	manifestSource := source
	if rootType == v1.MediaTypeImageIndex || rootType == dockerIndexType {
		var index v1.Index
		if err := json.Unmarshal(rootData, &index); err != nil {
			return nil, fmt.Errorf("decode stored image %q index: %w", name, err)
		}
		matches := make([]v1.Descriptor, 0, 1)
		for _, candidate := range index.Manifests {
			if candidate.Platform != nil && storedPlatformMatches(*candidate.Platform, platform) {
				matches = append(matches, candidate)
			}
		}
		if len(matches) != 1 {
			return nil, fmt.Errorf("%w: image %q index has %d manifests for platform %s/%s/%s; expected one", ErrStoredPlatformUnavailable, name, len(matches), platform.OS, platform.Architecture, platform.Variant)
		}
		selected = matches[0]
		_, list, err := manifests.LoadFromImage(backend, image.ID)
		if err != nil {
			return nil, fmt.Errorf("load stored image %q index instances: %w", name, err)
		}
		selectedRef, err := list.Reference(backend, imagecopy.CopySpecificImages, []digest.Digest{selected.Digest})
		if err != nil {
			return nil, fmt.Errorf("reference stored image %q selected instance: %w", name, err)
		}
		selectedSource, err := selectedRef.NewImageSource(ctx, system)
		if err != nil {
			return nil, fmt.Errorf("open stored image %q selected instance: %w", name, err)
		}
		defer func() { _ = selectedSource.Close() }()
		manifestSource = selectedSource
		manifestData, rootType, err = selectedSource.GetManifest(ctx, &selected.Digest)
		if err != nil {
			return nil, fmt.Errorf("read stored image %q selected manifest: %w", name, err)
		}
		if actual := Descriptor(rootType, manifestData); actual.Digest != selected.Digest || actual.Size != selected.Size || actual.MediaType != selected.MediaType {
			return nil, fmt.Errorf("stored image %q selected manifest differs from its index", name)
		}
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, fmt.Errorf("decode stored image %q manifest: %w", name, err)
	}
	reader, _, err := manifestSource.GetBlob(ctx, types.BlobInfo{Digest: manifest.Config.Digest, Size: manifest.Config.Size}, nil)
	if err != nil {
		return nil, fmt.Errorf("read stored image %q config: %w", name, err)
	}
	configData, readErr := io.ReadAll(reader)
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return nil, fmt.Errorf("read stored image %q config: %w", name, err)
	}
	if int64(len(configData)) != manifest.Config.Size || manifest.Config.Digest != digest.FromBytes(configData) {
		return nil, fmt.Errorf("stored image %q config differs from its manifest", name)
	}
	var config v1.Image
	if err := json.Unmarshal(configData, &config); err != nil {
		return nil, fmt.Errorf("decode stored image %q config: %w", name, err)
	}
	actualPlatform := v1.Platform{OS: config.OS, Architecture: config.Architecture, Variant: config.Variant, OSVersion: config.OSVersion, OSFeatures: config.OSFeatures}
	if !storedPlatformMatches(actualPlatform, platform) {
		return nil, fmt.Errorf("%w: image %q has platform %s/%s/%s, want %s/%s/%s", ErrStoredPlatformUnavailable, name, actualPlatform.OS, actualPlatform.Architecture, actualPlatform.Variant, platform.OS, platform.Architecture, platform.Variant)
	}
	storageImageID := manifest.Config.Digest.Encoded()
	if selectedImage, err := backend.Image(storageImageID); err != nil {
		return nil, fmt.Errorf("resolve stored image %q selected storage record %s: %w", name, storageImageID, err)
	} else if selectedImage.ID != storageImageID {
		return nil, fmt.Errorf("stored image %q selected storage record changed from %s to %s", name, storageImageID, selectedImage.ID)
	}
	return &Resolved{
		Reference: name, Repository: "containers-storage", Kind: Image, Platform: actualPlatform,
		Root: root, Selected: selected, Manifest: manifest, Config: manifest.Config,
		ConfigData: configData, Layers: append([]v1.Descriptor(nil), manifest.Layers...), StorageImageID: storageImageID,
	}, nil
}

// StoredImagePlatforms returns the runnable platforms exposed by a native
// image name, including every manifest-list instance.
func StoredImagePlatforms(ctx context.Context, backend storage.Store, name string) ([]v1.Platform, error) {
	image, err := findStoredImage(backend, name)
	if err != nil {
		return nil, err
	}
	ref, err := imagestorage.Transport.NewStoreReference(backend, nil, image.ID)
	if err != nil {
		return nil, err
	}
	source, err := ref.NewImageSource(ctx, &types.SystemContext{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = source.Close() }()
	data, mediaType, err := source.GetManifest(ctx, nil)
	if err != nil {
		return nil, err
	}
	if mediaType == v1.MediaTypeImageIndex || mediaType == dockerIndexType {
		var index v1.Index
		if err := json.Unmarshal(data, &index); err != nil {
			return nil, err
		}
		result := make([]v1.Platform, 0, len(index.Manifests))
		for _, descriptor := range index.Manifests {
			if descriptor.Platform != nil && descriptor.Platform.OS == "linux" && descriptor.Platform.Architecture != "" && descriptor.Platform.Architecture != "unknown" && descriptor.ArtifactType == "" {
				result = append(result, *descriptor.Platform)
			}
		}
		return result, nil
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	reader, _, err := source.GetBlob(ctx, types.BlobInfo{Digest: manifest.Config.Digest, Size: manifest.Config.Size}, nil)
	if err != nil {
		return nil, err
	}
	configData, readErr := io.ReadAll(reader)
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return nil, err
	}
	var config v1.Image
	if err := json.Unmarshal(configData, &config); err != nil {
		return nil, err
	}
	return []v1.Platform{{OS: config.OS, Architecture: config.Architecture, Variant: config.Variant, OSVersion: config.OSVersion, OSFeatures: config.OSFeatures}}, nil
}

// StoredImageIndex returns the exact manifest-list descriptor and bytes for a
// native image name. The boolean is false for a single-platform image.
func StoredImageIndex(ctx context.Context, backend storage.Store, name string) (v1.Descriptor, []byte, bool, error) {
	image, err := findStoredImage(backend, name)
	if err != nil {
		return v1.Descriptor{}, nil, false, err
	}
	ref, err := imagestorage.Transport.NewStoreReference(backend, nil, image.ID)
	if err != nil {
		return v1.Descriptor{}, nil, false, err
	}
	source, err := ref.NewImageSource(ctx, &types.SystemContext{})
	if err != nil {
		return v1.Descriptor{}, nil, false, err
	}
	defer func() { _ = source.Close() }()
	data, mediaType, err := source.GetManifest(ctx, nil)
	if err != nil {
		return v1.Descriptor{}, nil, false, err
	}
	if mediaType != v1.MediaTypeImageIndex && mediaType != dockerIndexType {
		return v1.Descriptor{}, nil, false, nil
	}
	descriptor := v1.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
	return descriptor, data, true, nil
}

func storedPlatformMatches(actual, requested v1.Platform) bool {
	return actual.OS == requested.OS && actual.Architecture == requested.Architecture && normalizeVariant(actual) == normalizeVariant(requested)
}

func findStoredImage(backend storage.Store, name string) (*storage.Image, error) {
	image, _, err := findStoredImageAndName(backend, name)
	return image, err
}

// StoredImageName returns the concrete native name selected by the same lookup
// order used for native image resolution.
func StoredImageName(backend storage.Store, name string) (string, error) {
	_, matched, err := findStoredImageAndName(backend, name)
	return matched, err
}

func findStoredImageAndName(backend storage.Store, name string) (*storage.Image, string, error) {
	resolved, err := shortnames.ResolveLocally(&types.SystemContext{}, name)
	if err != nil {
		return nil, "", fmt.Errorf("resolve local image name %q: %w", name, err)
	}
	candidates := make([]string, 0, len(resolved)+2)
	for _, candidate := range resolved {
		candidates = append(candidates, candidate.String())
	}
	// Match libimage's compatibility fallback after aliases, localhost, and
	// configured unqualified-search registries.
	if dockerNamed, err := dockerreference.ParseDockerRef(name); err == nil {
		candidates = append(candidates, dockerNamed.String())
	}
	// IDs are checked only after name expansion, as in libimage lookup.
	candidates = append(candidates, name)
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		image, err := backend.Image(candidate)
		if err == nil {
			return image, candidate, nil
		}
		if !errors.Is(err, storage.ErrImageUnknown) {
			return nil, "", fmt.Errorf("inspect stored image %q: %w", name, err)
		}
	}
	return nil, "", fmt.Errorf("stored image %q: %w", name, storage.ErrImageUnknown)
}
