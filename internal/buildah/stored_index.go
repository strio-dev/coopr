package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage/manifests"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/directory"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/signature"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"oras.land/oras-go/v2/content"
)

// StoredIndexSnapshot pins the exact index and resident manifests selected by
// the caller without creating an intermediate containers/storage image record.
type StoredIndexSnapshot struct {
	Root       v1.Descriptor
	Data       []byte
	Selections map[string]oci.StoredSelection
}

// prepareStoredIndexCopySource assembles verified native members in the native
// directory transport, whose private signature API retains every signature
// format. The temporary directory is the only staging storage.
func prepareStoredIndexCopySource(ctx context.Context, store storage.Store, snapshot StoredIndexSnapshot, system *types.SystemContext, removeSignatures bool, writer io.Writer) (_ types.ImageReference, cleanup func() error, retErr error) {
	cleanup = func() error { return nil }
	root, original, selections := snapshot.Root, snapshot.Data, snapshot.Selections
	if root.Digest.Validate() != nil || root.Digest.Algorithm().FromBytes(original) != root.Digest || int64(len(original)) != root.Size {
		return nil, cleanup, errors.New("stored index snapshot differs from descriptor")
	}
	if root.MediaType != v1.MediaTypeImageIndex && root.MediaType != manifest.DockerV2ListMediaType {
		return nil, cleanup, fmt.Errorf("stored snapshot is not an image index: %s", root.MediaType)
	}
	var index v1.Index
	if err := json.Unmarshal(original, &index); err != nil {
		return nil, cleanup, fmt.Errorf("decode stored index: %w", err)
	}
	if index.SchemaVersion != 2 || index.MediaType != "" && index.MediaType != root.MediaType {
		return nil, cleanup, errors.New("invalid stored image index")
	}
	// Artifacts are native list members, but deliberately have no runnable
	// StoredSelection. Let the native list supply their references rather than
	// reconstructing them from image/platform metadata. Historical roots still
	// use the snapshot path below unless a current native list matches exactly.
	var unresolved []digest.Digest
	transformed := false
	for _, member := range index.Manifests {
		selection, found := storedIndexMemberSelection(selections, member)
		if !found {
			unresolved = append(unresolved, member.Digest)
		} else {
			transformed = transformed || !content.Equal(selection.Manifest, member)
		}
	}
	if !transformed && len(unresolved) != 0 {
		reference, release, err := currentStoredIndexReference(ctx, store, root, original, system, nil)
		if err != nil || reference != nil {
			return reference, release, err
		}
	}
	temporaryDir, err := os.MkdirTemp("", "coopr-native-index-")
	if err != nil {
		return nil, cleanup, err
	}
	cleanup = func() error { return os.RemoveAll(temporaryDir) }
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, cleanup())
			cleanup = func() error { return nil }
		}
	}()
	aggregatePath := filepath.Join(temporaryDir, "index")
	aggregate, err := directory.NewReference(aggregatePath)
	if err != nil {
		return nil, cleanup, err
	}
	if err := os.MkdirAll(aggregatePath, 0o700); err != nil {
		return nil, cleanup, err
	}
	if err := os.WriteFile(filepath.Join(aggregatePath, "version"), []byte("Directory Transport Version: 1.1\n"), 0o644); err != nil {
		return nil, cleanup, err
	}
	if err := os.WriteFile(filepath.Join(aggregatePath, "manifest.json"), original, 0o644); err != nil {
		return nil, cleanup, err
	}
	preserveDigests := false
	if !removeSignatures {
		rootSignatures, err := storedIndexSignatureBlobs(store, root.Digest)
		if err != nil {
			return nil, cleanup, err
		}
		preserveDigests = len(rootSignatures) != 0
		for i, blob := range rootSignatures {
			if err := os.WriteFile(filepath.Join(aggregatePath, fmt.Sprintf("signature-%d", i+1)), blob, 0o644); err != nil {
				return nil, cleanup, err
			}
		}
	}
	type emittedMember struct {
		descriptor v1.Descriptor
		manifest   []byte
	}
	emitted := map[digest.Digest]emittedMember{}
	if transformed && len(unresolved) != 0 {
		reference, release, err := currentStoredIndexReference(ctx, store, root, original, system, unresolved)
		if err != nil {
			return nil, cleanup, err
		}
		if reference != nil {
			// Native selective copying supplies artifacts and their signatures
			// without reading unavailable ciphertext blobs of transformed members.
			nativePath := filepath.Join(temporaryDir, "native-members")
			nativeDestination, err := directory.NewReference(nativePath)
			if err != nil {
				_ = release()
				return nil, cleanup, err
			}
			policy, err := signature.DefaultPolicy(system)
			if err != nil {
				_ = release()
				return nil, cleanup, err
			}
			policyContext, err := signature.NewPolicyContext(policy)
			if err != nil {
				_ = release()
				return nil, cleanup, err
			}
			_, copyErr := imagecopy.Image(ctx, policyContext, nativeDestination, reference, &imagecopy.Options{SourceCtx: system, DestinationCtx: system, ImageListSelection: imagecopy.CopySpecificImages, Instances: unresolved, RemoveSignatures: removeSignatures, PreserveDigests: true, ReportWriter: writer})
			if err := errors.Join(copyErr, policyContext.Destroy(), release()); err != nil {
				return nil, cleanup, err
			}
			if err := linkStoredIndexFiles(nativePath, aggregatePath, root.Digest); err != nil {
				return nil, cleanup, err
			}
			source, err := nativeDestination.NewImageSource(ctx, system)
			if err != nil {
				return nil, cleanup, err
			}
			for _, selected := range unresolved {
				raw, mediaType, err := source.GetManifest(ctx, &selected)
				if err != nil {
					return nil, cleanup, errors.Join(err, source.Close())
				}
				emitted[selected] = emittedMember{descriptor: oci.Descriptor(mediaType, raw), manifest: raw}
			}
			if err := source.Close(); err != nil {
				return nil, cleanup, err
			}
		}
	}
	changed := false
	for i, member := range index.Manifests {
		result, exists := emitted[member.Digest]
		if !exists {
			selection, found := storedIndexMemberSelection(selections, member)
			if !found {
				return nil, cleanup, fmt.Errorf("stored index lacks manifest %s", member.Digest)
			}
			memberPath := filepath.Join(temporaryDir, fmt.Sprintf("member-%d", i))
			memberReference, err := directory.NewReference(memberPath)
			if err != nil {
				return nil, cleanup, err
			}
			manifestData, err := copyStoredIndexMember(ctx, store, selection, system, memberReference, removeSignatures, preserveDigests, writer)
			if err != nil {
				return nil, cleanup, fmt.Errorf("prepare stored manifest %s: %w", member.Digest, err)
			}
			emittedDescriptor := v1.Descriptor{Digest: digest.FromBytes(manifestData), Size: int64(len(manifestData)), MediaType: manifest.GuessMIMEType(manifestData)}
			if err := linkStoredIndexFiles(memberPath, aggregatePath, emittedDescriptor.Digest); err != nil {
				return nil, cleanup, err
			}
			result = emittedMember{descriptor: emittedDescriptor, manifest: manifestData}
			emitted[member.Digest] = result
		}
		if member.Digest != result.descriptor.Digest || member.Size != result.descriptor.Size || member.MediaType != result.descriptor.MediaType {
			changed = true
			member.Digest, member.Size, member.MediaType = result.descriptor.Digest, result.descriptor.Size, result.descriptor.MediaType
			if len(member.Data) != 0 {
				member.Data = result.manifest
			}
		}
		index.Manifests[i] = member
	}
	finalData := original
	if changed {
		_, finalData, err = oci.RewriteImageIndex(root, original, index.Manifests)
		if err != nil {
			return nil, cleanup, err
		}
	}
	if !bytes.Equal(finalData, original) {
		if preserveDigests {
			return nil, cleanup, errors.New("cannot change a signed stored index without removing its signatures")
		}
		// Native directory transport stores the root at manifest.json; opening
		// another destination would erase the member blobs and signatures.
		if err := os.WriteFile(filepath.Join(aggregatePath, "manifest.json"), finalData, 0o644); err != nil {
			return nil, cleanup, err
		}
	}
	return aggregate, cleanup, nil
}

