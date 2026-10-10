// Package imagestore writes finished OCI archives into containers-storage.
package imagestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"coopr/internal/oci"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage/manifests"
	imagecopy "go.podman.io/image/v5/copy"
	imagereference "go.podman.io/image/v5/docker/reference"
	ociarchive "go.podman.io/image/v5/oci/archive"
	ocilayout "go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/signature"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/homedir"
	"go.podman.io/storage/pkg/unshare"
	storagetypes "go.podman.io/storage/types"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// Store provides image operations against containers/storage.
type Store struct {
	backend storage.Store
	system  *types.SystemContext
}

// FromStore borrows a native handle for image operations. Its caller owns the
// handle and must keep it open for the entire operation; do not call Close.
func FromStore(backend storage.Store) *Store {
	return &Store{backend: backend, system: &types.SystemContext{BigFilesTemporaryDir: os.TempDir()}}
}

// NewWithOptions opens an explicitly selected containers/storage graph.
func NewWithOptions(options storagetypes.StoreOptions) (*Store, error) {
	backend, err := storage.GetStore(options)
	if err != nil {
		return nil, fmt.Errorf("open containers-storage (run root %s, graph root %s): %w", options.RunRoot, options.GraphRoot, err)
	}
	return &Store{backend: backend, system: &types.SystemContext{BigFilesTemporaryDir: os.TempDir()}}, nil
}

// Tag gives an existing storage image a Podman-visible name.
func (s *Store) Tag(imageID, tag string) (string, error) {
	if s == nil || s.backend == nil {
		return "", errors.New("image store is not initialized")
	}
	name, err := NormalizeTag(tag)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", errors.New("container storage destination requires a tag")
	}
	if _, err := s.backend.Image(imageID); err != nil {
		return "", fmt.Errorf("resolve stored image %s: %w", imageID, err)
	}
	if err := s.backend.AddNames(imageID, []string{name}); err != nil {
		return "", fmt.Errorf("tag stored image: %w", err)
	}
	return name, nil
}

