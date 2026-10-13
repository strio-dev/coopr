package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/containerd/platforms"
	"oras.land/oras-go/v2/content"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage"
	dockerreference "go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/transports/alltransports"
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
	image, matched, err := findStoredImageAndName(backend, name)
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
	rootData, rootType, err := storedRootManifest(ctx, backend, image.ID, matched, source)
	if err != nil {
		return nil, fmt.Errorf("read stored image %q manifest: %w", name, err)
	}
	root := Descriptor(rootType, rootData)
	selected := root
	manifestData := rootData
	manifestSource := source
	storageImageID := image.ID
	if rootType == v1.MediaTypeImageIndex || rootType == dockerIndexType {
		var index v1.Index
		if err := json.Unmarshal(rootData, &index); err != nil {
			return nil, fmt.Errorf("decode stored image %q index: %w", name, err)
		}
		matches := make([]v1.Descriptor, 0, 1)
		for _, candidate := range index.Manifests {
			candidatePlatform, runnable, err := storedIndexMemberPlatform(ctx, backend, candidate)
			if err != nil {
				return nil, err
			}
			if runnable && storedPlatformMatches(candidatePlatform, platform) {
				matches = append(matches, candidate)
			}
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("%w: image %q has no manifest for %s", ErrStoredPlatformUnavailable, name, platforms.Format(platform))
		}
		selected = matches[0]
		if len(matches) > 1 {
			list, err := manifest.ListFromBlob(rootData, rootType)
			if err != nil {
				return nil, err
			}
			chosen, err := list.ChooseInstance(system)
			if err != nil {
				return nil, err
			}
			for _, candidate := range matches {
				if candidate.Digest == chosen {
					selected = candidate
					break
				}
			}
		}
		selectedImages, err := backend.ImagesByDigest(selected.Digest)
		if err != nil {
			return nil, err
		}
		if len(selectedImages) == 0 {
			return nil, fmt.Errorf("stored child %s: %w", selected.Digest, storage.ErrImageUnknown)
		}
		selectedRef, err := imagestorage.Transport.NewStoreReference(backend, nil, selectedImages[0].ID)
		if err != nil {
			return nil, err
		}
		selectedSource, err := selectedRef.NewImageSource(ctx, system)
		if err != nil {
			return nil, err
		}
		defer func() { _ = selectedSource.Close() }()
		manifestSource = selectedSource
		storageImageID = selectedImages[0].ID
		manifestData, rootType, err = selectedSource.GetManifest(ctx, &selected.Digest)
		if err != nil {
			return nil, err
		}
		if actual := Descriptor(rootType, manifestData); !content.Equal(actual, selected) {
			return nil, fmt.Errorf("stored image %q selected manifest differs from its index", name)
		}
	}
	var sourceManifest *v1.Descriptor
	var encoded v1.Manifest
	if err := json.Unmarshal(manifestData, &encoded); err != nil {
		return nil, err
	}
	encrypted := false
	for _, layer := range encoded.Layers {
		encrypted = encrypted || strings.HasSuffix(layer.MediaType, "+encrypted")
	}
	if encrypted {
		original := selected
		sourceManifest = &original
		manifestData, rootType, err = manifestSource.GetManifest(ctx, nil)
		if err != nil {
			return nil, err
		}
		selected = Descriptor(rootType, manifestData)
		if root.Digest == original.Digest {
			root = selected
		}
	} else if origin, originErr := storedOriginForName(backend, image.ID, name, selected.Digest); originErr != nil {
		return nil, originErr
	} else if origin != nil && origin.Source != "" {
		raw, mediaType, err := source.GetManifest(ctx, &origin.Source)
		if err != nil {
			return nil, err
		}
		original := Descriptor(mediaType, raw)
		if original.Digest != origin.Source {
			return nil, errors.New("stored encrypted origin differs from recorded digest")
		}
		sourceManifest = &original
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
	return &Resolved{
		Reference: name, Repository: "containers-storage", Kind: Image, Platform: actualPlatform,
		Root: root, Selected: selected, SourceManifest: sourceManifest, Manifest: manifest, Config: manifest.Config,
		ConfigData: configData, Layers: append([]v1.Descriptor(nil), manifest.Layers...), StorageImageID: storageImageID,
	}, nil
}