func storedIndexMemberSelection(selections map[string]oci.StoredSelection, member v1.Descriptor) (oci.StoredSelection, bool) {
	for _, selection := range selections {
		if content.Equal(selection.Manifest, member) {
			return selection, true
		}
	}
	for _, selection := range selections {
		if selection.SourceManifest != nil && content.Equal(*selection.SourceManifest, member) {
			return selection, true
		}
	}
	return oci.StoredSelection{}, false
}

// currentStoredIndexReference keeps the native list locked until the copy has
// closed its sources. The native reference reads the image's current manifest,
// so checking its bytes and holding the lock are both required for exact-root
// selection. Binding the transport also covers native artifact references.
func currentStoredIndexReference(ctx context.Context, store storage.Store, root v1.Descriptor, original []byte, system *types.SystemContext, instances []digest.Digest) (types.ImageReference, func() error, error) {
	noop := func() error { return nil }
	images, err := store.ImagesByDigest(root.Digest)
	if err != nil {
		return nil, noop, err
	}
	for _, image := range images {
		lock, err := manifests.LockerForImage(store, image.ID)
		if err != nil {
			return nil, noop, err
		}
		lock.Lock()
		raw, err := store.ImageBigData(image.ID, storage.ImageDigestBigDataKey)
		if err != nil {
			lock.Unlock()
			return nil, noop, err
		}
		if !bytes.Equal(raw, original) {
			lock.Unlock()
			continue
		}
		_, list, err := manifests.LoadFromImage(store, image.ID)
		if err != nil {
			lock.Unlock()
			return nil, noop, err
		}
		previous := imagestorage.Transport.GetStoreIfSet()
		imagestorage.Transport.SetStore(store)
		release := func() error {
			imagestorage.Transport.SetStore(previous)
			lock.Unlock()
			return nil
		}
		selection := imagecopy.CopyAllImages
		if instances != nil {
			selection = imagecopy.CopySpecificImages
		}
		reference, err := list.Reference(store, selection, instances)
		if err != nil {
			_ = release()
			return nil, noop, err
		}
		source, err := reference.NewImageSource(ctx, system)
		if err == nil {
			var data []byte
			data, _, err = source.GetManifest(ctx, nil)
			if err == nil && !bytes.Equal(data, original) {
				err = errors.New("native list reference differs from stored index snapshot")
			}
			err = errors.Join(err, source.Close())
		}
		if err != nil {
			_ = release()
			return nil, noop, err
		}
		return reference, release, nil
	}
	return nil, noop, nil
}