// TagSelected names a verified manifest already present on a native image.
// It preserves existing native aliases when selecting a different manifest.
func (s *Store) TagSelected(ctx context.Context, imageID string, selected v1.Descriptor, tag string) (string, error) {
	name, err := NormalizeTag(tag)
	if err != nil || name == "" {
		if err == nil {
			err = errors.New("container storage destination requires a tag")
		}
		return "", err
	}
	lock, err := manifests.LockerForImage(s.backend, imageID)
	if err != nil {
		return "", err
	}
	lock.Lock()
	defer lock.Unlock()
	ref, err := imagestorage.Transport.NewStoreReference(s.backend, nil, imageID)
	if err != nil {
		return "", err
	}
	source, err := ref.NewImageSource(ctx, s.system)
	if err != nil {
		return "", err
	}
	defer func() { _ = source.Close() }()
	data, mediaType, err := source.GetManifest(ctx, &selected.Digest)
	if err != nil {
		return "", err
	}
	if actual := oci.Descriptor(mediaType, data); actual.Digest != selected.Digest || actual.Size != selected.Size || actual.MediaType != selected.MediaType {
		return "", errors.New("stored manifest differs from selected descriptor")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	previous, err := oci.CaptureStoredSignatures(s.backend, imageID)
	if err != nil {
		return "", err
	}
	defaultData, _, err := source.GetManifest(ctx, nil)
	if err != nil {
		return "", err
	}
	previousDigest := digest.FromBytes(defaultData)
	selectedSignatures, err := oci.StoredSignatureBlobs(previous, selected.Digest, previousDigest == selected.Digest)
	if err != nil {
		return "", err
	}
	if previousDigest != selected.Digest {
		image, err := s.backend.Image(imageID)
		if err != nil {
			return "", err
		}
		for _, existing := range image.Names {
			if parsed, err := reference.ParseNormalizedNamed(existing); err == nil {
				if _, immutable := parsed.(reference.Canonical); immutable {
					continue
				}
			}
			if existing == name {
				continue
			}
			// Native names bind the entire record, not one stored manifest.
			// Select the exact variant in its own record; native storage shares
			// the existing layers and leaves other aliases and origins intact.
			aliasID, err := oci.SelectedStoredImage(ctx, s.backend, imageID, selected.Digest)
			if err != nil {
				return "", err
			}
			aliasLock, err := manifests.LockerForImage(s.backend, aliasID)
			if err != nil {
				return "", err
			}
			aliasLock.Lock()
			defer aliasLock.Unlock()
			aliasSignatures, err := oci.CaptureStoredSignatures(s.backend, aliasID)
			if err != nil {
				return "", err
			}
			retainedSignatures, err := oci.StoredSignatureBlobs(aliasSignatures, selected.Digest, true)
			if err != nil {
				return "", err
			}
			for _, value := range selectedSignatures {
				if !slices.ContainsFunc(retainedSignatures, func(existing []byte) bool { return bytes.Equal(existing, value) }) {
					retainedSignatures = append(retainedSignatures, value)
				}
			}
			if err := oci.WriteStoredManifestSignatures(s.backend, aliasID, selected.Digest, true, retainedSignatures, aliasSignatures); err != nil {
				return "", errors.Join(err, oci.RestoreStoredSignatures(s.backend, aliasID, aliasSignatures))
			}
			if _, err := s.Tag(aliasID, name); err != nil {
				return "", errors.Join(err, oci.RestoreStoredSignatures(s.backend, aliasID, aliasSignatures))
			}
			if err := oci.RecordStoredOrigin(ctx, s.backend, name, oci.StoredSelection{Root: selected, Manifest: selected, ImageID: aliasID}); err != nil {
				return "", err
			}
			return name, nil
		}
	}
	if previousDigest != selected.Digest {
		previousSignatures, err := oci.StoredSignatureBlobs(previous, previousDigest, true)
		if err != nil {
			return "", err
		}
		if err := oci.WriteStoredManifestSignatures(s.backend, imageID, previousDigest, false, previousSignatures, previous); err != nil {
			return "", errors.Join(err, oci.RestoreStoredSignatures(s.backend, imageID, previous))
		}
		preserved, err := oci.CaptureStoredSignatures(s.backend, imageID)
		if err != nil {
			return "", errors.Join(err, oci.RestoreStoredSignatures(s.backend, imageID, previous))
		}
		if err := oci.WriteStoredManifestSignatures(s.backend, imageID, selected.Digest, true, selectedSignatures, preserved); err != nil {
			return "", errors.Join(err, oci.RestoreStoredSignatures(s.backend, imageID, previous))
		}
	}
	if err := s.backend.SetImageBigData(imageID, storage.ImageDigestBigDataKey, data, func(data []byte) (digest.Digest, error) { return digest.FromBytes(data), nil }); err != nil {
		return "", errors.Join(err, oci.RestoreStoredSignatures(s.backend, imageID, previous))
	}
	result, err := s.Tag(imageID, name)
	if err != nil {
		restoreErr := s.backend.SetImageBigData(imageID, storage.ImageDigestBigDataKey, defaultData, func(data []byte) (digest.Digest, error) { return digest.FromBytes(data), nil })
		return "", errors.Join(err, restoreErr, oci.RestoreStoredSignatures(s.backend, imageID, previous))
	}
	// An explicit single-platform tag names this manifest, including when the
	// destination previously referred to a pulled index on the same record.
	if err := oci.RecordStoredOrigin(ctx, s.backend, name, oci.StoredSelection{Root: selected, Manifest: selected, ImageID: imageID}); err != nil {
		return "", fmt.Errorf("clear tagged image origin: %w", err)
	}
	return result, nil
}

// TagExisting adds a name to the exact native image or manifest-list record
// selected by source, without importing or reconstructing any payload.
func (s *Store) TagExisting(source, tag string) (string, error) {
	if s == nil || s.backend == nil {
		return "", errors.New("image store is not initialized")
	}
	matched, err := oci.StoredImageName(s.backend, source)
	if err != nil {
		return "", err
	}
	image, err := s.backend.Image(matched)
	if err != nil {
		return "", err
	}
	return s.Tag(image.ID, tag)
}

// RemoveTag removes only one Podman-visible name, never the underlying image.
func (s *Store) RemoveTag(tag string) (bool, error) {
	if s == nil || s.backend == nil {
		return false, errors.New("image store is not initialized")
	}
	name, err := oci.StoredImageName(s.backend, tag)
	if err != nil {
		if errors.Is(err, storage.ErrImageUnknown) {
			return false, nil
		}
		return false, err
	}
	image, err := s.backend.Image(name)
	if err != nil {
		return false, err
	}
	if err := s.backend.RemoveNames(image.ID, []string{name}); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) MatchedName(name string) (string, error) {
	if s == nil || s.backend == nil {
		return "", errors.New("image store is not initialized")
	}
	return oci.StoredImageName(s.backend, name)
}

// Names returns every current mutable image name in the selected native store.
func (s *Store) Names() ([]string, error) {
	if s == nil || s.backend == nil {
		return nil, errors.New("image store is not initialized")
	}
	images, err := s.backend.Images()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, image := range images {
		for _, name := range image.Names {
			if parsed, err := reference.ParseNormalizedNamed(name); err == nil {
				if _, immutable := parsed.(reference.Canonical); immutable {
					continue
				}
			}
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

func (s *Store) Resolve(ctx context.Context, name string, platform v1.Platform) (*oci.Resolved, error) {
	if s == nil || s.backend == nil {
		return nil, errors.New("image store is not initialized")
	}
	return oci.ResolveStoredImage(ctx, s.backend, name, platform)
}

func (s *Store) Platforms(ctx context.Context, name string) ([]v1.Platform, error) {
	if s == nil || s.backend == nil {
		return nil, errors.New("image store is not initialized")
	}
	return oci.StoredImagePlatforms(ctx, s.backend, name)
}

func (s *Store) Index(ctx context.Context, name string) (v1.Descriptor, []byte, bool, error) {
	if s == nil || s.backend == nil {
		return v1.Descriptor{}, nil, false, errors.New("image store is not initialized")
	}
	return oci.StoredImageIndex(ctx, s.backend, name)
}

// DefaultStoreOptions loads the effective Podman containers/storage
// configuration at call time and applies the same rootless path correction as
// explicit Podman transfers.
func DefaultStoreOptions() (storagetypes.StoreOptions, error) {
	// DefaultStoreOptions caches its first result. The CLI may have entered a
	// user namespace after package initialization, so load the current rootless
	// paths at the moment the explicit Podman destination is opened.
	options, err := storagetypes.LoadStoreOptions(storagetypes.LoadOptions{})
	if err != nil {
		return storagetypes.StoreOptions{}, fmt.Errorf("load containers-storage options: %w", err)
	}
	if err := normalizeRootlessStoragePaths(&options); err != nil {
		return storagetypes.StoreOptions{}, err
	}
	return options, nil
}

func normalizeRootlessStoragePaths(options *storagetypes.StoreOptions) error {
	if !unshare.IsRootless() || unshare.GetRootlessUID() == 0 {
		return nil
	}
	// Some system storage.conf files explicitly set the rootful defaults.
	// containers/storage applies those values after its rootless defaults, so
	// correct only the two rootful paths and preserve actual custom paths.
	if options.GraphRoot == "/var/lib/containers/storage" {
		data, err := homedir.GetDataHome()
		if err != nil {
			return fmt.Errorf("find rootless image storage directory: %w", err)
		}
		options.GraphRoot = filepath.Join(data, "containers", "storage")
	}
	if options.RunRoot == "/run/containers/storage" {
		runtime, err := homedir.GetRuntimeDir()
		if err != nil {
			return fmt.Errorf("find rootless image runtime directory: %w", err)
		}
		options.RunRoot = filepath.Join(runtime, "containers")
	}
	options.GraphDriverOptions = slices.DeleteFunc(options.GraphDriverOptions, func(option string) bool {
		return option == "imagestore=/usr/lib/containers/storage"
	})
	return nil
}

// Close releases storage resources without unmounting other users' layers.
func (s *Store) Close() error {
	if s == nil || s.backend == nil {
		return nil
	}
	backend := s.backend
	_, err := backend.Shutdown(false)
	// A shared store can contain layers mounted by another process. The
	// non-forced shutdown intentionally leaves those mounts alone.
	if errors.Is(err, storage.ErrLayerUsedByContainer) {
		s.backend = nil
		return nil
	}
	if err != nil {
		return fmt.Errorf("close containers-storage: %w", err)
	}
	backend.Free()
	s.backend = nil
	return nil
}

// NormalizeTag resolves Podman's local-name and latest defaults.
func NormalizeTag(tag string) (string, error) {
	if tag == "" {
		return "", nil
	}
	if strings.TrimSpace(tag) != tag || strings.ContainsAny(tag, " \t\r\n") || strings.HasPrefix(tag, "-") || strings.Contains(tag, "://") {
		return "", fmt.Errorf("invalid local image tag %q", tag)
	}
	first := strings.SplitN(tag, "/", 2)[0]
	if !strings.Contains(tag, "/") || first != "localhost" && !strings.ContainsAny(first, ".:") {
		tag = "localhost/" + tag
	}
	ref, err := reference.ParseNormalizedNamed(tag)
	if err != nil {
		return "", fmt.Errorf("invalid local image tag %q: %w", tag, err)
	}
	if _, ok := ref.(reference.Digested); ok {
		return "", fmt.Errorf("local image tag %q must not include a digest", tag)
	}
	return reference.TagNameOnly(ref).String(), nil
}

// Write copies an OCI image into the owned store and returns its normalized
// name, or its image ID when no name was requested.
func (s *Store) Write(ctx context.Context, archivePath, tag string) (string, error) {
	if s == nil || s.backend == nil {
		return "", errors.New("image store is not initialized")
	}
	name, err := NormalizeTag(tag)
	if err != nil {
		return "", err
	}
	archive, err := filepath.Abs(archivePath)
	if err != nil {
		return "", fmt.Errorf("resolve OCI archive path: %w", err)
	}
	imageID, err := oci.ArchiveImageConfigDigest(ctx, archive)
	if err != nil {
		return "", fmt.Errorf("identify OCI image: %w", err)
	}
	return s.writeImage(ctx, imageID, name, func() (types.ImageReference, error) {
		src, err := ociarchive.NewReference(archive, "")
		if err != nil {
			return nil, fmt.Errorf("open OCI archive reference: %w", err)
		}
		return src, nil
	})
}

// WriteLayout copies one rooted image from an OCI image layout into this
// containers-storage store without creating an intermediate archive.
func (s *Store) WriteLayout(ctx context.Context, layoutPath string, root v1.Descriptor, tag string) (string, error) {
	if s == nil || s.backend == nil {
		return "", errors.New("image store is not initialized")
	}
	name, err := NormalizeTag(tag)
	if err != nil {
		return "", err
	}
	source, err := orasoci.NewWithContext(ctx, layoutPath)
	if err != nil {
		return "", fmt.Errorf("open OCI image layout: %w", err)
	}
	imageID, err := imageConfigDigest(ctx, source, root)
	if err != nil {
		return "", fmt.Errorf("identify OCI image: %w", err)
	}
	return s.writeImage(ctx, imageID, name, func() (types.ImageReference, error) {
		return LayoutReference(layoutPath, root)
	})
}

// WriteIndexLayout copies every instance of a complete OCI index into the
// selected containers-storage store and saves a manifest list under tag.
func (s *Store) WriteIndexLayout(ctx context.Context, layoutPath string, root v1.Descriptor, tag string) (_ string, retErr error) {
	if s == nil || s.backend == nil {
		return "", errors.New("image store is not initialized")
	}
	if root.MediaType != v1.MediaTypeImageIndex && root.MediaType != "application/vnd.docker.distribution.manifest.list.v2+json" {
		return "", fmt.Errorf("oci layout root is not an image index: %s", root.MediaType)
	}
	name, err := NormalizeTag(tag)
	if err != nil {
		return "", err
	}
	source, err := orasoci.NewWithContext(ctx, layoutPath)
	if err != nil {
		return "", fmt.Errorf("open OCI image layout: %w", err)
	}
	indexData, err := content.FetchAll(ctx, source, root)
	if err != nil {
		return "", fmt.Errorf("read OCI image index: %w", err)
	}
	var index v1.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		return "", fmt.Errorf("parse OCI image index: %w", err)
	}
	if index.SchemaVersion != 2 {
		return "", fmt.Errorf("invalid OCI image index schema version %d", index.SchemaVersion)
	}

	temporary, err := os.MkdirTemp("", "coopr-podman-index-")
	if err != nil {
		return "", fmt.Errorf("create temporary OCI layout: %w", err)
	}
	defer func() { _ = os.RemoveAll(temporary) }()

	imageIDs := make(map[digest.Digest]string, len(index.Manifests))
	for i, child := range index.Manifests {
		childLayout := filepath.Join(temporary, fmt.Sprintf("%d", i))
		childStore, err := orasoci.NewWithContext(ctx, childLayout)
		if err != nil {
			return "", fmt.Errorf("create temporary OCI child layout: %w", err)
		}
		if err := oras.CopyGraph(ctx, source, childStore, child, oras.CopyGraphOptions{}); err != nil {
			return "", fmt.Errorf("copy OCI image child %s: %w", child.Digest, err)
		}
		if err := childStore.Tag(ctx, child, child.Digest.String()); err != nil {
			return "", fmt.Errorf("anchor OCI image child %s: %w", child.Digest, err)
		}
		imageID, err := imageConfigDigest(ctx, source, child)
		if err != nil {
			return "", fmt.Errorf("identify OCI image child %s: %w", child.Digest, err)
		}
		if _, err := s.writeImage(ctx, imageID, "", func() (types.ImageReference, error) {
			return LayoutReference(childLayout, child)
		}); err != nil {
			return "", fmt.Errorf("import OCI image child %s: %w", child.Digest, err)
		}
		imageIDs[child.Digest] = imageID.Encoded()
	}
	result, err := s.WriteStoredIndex(ctx, root, indexData, imageIDs, name)
	if err != nil || name == "" {
		return result, err
	}
	return name, nil
}

var errStoredIndexRetry = errors.New("stored image index appeared during creation")

// WriteStoredIndex saves an exact index referencing child images already in the
// native store and returns its native image ID. An existing exact list is
// retagged without rewriting its data.
func (s *Store) WriteStoredIndex(ctx context.Context, root v1.Descriptor, indexData []byte, imageIDs map[digest.Digest]string, tag string) (string, error) {
	for {
		result, err := s.writeStoredIndex(ctx, "", root, indexData, imageIDs, tag)
		if !errors.Is(err, errStoredIndexRetry) {
			return result, err
		}
	}
}

// UpdateStoredIndex updates a native manifest list without replacing its ID.
// It returns the native image ID. The caller must hold the native
// manifest-list locker for imageID.
func (s *Store) UpdateStoredIndex(ctx context.Context, imageID string, root v1.Descriptor, indexData []byte, imageIDs map[digest.Digest]string, tag string) (string, error) {
	if imageID == "" {
		return "", errors.New("native image index ID is required")
	}
	return s.writeStoredIndex(ctx, imageID, root, indexData, imageIDs, tag)
}

// reuseStoredIndex takes the candidate image lock before the root lock, so
// tentative creation has completed before reuse. Creation never waits for an
// image lock while holding repository or root locks.
func (s *Store) reuseStoredIndex(ctx context.Context, root v1.Descriptor, indexData []byte, name string) (string, bool, error) {
	images, err := s.backend.ImagesByDigest(root.Digest)
	if err != nil && !errors.Is(err, storage.ErrImageUnknown) {
		return "", false, err
	}
	for _, image := range images {
		// Pulled children may retain this index as metadata. Their default is a
		// runnable manifest, so they are not reusable native list records.
		candidate, err := s.backend.ImageBigData(image.ID, storage.ImageDigestBigDataKey)
		if errors.Is(err, storage.ErrImageUnknown) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		if !bytes.Equal(candidate, indexData) {
			continue
		}
		lock, err := manifests.LockerForImage(s.backend, image.ID)
		if err != nil {
			return "", false, err
		}
		lock.Lock()
		rootLock, err := s.backend.GetDigestLock(root.Digest)
		if err != nil {
			lock.Unlock()
			return "", false, err
		}
		rootLock.Lock()
		result, found, readErr := func() (string, bool, error) {
			if _, err := s.backend.Image(image.ID); errors.Is(err, storage.ErrImageUnknown) {
				return "", false, nil
			} else if err != nil {
				return "", false, err
			}
			if err := ctx.Err(); err != nil {
				return "", false, err
			}
			data, err := s.backend.ImageBigData(image.ID, storage.ImageDigestManifestBigDataNamePrefix)
			if errors.Is(err, storage.ErrImageUnknown) {
				return "", false, nil
			}
			if err != nil {
				return "", false, err
			}
			if !bytes.Equal(data, indexData) {
				return "", false, nil
			}
			if _, _, err := manifests.LoadFromImage(s.backend, image.ID); err != nil {
				return "", false, err
			}
			if name == "" {
				return image.ID, true, nil
			}
			if err := s.backend.AddNames(image.ID, []string{name}); err != nil {
				return "", false, err
			}
			return image.ID, true, nil
		}()
		rootLock.Unlock()
		lock.Unlock()
		if readErr != nil || found {
			return result, found, readErr
		}
	}
	return "", false, nil
}

func (s *Store) writeStoredIndex(ctx context.Context, existingID string, root v1.Descriptor, indexData []byte, imageIDs map[digest.Digest]string, tag string) (_ string, retErr error) {
	if s == nil || s.backend == nil {
		return "", errors.New("image store is not initialized")
	}
	if root.MediaType != v1.MediaTypeImageIndex && root.MediaType != "application/vnd.docker.distribution.manifest.list.v2+json" {
		return "", fmt.Errorf("root is not an image index: %s", root.MediaType)
	}
	if err := root.Digest.Validate(); err != nil {
		return "", fmt.Errorf("invalid image index digest: %w", err)
	}
	if int64(len(indexData)) != root.Size || root.Digest.Algorithm().FromBytes(indexData) != root.Digest {
		return "", errors.New("image index bytes do not match root descriptor")
	}
	var index v1.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		return "", fmt.Errorf("parse image index: %w", err)
	}
	if index.SchemaVersion != 2 || index.MediaType != "" && index.MediaType != root.MediaType {
		return "", errors.New("invalid image index")
	}
	name, err := NormalizeTag(tag)
	if err != nil {
		return "", err
	}
	if existingID == "" {
		if result, found, err := s.reuseStoredIndex(ctx, root, indexData, name); err != nil || found {
			return result, err
		}
	}
	repository := name
	if repository == "" {
		// Native instance references need a repository to pin an alternate
		// manifest sharing a config ID. This internal name is not a list tag.
		repository = "coopr.internal/index/" + root.Digest.Encoded()
	}
	destination, err := imagereference.ParseNormalizedNamed(repository)
	if err != nil {
		return "", err
	}
	mutationLock, err := s.backend.GetDigestLock(digest.FromString("coopr stored-image index repository\x00" + imagereference.TrimNamed(destination).String()))
	if err != nil {
		return "", fmt.Errorf("open stored-image index repository lock: %w", err)
	}
	mutationLock.Lock()
	defer mutationLock.Unlock()
	indexLock, err := s.backend.GetDigestLock(root.Digest)
	if err != nil {
		return "", fmt.Errorf("open stored-image index digest lock: %w", err)
	}
	indexLock.Lock()
	defer indexLock.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if existingID == "" {
		images, err := s.backend.ImagesByDigest(root.Digest)
		if err != nil && !errors.Is(err, storage.ErrImageUnknown) {
			return "", fmt.Errorf("find stored image index: %w", err)
		}
		for _, image := range images {
			data, err := s.backend.ImageBigData(image.ID, storage.ImageDigestManifestBigDataNamePrefix)
			if err != nil {
				return "", fmt.Errorf("read stored image index: %w", err)
			}
			if bytes.Equal(data, indexData) {
				return "", errStoredIndexRetry
			}
		}
	}
	var addedNames []struct{ imageID, name string }
	keepNames := false
	defer func() {
		if !keepNames {
			for _, added := range addedNames {
				retErr = errors.Join(retErr, s.backend.RemoveNames(added.imageID, []string{added.name}))
			}
		}
	}()
	list := manifests.Create()
	for _, child := range index.Manifests {
		imageID := imageIDs[child.Digest]
		if imageID == "" {
			return "", fmt.Errorf("image index child %s has no stored image", child.Digest)
		}
		canonical, err := imagereference.WithDigest(imagereference.TrimNamed(destination), child.Digest)
		if err != nil {
			return "", fmt.Errorf("name stored image child %s: %w", child.Digest, err)
		}
		image, err := s.backend.Image(imageID)
		if err != nil {
			return "", fmt.Errorf("resolve stored image child %s: %w", child.Digest, err)
		}
		if !slices.Contains(image.Names, canonical.String()) {
			if existing, err := s.backend.Image(canonical.String()); err == nil {
				return "", fmt.Errorf("stored image child name %s already belongs to image %s", canonical, existing.ID)
			} else if !errors.Is(err, storage.ErrImageUnknown) {
				return "", fmt.Errorf("inspect stored image child name %s: %w", canonical, err)
			}
			if err := s.backend.AddNames(imageID, []string{canonical.String()}); err != nil {
				return "", fmt.Errorf("name stored image child %s: %w", child.Digest, err)
			}
			addedNames = append(addedNames, struct{ imageID, name string }{imageID, canonical.String()})
		}
		storageRef, err := imagestorage.Transport.NewStoreReference(s.backend, canonical, imageID)
		if err != nil {
			return "", fmt.Errorf("reference stored image child %s: %w", child.Digest, err)
		}
		// Persist a standard repository@digest reference plus native ID. An
		// immutable name keeps libimage's instance reads pinned even when the
		// child's default manifest or the destination's mutable tag changes.
		added, err := list.Add(ctx, s.system, storageRef, false)
		if err != nil {
			return "", fmt.Errorf("add stored image child %s to manifest list: %w", child.Digest, err)
		}
		if added != child.Digest {
			return "", fmt.Errorf("stored image child digest %s differs from source %s", added, child.Digest)
		}
	}
	if existingID != "" {
		previous, err := s.backend.ImageBigData(existingID, storage.ImageDigestManifestBigDataNamePrefix)
		if err != nil {
			return "", fmt.Errorf("read prior native image index: %w", err)
		}
		previousDigest := digest.FromBytes(previous)
		if err := s.backend.SetImageBigData(existingID, storage.ImageDigestManifestBigDataNamePrefix+"-"+previousDigest.String(), previous, func(data []byte) (digest.Digest, error) { return digest.FromBytes(data), nil }); err != nil {
			return "", fmt.Errorf("preserve prior native image index: %w", err)
		}
		// SaveToImage can persist part of its instance map before returning an
		// error; those native references must remain usable after an update.
		keepNames = true
	}
	indexID, err := list.SaveToImage(s.backend, existingID, nil, root.MediaType)
	if err != nil {
		return "", fmt.Errorf("save OCI image index to containers-storage: %w", err)
	}
	keepIndex := false
	defer func() {
		if !keepIndex && existingID == "" {
			_, cleanupErr := s.backend.DeleteImage(indexID, true)
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()
	// SaveToImage records the instance-to-storage references Podman needs, but
	// serializes the list again. Restore the exact source bytes before tagging
	// so a later Podman push keeps the original index digest.
	if err := s.backend.SetImageBigData(indexID, storage.ImageDigestManifestBigDataNamePrefix, indexData, func(data []byte) (digest.Digest, error) {
		return root.Digest.Algorithm().FromBytes(data), nil
	}); err != nil {
		return "", fmt.Errorf("preserve OCI image index digest in containers-storage: %w", err)
	}
	if err := s.backend.SetImageBigData(indexID, storage.ImageDigestManifestBigDataNamePrefix+"-"+root.Digest.String(), indexData, func(data []byte) (digest.Digest, error) {
		return digest.FromBytes(data), nil
	}); err != nil {
		return "", fmt.Errorf("store digest-specific image index: %w", err)
	}
	storedIndex, err := s.backend.ImageBigData(indexID, storage.ImageDigestManifestBigDataNamePrefix)
	if err != nil {
		return "", fmt.Errorf("verify OCI image index in containers-storage: %w", err)
	}
	if storedDigest := root.Digest.Algorithm().FromBytes(storedIndex); storedDigest != root.Digest {
		return "", fmt.Errorf("containers-storage image index digest %s differs from source %s", storedDigest, root.Digest)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// AddNames moves an existing tag from the previous image under the storage
	// image lock. Keep the old name intact until every new instance is present.
	if name != "" {
		if err := s.backend.AddNames(indexID, []string{name}); err != nil {
			return "", fmt.Errorf("tag OCI image index in containers-storage: %w", err)
		}
	}
	keepIndex = true
	keepNames = true
	return indexID, nil
}

func (s *Store) writeImage(ctx context.Context, imageID digest.Digest, name string, source func() (types.ImageReference, error)) (string, error) {
	if imageID.Algorithm() != digest.SHA256 {
		return "", fmt.Errorf("containers-storage image ID must be sha256, got %s", imageID.Algorithm())
	}
	src, err := source()
	if err != nil {
		return "", err
	}
	var named imagereference.Named
	result := imageID.String()
	if name != "" {
		named, err = imagereference.ParseNormalizedNamed(name)
		if err != nil {
			return "", fmt.Errorf("local image name %q: %w", name, err)
		}
		result = name
	}
	dst, err := imagestorage.Transport.NewStoreReference(s.backend, named, imageID.Encoded())
	if err != nil {
		return "", fmt.Errorf("local image destination: %w", err)
	}
	if err := Copy(ctx, s.system, src, dst, true); err != nil {
		return "", fmt.Errorf("copy OCI image to containers-storage: %w", err)
	}
	return result, nil
}

// LayoutReference selects root exactly from layoutPath's index.
func LayoutReference(layoutPath string, root v1.Descriptor) (types.ImageReference, error) {
	abs, err := filepath.Abs(layoutPath)
	if err != nil {
		return nil, fmt.Errorf("resolve OCI layout path: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(abs, "index.json"))
	if err != nil {
		return nil, fmt.Errorf("read OCI layout index: %w", err)
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("parse OCI layout index: %w", err)
	}
	for i, candidate := range index.Manifests {
		if candidate.Digest == root.Digest && candidate.MediaType == root.MediaType && candidate.Size == root.Size {
			ref, err := ocilayout.NewIndexReference(abs, i)
			if err != nil {
				return nil, fmt.Errorf("open OCI layout root: %w", err)
			}
			return ref, nil
		}
	}
	return nil, fmt.Errorf("OCI layout does not contain root %s", root.Digest)
}

// Copy transfers an image between containers/image transports using the host's
// signature policy and without an intermediate archive.
func Copy(ctx context.Context, system *types.SystemContext, src, dst types.ImageReference, preserveDigests bool) error {
	return CopyImages(ctx, system, src, dst, preserveDigests, imagecopy.CopySystemImage)
}

// CopyImages shares signature-policy handling for single images and complete
// indexes. The selection is ignored by containers/image for a single image.
func CopyImages(ctx context.Context, system *types.SystemContext, src, dst types.ImageReference, preserveDigests bool, selection imagecopy.ImageListSelection) error {
	if system == nil {
		system = &types.SystemContext{BigFilesTemporaryDir: os.TempDir()}
	}
	policy, err := signature.DefaultPolicy(system)
	if err != nil {
		return fmt.Errorf("load image policy: %w", err)
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		return fmt.Errorf("initialize image policy: %w", err)
	}
	_, copyErr := imagecopy.Image(ctx, policyContext, dst, src, &imagecopy.Options{
		SourceCtx: system, DestinationCtx: system, PreserveDigests: preserveDigests,
		ImageListSelection: selection,
	})
	closeErr := policyContext.Destroy()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return fmt.Errorf("close image policy: %w", closeErr)
	}
	return nil
}

func imageConfigDigest(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor) (digest.Digest, error) {
	if root.MediaType != v1.MediaTypeImageManifest && root.MediaType != "application/vnd.docker.distribution.manifest.v2+json" {
		return "", fmt.Errorf("OCI layout root is not a single image manifest: %s", root.MediaType)
	}
	manifestData, err := content.FetchAll(ctx, source, root)
	if err != nil {
		return "", fmt.Errorf("read image manifest: %w", err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil || manifest.SchemaVersion != 2 {
		return "", fmt.Errorf("invalid OCI image manifest: %v", err)
	}
	configData, err := content.FetchAll(ctx, source, manifest.Config)
	if err != nil {
		return "", fmt.Errorf("read image config: %w", err)
	}
	if int64(len(configData)) != manifest.Config.Size || manifest.Config.Digest != manifest.Config.Digest.Algorithm().FromBytes(configData) {
		return "", errors.New("OCI image config differs from manifest")
	}
	return manifest.Config.Digest, nil
}
