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
)

// prepareStoredIndexCopySource assembles verified native members in the native
// directory transport, whose private signature API retains every signature
// format. A supplemented source exposes only the legacy signature API.
// The caller holds the stored index lock for the lifetime of this source.
func prepareStoredIndexCopySource(ctx context.Context, store storage.Store, imageID string, expected digest.Digest, system *types.SystemContext, removeSignatures bool, writer io.Writer) (_ types.ImageReference, cleanup func() error, retErr error) {
	cleanup = func() error { return nil }
	root, original, selections, err := oci.StoredImageSelections(ctx, store, imageID)
	if err != nil {
		return nil, cleanup, err
	}
	if root.Digest != expected {
		return nil, cleanup, fmt.Errorf("stored index is %s, want %s", root.Digest, expected)
	}
	var index v1.Index
	if err := json.Unmarshal(original, &index); err != nil {
		return nil, cleanup, fmt.Errorf("decode stored index: %w", err)
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
	base, err := imagestorage.Transport.NewStoreReference(store, nil, imageID)
	if err != nil {
		return nil, cleanup, err
	}
	policy, err := signature.NewPolicyContext(oci.PreparedImagePolicy(base))
	if err != nil {
		return nil, cleanup, err
	}
	// Copy only the root with the native signature API. Every member is
	// independently approved against its original source below before this
	// prepared source can be returned to the final copier.
	copiedRoot, copyErr := imagecopy.Image(ctx, policy, aggregate, base, &imagecopy.Options{
		SourceCtx: system, DestinationCtx: system,
		ImageListSelection: imagecopy.CopySpecificImages,
		RemoveSignatures:   removeSignatures, PreserveDigests: true, ReportWriter: writer,
	})
	if err := errors.Join(copyErr, policy.Destroy()); err != nil {
		return nil, cleanup, err
	}
	if !bytes.Equal(copiedRoot, original) {
		return nil, cleanup, errors.New("prepared stored index differs from original")
	}
	preserveDigests := false
	if !removeSignatures {
		state, err := oci.CaptureStoredSignatures(store, imageID)
		if err != nil {
			return nil, cleanup, err
		}
		rootSignatures, err := oci.StoredSignatureBlobs(state, root.Digest, true)
		if err != nil {
			return nil, cleanup, err
		}
		preserveDigests = len(rootSignatures) != 0
	}
	type emittedMember struct {
		descriptor v1.Descriptor
		manifest   []byte
	}
	emitted := map[digest.Digest]emittedMember{}
	changed := false
	for i, member := range index.Manifests {
		result, exists := emitted[member.Digest]
		if !exists {
			var selection oci.StoredSelection
			found := false
			for _, candidate := range selections {
				if candidate.Manifest.Digest == member.Digest && candidate.Manifest.Size == member.Size && candidate.Manifest.MediaType == member.MediaType {
					selection, found = candidate, true
					break
				}
			}
			if !found {
				return nil, cleanup, fmt.Errorf("stored index lacks manifest %s", member.Digest)
			}
			memberPath := filepath.Join(temporaryDir, fmt.Sprintf("member-%d", i))
			memberReference, err := directory.NewReference(memberPath)
			if err != nil {
				return nil, cleanup, err
			}
			manifestData, err := copyStoredIndexMember(ctx, store, imageID, selection, system, memberReference, removeSignatures, preserveDigests, writer)
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

// copyStoredIndexMember uses a native single-image copy so signatures are read
// from the selected instance, never from an unrelated synthetic index root.
func copyStoredIndexMember(ctx context.Context, store storage.Store, rootID string, selection oci.StoredSelection, system *types.SystemContext, destination types.ImageReference, removeSignatures, preserveDigests bool, writer io.Writer) (_ []byte, retErr error) {
	if selection.ImageID != rootID {
		lock, err := manifests.LockerForImage(store, selection.ImageID)
		if err != nil {
			return nil, err
		}
		lock.Lock()
		defer lock.Unlock()
	}
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
			// The root-only copy has already initialized the version marker.
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