// storedIndexSignatureBlobs collects exact-root signatures from existing
// records, including historical roots whose native default has since changed.
func storedIndexSignatureBlobs(store storage.Store, selected digest.Digest) ([][]byte, error) {
	images, err := store.ImagesByDigest(selected)
	if err != nil {
		return nil, err
	}
	var retained [][]byte
	for _, image := range images {
		blobs, err := func() ([][]byte, error) {
			lock, err := manifests.LockerForImage(store, image.ID)
			if err != nil {
				return nil, err
			}
			lock.Lock()
			defer lock.Unlock()
			current, err := store.ImageBigData(image.ID, storage.ImageDigestBigDataKey)
			if err != nil {
				return nil, err
			}
			state, err := oci.CaptureStoredSignatures(store, image.ID)
			if err != nil {
				return nil, err
			}
			return oci.StoredSignatureBlobs(state, selected, digest.FromBytes(current) == selected)
		}()
		if err != nil {
			return nil, err
		}
		for _, blob := range blobs {
			if !slices.ContainsFunc(retained, func(existing []byte) bool { return bytes.Equal(existing, blob) }) {
				retained = append(retained, blob)
			}
		}
	}
	return retained, nil
}

// copyStoredIndexMember uses a native single-image copy so signatures are read
// from the selected instance, never from an unrelated synthetic index root.
func copyStoredIndexMember(ctx context.Context, store storage.Store, selection oci.StoredSelection, system *types.SystemContext, destination types.ImageReference, removeSignatures, preserveDigests bool, writer io.Writer) (_ []byte, retErr error) {
	lock, err := manifests.LockerForImage(store, selection.ImageID)
	if err != nil {
		return nil, err
	}
	lock.Lock()
	defer lock.Unlock()
	base, err := imagestorage.Transport.NewStoreReference(store, nil, selection.ImageID)
	if err != nil {
		return nil, err
	}
	defaultManifest, err := store.ImageBigData(selection.ImageID, storage.ImageDigestBigDataKey)
	if err != nil {
		return nil, err
	}
	memberSystem := *system
	var source types.ImageReference
	var policy *signature.PolicyContext
	var cleanup func() error
	if digest.FromBytes(defaultManifest) == selection.Manifest.Digest {
		// A native leaf's signatures may exist only in its default metadata.
		// Keep its real native reference so the copier reads that default set.
		if err := approveStoredImageExport(ctx, store, selection.ImageID, &memberSystem, nil); err != nil {
			return nil, err
		}
		source = base
		policy, err = signature.NewPolicyContext(oci.PreparedImagePolicy(base))
		if err != nil {
			return nil, err
		}
		cleanup = policy.Destroy
	} else {
		source, policy, cleanup, err = verifiedStoredCopySourceReference(ctx, store, base, &memberSystem, selection.ImageID, selection.Manifest.Digest)
		if err != nil {
			return nil, errors.Join(err, cleanup())
		}
	}
	defer func() { retErr = errors.Join(retErr, cleanup()) }()
	return imagecopy.Image(ctx, policy, destination, source, &imagecopy.Options{
		SourceCtx: &memberSystem, DestinationCtx: &memberSystem,
		ImageListSelection: imagecopy.CopySystemImage,
		RemoveSignatures:   removeSignatures, PreserveDigests: preserveDigests, ReportWriter: writer,
	})
}

