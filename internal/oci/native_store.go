package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/containerd/platforms"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
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
	rootData, rootType, err := storedRootManifest(ctx, backend, image.ID, name, source)
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
		manifestData, rootType, err = selectedSource.GetManifest(ctx, &selected.Digest)
		if err != nil {
			return nil, err
		}
		if actual := Descriptor(rootType, manifestData); actual.Digest != selected.Digest || actual.Size != selected.Size || actual.MediaType != selected.MediaType {
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
	storageImageID := manifest.Config.Digest.Encoded()
	if selectedImage, err := backend.Image(storageImageID); err != nil {
		return nil, fmt.Errorf("resolve stored image %q selected storage record %s: %w", name, storageImageID, err)
	} else if selectedImage.ID != storageImageID {
		return nil, fmt.Errorf("stored image %q selected storage record changed from %s to %s", name, storageImageID, selectedImage.ID)
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
	data, mediaType, err := storedRootManifest(ctx, backend, image.ID, name, source)
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
	data, mediaType, err := storedRootManifest(ctx, backend, image.ID, name, source)
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
	if d, err := digest.Parse(name); err == nil {
		images, err := backend.ImagesByDigest(d)
		if err != nil {
			return nil, "", err
		}
		if len(images) == 0 {
			return nil, "", fmt.Errorf("stored manifest %s: %w", d, storage.ErrImageUnknown)
		}
		return images[0], name, nil
	}

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
		if err != nil && strings.Contains(candidate, "@") {
			if ref, refErr := imagestorage.Transport.ParseStoreReference(backend, candidate); refErr == nil {
				_, image, err = imagestorage.ResolveReference(ref)
				if errors.Is(err, imagestorage.ErrNoSuchImage) {
					err = storage.ErrImageUnknown
				}
			}
		}
		if err == nil {
			return image, candidate, nil
		}
		if !errors.Is(err, storage.ErrImageUnknown) {
			return nil, "", fmt.Errorf("inspect stored image %q: %w", name, err)
		}
	}
	return nil, "", fmt.Errorf("stored image %q: %w", name, storage.ErrImageUnknown)
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