// StoredImagePlatforms returns the runnable platforms exposed by a native
// image name, including every manifest-list instance.
func StoredImagePlatforms(ctx context.Context, backend storage.Store, name string) ([]v1.Platform, error) {
	image, matched, err := findStoredImageAndName(backend, name)
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
	data, mediaType, err := storedRootManifest(ctx, backend, image.ID, matched, source)
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
			platform, runnable, err := storedIndexMemberPlatform(ctx, backend, descriptor)
			if err != nil {
				return nil, err
			}
			if runnable && platform.OS == "linux" {
				result = append(result, platform)
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
	if int64(len(configData)) != manifest.Config.Size || digest.FromBytes(configData) != manifest.Config.Digest {
		return nil, errors.New("stored image config differs from manifest")
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
	image, matched, err := findStoredImageAndName(backend, name)
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
	data, mediaType, err := storedRootManifest(ctx, backend, image.ID, matched, source)
	if err != nil {
		return v1.Descriptor{}, nil, false, err
	}
	if mediaType != v1.MediaTypeImageIndex && mediaType != dockerIndexType {
		return v1.Descriptor{}, nil, false, nil
	}
	descriptor := content.NewDescriptorFromBytes(mediaType, data)
	return descriptor, data, true, nil
}

func storedPlatformMatches(actual, requested v1.Platform) bool {
	return actual.OS == requested.OS && actual.Architecture == requested.Architecture && normalizeVariant(actual) == normalizeVariant(requested)
}

// StoredImageName returns the concrete native name selected by the same lookup
// order used for native image resolution.
func StoredImageName(backend storage.Store, name string) (string, error) {
	_, matched, err := findStoredImageAndName(backend, name)
	return matched, err
}

func findStoredImageAndName(backend storage.Store, name string) (*storage.Image, string, error) {
	if dockerreference.IsFullIdentifier(name) {
		image, err := backend.Image(name)
		if err != nil {
			return nil, "", err
		}
		return image, image.ID, nil
	}
	if d, err := digest.Parse(name); err == nil {
		images, err := backend.ImagesByDigest(d)
		if err != nil {
			return nil, "", err
		}
		if len(images) == 0 {
			if image, err := backend.Image(d.Encoded()); err == nil && image.ID == d.Encoded() {
				return image, image.ID, nil
			} else if err != nil && !errors.Is(err, storage.ErrImageUnknown) {
				return nil, "", err
			}
			return nil, "", fmt.Errorf("stored manifest %s: %w", d, storage.ErrImageUnknown)
		}
		return images[0], name, nil
	}

	// Do not let native transport parsing open a different store. A name
	// such as docker-daemon:dev is also a valid local repository/tag; normalize
	// that ordinary image name before LookupImage sees the transport prefix.
	if transport := alltransports.TransportFromImageName(name); transport != nil {
		normalized, err := libimage.NormalizeName(name)
		if err != nil {
			return nil, "", fmt.Errorf("unsupported transport %q for looking up local images", transport.Name())
		}
		name = normalized.String()
		if alltransports.TransportFromImageName(name) != nil {
			// Fully qualified hosts can retain a transport spelling. Resolve
			// the normalized reference against this backend, including digests,
			// without entering transport parsing or opening a default store.
			ref, err := imagestorage.Transport.NewStoreReference(backend, normalized, "")
			if err != nil {
				return nil, "", err
			}
			_, image, err := imagestorage.ResolveReference(ref)
			if errors.Is(err, imagestorage.ErrNoSuchImage) {
				err = storage.ErrImageUnknown
			}
			if err != nil {
				return nil, "", err
			}
			return image, name, nil
		}
	}

	runtime, err := libimage.RuntimeFromStore(backend, nil)
	if err != nil {
		return nil, "", err
	}
	image, matched, err := runtime.LookupImage(name, &libimage.LookupImageOptions{ManifestList: true})
	if err != nil {
		return nil, "", err
	}
	return image.StorageImage(), matched, nil
}

// StoredSelection is transient verified metadata for one resident platform.
// Native names, manifests and image records remain the authoritative storage.
type StoredSelection struct {
	Root           v1.Descriptor
	Manifest       v1.Descriptor
	SourceManifest *v1.Descriptor
	ImageID        string
	ConfigData     []byte
}

// Platform returns the selected descriptor's platform, or its verified config
// platform for single-image records whose manifest descriptor has no platform.
func (selection StoredSelection) Platform() (v1.Platform, error) {
	if selection.Manifest.Platform != nil {
		return platforms.Normalize(*selection.Manifest.Platform), nil
	}
	var config v1.Image
	if err := json.Unmarshal(selection.ConfigData, &config); err != nil {
		return v1.Platform{}, err
	}
	return platforms.Normalize(config.Platform), nil
}

func storedReferenceDigest(name string) *digest.Digest {
	value := name
	if at := strings.LastIndexByte(name, '@'); at >= 0 {
		value = name[at+1:]
	}
	if d, err := digest.Parse(value); err == nil {
		return &d
	}
	return nil
}

// StoredImageSelections reads the exact root and resident runnable instances.
// For partially pulled indexes it retains the full index while omitting absent
// children; callers requiring a complete image must check every runnable child.
func StoredImageSelections(ctx context.Context, backend storage.Store, name string) (v1.Descriptor, []byte, map[string]StoredSelection, error) {
	root, data, indexed, err := StoredImageIndex(ctx, backend, name)
	if err != nil {
		return v1.Descriptor{}, nil, nil, err
	}
	if indexed {
		var index v1.Index
		if err := json.Unmarshal(data, &index); err != nil {
			return v1.Descriptor{}, nil, nil, err
		}
		list, err := manifest.ListFromBlob(data, root.MediaType)
		if err != nil {
			return v1.Descriptor{}, nil, nil, err
		}
		result := map[string]StoredSelection{}
		for _, child := range index.Manifests {
			platform, runnable, err := storedIndexMemberPlatform(ctx, backend, child)
			if err != nil {
				return v1.Descriptor{}, nil, nil, err
			}
			if !runnable {
				continue
			}
			resolved, err := ResolveStoredImage(ctx, backend, child.Digest.String(), platform)
			if errors.Is(err, storage.ErrImageUnknown) || os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return v1.Descriptor{}, nil, nil, err
			}
			if resolved.Selected.Digest != child.Digest && (resolved.SourceManifest == nil || resolved.SourceManifest.Digest != child.Digest) {
				return v1.Descriptor{}, nil, nil, fmt.Errorf("stored child %s differs from index", child.Digest)
			}
			key := platforms.Format(platforms.Normalize(platform))
			if existing, exists := result[key]; exists {
				chosen, err := list.ChooseInstance(&types.SystemContext{OSChoice: platform.OS, ArchitectureChoice: platform.Architecture, VariantChoice: platform.Variant})
				if err != nil {
					return v1.Descriptor{}, nil, nil, err
				}
				if child.Digest == chosen {
					result[existing.Manifest.Digest.String()] = existing
				} else {
					key = child.Digest.String()
				}
			}
			selected := child
			if resolved.Selected.Digest != child.Digest {
				selected = resolved.Selected
				selected.Platform = child.Platform
			}
			result[key] = StoredSelection{Root: root, Manifest: selected, SourceManifest: resolved.SourceManifest, ImageID: resolved.StorageImageID, ConfigData: append([]byte(nil), resolved.ConfigData...)}
		}
		return root, data, result, nil
	}
	available, err := StoredImagePlatforms(ctx, backend, name)
	if err != nil {
		return v1.Descriptor{}, nil, nil, err
	}
	result := map[string]StoredSelection{}
	for _, platform := range available {
		resolved, err := ResolveStoredImage(ctx, backend, name, platform)
		if indexed && (errors.Is(err, storage.ErrImageUnknown) || os.IsNotExist(err)) {
			continue
		}
		if err != nil {
			return v1.Descriptor{}, nil, nil, err
		}
		if !indexed {
			root = resolved.Root
		}
		result[platforms.Format(platforms.Normalize(platform))] = StoredSelection{Root: resolved.Root, Manifest: resolved.Selected, SourceManifest: resolved.SourceManifest, ImageID: resolved.StorageImageID, ConfigData: append([]byte(nil), resolved.ConfigData...)}
	}
	return root, data, result, nil
}

const storedOriginKey = "coopr.image-origins.v1"

type storedOrigin struct {
	Root     digest.Digest `json:"root"`
	Selected digest.Digest `json:"selected"`
	Source   digest.Digest `json:"source,omitempty"`
}

// RecordStoredOrigin retains only native-manifest pointers when a registry
// pull's root or encrypted source differs from its runnable default manifest.
// containers/image retains those manifests but has no per-name association.
func RecordStoredOrigin(ctx context.Context, backend storage.Store, name string, selected StoredSelection) error {
	image, matched, err := findStoredImageAndName(backend, name)
	if err != nil {
		return err
	}
	if image.ID != selected.ImageID {
		return errors.New("native name changed while recording pulled origin")
	}
	lock, err := backend.GetDigestLock(digest.FromString(storedOriginKey + image.ID))
	if err != nil {
		return err
	}
	lock.Lock()
	defer lock.Unlock()
	origins := map[string]storedOrigin{}
	names, err := backend.ListImageBigData(image.ID)
	if err != nil {
		return err
	}
	var data []byte
	if slices.Contains(names, storedOriginKey) {
		data, err = backend.ImageBigData(image.ID, storedOriginKey)
	} else {
		err = os.ErrNotExist
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(data, &origins); err != nil {
			return err
		}
	}
	if selected.Root.Digest == selected.Manifest.Digest && selected.SourceManifest == nil {
		if _, exists := origins[matched]; !exists {
			return nil
		}
		delete(origins, matched)
		data, err = json.Marshal(origins)
		if err != nil {
			return err
		}
		return backend.SetImageBigData(image.ID, storedOriginKey, data, nil)
	}
	origin := storedOrigin{Root: selected.Root.Digest, Selected: selected.Manifest.Digest}
	if selected.SourceManifest != nil {
		origin.Source = selected.SourceManifest.Digest
	}
	origins[matched] = origin
	data, err = json.Marshal(origins)
	if err != nil {
		return err
	}
	return backend.SetImageBigData(image.ID, storedOriginKey, data, nil)
}

func storedOriginForName(backend storage.Store, imageID, name string, current digest.Digest) (*storedOrigin, error) {
	if storedReferenceDigest(name) != nil {
		return nil, nil
	}
	_, matched, err := findStoredImageAndName(backend, name)
	if err != nil {
		return nil, err
	}
	names, err := backend.ListImageBigData(imageID)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(names, storedOriginKey) {
		return nil, nil
	}
	data, err := backend.ImageBigData(imageID, storedOriginKey)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var origins map[string]storedOrigin
	if err := json.Unmarshal(data, &origins); err != nil {
		return nil, err
	}
	origin, ok := origins[matched]
	if !ok || origin.Selected != current {
		return nil, nil
	}
	return &origin, nil
}

func storedRootManifest(ctx context.Context, backend storage.Store, imageID, name string, source types.ImageSource) ([]byte, string, error) {
	requested := storedReferenceDigest(name)
	data, mediaType, err := source.GetManifest(ctx, requested)
	if err != nil && requested != nil && (errors.Is(err, os.ErrNotExist) || os.IsNotExist(err)) {
		data, mediaType, err = source.GetManifest(ctx, nil)
	}
	if err != nil {
		return nil, "", err
	}
	if requested != nil && digest.FromBytes(data) != *requested {
		return nil, "", errors.New("stored manifest differs from requested digest")
	}
	origin, err := storedOriginForName(backend, imageID, name, digest.FromBytes(data))
	if err != nil {
		return nil, "", err
	}
	if origin == nil || origin.Root == digest.FromBytes(data) {
		return data, mediaType, nil
	}
	data, mediaType, err = source.GetManifest(ctx, &origin.Root)
	if err != nil {
		return nil, "", err
	}
	if digest.FromBytes(data) != origin.Root {
		return nil, "", errors.New("stored origin manifest differs from recorded digest")
	}
	return data, mediaType, nil
}

// storedIndexMemberPlatform retains descriptor metadata and derives omitted
// platforms from the resident child's verified image configuration.
func storedIndexMemberPlatform(ctx context.Context, backend storage.Store, child v1.Descriptor) (v1.Platform, bool, error) {
	if child.ArtifactType != "" {
		return v1.Platform{}, false, nil
	}
	var platform v1.Platform
	if child.Platform != nil {
		platform = *child.Platform
	} else {
		available, err := StoredImagePlatforms(ctx, backend, child.Digest.String())
		if errors.Is(err, storage.ErrImageUnknown) || os.IsNotExist(err) {
			return v1.Platform{}, false, nil
		}
		if err != nil {
			return v1.Platform{}, false, err
		}
		if len(available) != 1 {
			return v1.Platform{}, false, fmt.Errorf("stored child %s has no unique image platform", child.Digest)
		}
		platform = available[0]
	}
	if platform.OS == "unknown" || platform.Architecture == "unknown" {
		return v1.Platform{}, false, nil
	}
	if platform.OS == "" || platform.Architecture == "" {
		return v1.Platform{}, false, fmt.Errorf("stored child %s has no image OS or architecture", child.Digest)
	}
	return platforms.Normalize(platform), true, nil
}