// Native single-image roots become instance manifests/signatures in the
// aggregate directory. Blob filenames already use the native digest naming.
func linkStoredIndexFiles(source, destination string, instance digest.Digest) error {
	if err := instance.Validate(); err != nil {
		return err
	}
	prefix := instance.Encoded()
	if instance.Algorithm() != digest.Canonical {
		prefix = instance.Algorithm().String() + "-" + prefix
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "version" {
			data, err := os.ReadFile(filepath.Join(source, entry.Name()))
			if err != nil {
				return err
			}
			// Native directory version 1.2 supports non-SHA256 filenames.
			// The staged root initialized the version marker before member linking.
			if string(data) == "Directory Transport Version: 1.2\n" {
				if err := os.WriteFile(filepath.Join(destination, entry.Name()), data, 0o644); err != nil {
					return err
				}
			}
			continue
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unexpected prepared image entry %q", entry.Name())
		}
		name := entry.Name()
		if name == "manifest.json" {
			name = prefix + ".manifest.json"
		} else if strings.HasPrefix(name, "signature-") {
			name = prefix + "." + name
		}
		from, to := filepath.Join(source, entry.Name()), filepath.Join(destination, name)
		if err := os.Link(from, to); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return err
			}
			same, err := sameStoredIndexFile(from, to)
			if err != nil {
				return err
			}
			if !same {
				return fmt.Errorf("conflicting prepared image entry %q", entry.Name())
			}
		}
	}
	return nil
}

func sameStoredIndexFile(first, second string) (_ bool, retErr error) {
	a, err := os.Open(first)
	if err != nil {
		return false, err
	}
	defer func() { retErr = errors.Join(retErr, a.Close()) }()
	b, err := os.Open(second)
	if err != nil {
		return false, err
	}
	defer func() { retErr = errors.Join(retErr, b.Close()) }()
	firstDigest, err := digest.FromReader(a)
	if err != nil {
		return false, err
	}
	secondDigest, err := digest.FromReader(b)
	if err != nil {
		return false, err
	}
	return firstDigest == secondDigest, nil
}
