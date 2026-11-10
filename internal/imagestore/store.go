// Package imagestore operates on images in borrowed containers-storage handles.
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

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/localstore"
	"github.com/strio-dev/coopr/internal/oci"
	"go.podman.io/common/libimage/manifests"
	imagecopy "go.podman.io/image/v5/copy"
	imagereference "go.podman.io/image/v5/docker/reference"
	ocilayout "go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/signature"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	storagetypes "go.podman.io/storage/types"
	"oras.land/oras-go/v2/content"
)

// Store provides image operations against containers/storage.
type Store struct {
	backend storage.Store
	system  *types.SystemContext
}

// FromStore borrows a native handle for image operations. Its caller owns the
// handle and must keep it open for the entire operation.
func FromStore(backend storage.Store) *Store {
	return &Store{backend: backend, system: &types.SystemContext{BigFilesTemporaryDir: os.TempDir()}}
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
	if actual := oci.Descriptor(mediaType, data); !content.Equal(actual, selected) {
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

func (s *Store) MatchedName(name string) (string, error) {
	if s == nil || s.backend == nil {
		return "", errors.New("image store is not initialized")
	}
	return oci.StoredImageName(s.backend, name)
}

// DefaultStoreOptions loads the effective Podman containers/storage
// configuration at call time, preserving native rootless and configured paths.
func DefaultStoreOptions() (storagetypes.StoreOptions, error) {
	// DefaultStoreOptions caches its first result. The CLI may have entered a
	// user namespace after package initialization, so load the current rootless
	// paths at the moment the native store is opened.
	options, err := storagetypes.LoadStoreOptions(storagetypes.LoadOptions{})
	if err != nil {
		return storagetypes.StoreOptions{}, fmt.Errorf("load containers-storage options: %w", err)
	}
	return options, nil
}

// NormalizeTag resolves Podman's local-name and latest defaults.
func NormalizeTag(tag string) (string, error) {
	return localstore.NormalizeNativeImageTag(tag)
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
	if existingID != "" {
		// Load native instance locations and artifact payloads rather than rebuilding
		// their private metadata from the runnable image selections.
		_, list, err = manifests.LoadFromImage(s.backend, existingID)
		if err != nil {
			return "", fmt.Errorf("load native image index: %w", err)
		}
		instances := list.Instances()
		slices.Sort(instances)
		for _, instance := range slices.Compact(instances) {
			if !slices.ContainsFunc(index.Manifests, func(child v1.Descriptor) bool { return child.Digest == instance }) {
				if err := list.Remove(instance); err != nil {
					return "", fmt.Errorf("remove replaced image index child %s: %w", instance, err)
				}
			}
		}
	}
	for _, child := range index.Manifests {
		if slices.Contains(list.Instances(), child.Digest) {
			continue
		}
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
		if content.Equal(candidate, root) {
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
