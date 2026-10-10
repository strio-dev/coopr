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

	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	buildahdocker "go.podman.io/buildah/docker"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/image"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/pkg/blobinfocache"
	"go.podman.io/image/v5/pkg/blobinfocache/none"
	"go.podman.io/image/v5/signature"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

const dockerImageConfigMediaType = "application/vnd.docker.container.image.v1+json"

var errRetainedStorageBlobUnavailable = errors.New("exact compressed storage blob is unavailable")

// Approve the selected original before exporting either retained or normalized
// bytes. Generated layouts cannot carry the original source's signature identity.
func approveStoredImageExport(ctx context.Context, store storage.Store, imageID string, system *types.SystemContext, selected *digest.Digest) (retErr error) {
	reference, err := imagestorage.Transport.NewStoreReference(store, nil, imageID)
	if err != nil {
		return err
	}
	source, err := reference.NewImageSource(ctx, system)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, source.Close()) }()
	policy, err := signature.DefaultPolicy(system)
	if err != nil {
		return err
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, policyContext.Destroy()) }()
	allowed, err := policyContext.IsRunningImageAllowed(ctx, image.UnparsedInstance(source, selected))
	if err != nil {
		return err
	}
	if !allowed {
		return errors.New("signature policy rejects stored image")
	}
	return nil
}

// exportStoredImageRaw exports a Docker schema-2 image without asking
// containers/image to convert its manifest or configuration to OCI media
// types. Every byte read from containers/storage is verified against the
// descriptor that names it before the completed layout is made visible.
func exportStoredImageRaw(ctx context.Context, store storage.Store, imageID string, output Output, system *types.SystemContext) (result Result, retErr error) {
	return exportStoredImageVariantRaw(ctx, store, imageID, output, system, nil)
}

func exportStoredImageVariantRaw(ctx context.Context, store storage.Store, imageID string, output Output, system *types.SystemContext, selected *digest.Digest) (result Result, retErr error) {
	if store == nil {
		return Result{}, errors.New("nil containers/storage store")
	}
	if imageID == "" {
		return Result{}, errors.New("stored image ID is required")
	}
	if output.Path == "" || !filepath.IsAbs(output.Path) {
		return Result{}, fmt.Errorf("OCI layout output must be an absolute path: %q", output.Path)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	reference, err := imagestorage.Transport.NewStoreReference(store, nil, imageID)
	if err != nil {
		return Result{}, fmt.Errorf("create stored image reference: %w", err)
	}
	source, err := reference.NewImageSource(ctx, system)
	if err != nil {
		return Result{}, fmt.Errorf("open stored image: %w", err)
	}
	defer func() {
		if closeErr := source.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close stored image source: %w", closeErr))
		}
	}()

	retained, err := retainedLayerIDs(store, imageID)
	if err != nil {
		return Result{}, err
	}
	result, err = exportRawImageSource(ctx, source, imageID, output, selected, false, blobinfocache.DefaultCache(system), store, retained)
	if !errors.Is(err, errRetainedStorageBlobUnavailable) {
		return result, err
	}
	// Native storage retains filesystem layers, not every compressed source
	// representation. Let the native copier describe the bytes it can export.
	return exportStoredImageNative(ctx, store, reference, source, imageID, output, system, selected)
}

func exportStoredImageNative(ctx context.Context, store storage.Store, reference types.ImageReference, source types.ImageSource, imageID string, output Output, system *types.SystemContext, selected *digest.Digest) (_ Result, retErr error) {
	original, mediaType, err := source.GetManifest(ctx, selected)
	if err != nil {
		return Result{}, err
	}
	var before v1.Manifest
	if err := json.Unmarshal(original, &before); err != nil {
		return Result{}, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(output.Path), ".coopr-native-layout-*")
	if err != nil {
		return Result{}, err
	}
	defer func() {
		if staging != "" {
			retErr = errors.Join(retErr, os.RemoveAll(staging))
		}
	}()
	destination, err := layout.NewReference(staging, output.Reference)
	if err != nil {
		return Result{}, err
	}
	copySystem := types.SystemContext{}
	if system != nil {
		copySystem = *system
	}
	copySystem.OCIAcceptUncompressedLayers = true
	selectedDigest := digest.FromBytes(original)
	var cleanup func() error
	var policy *signature.PolicyContext
	reference, policy, cleanup, err = verifiedStoredCopySourceReference(ctx, store, reference, &copySystem, imageID, selectedDigest)
	defer func() { retErr = errors.Join(retErr, cleanup()) }()
	if err != nil {
		return Result{}, err
	}
	// Internal layouts do not carry registry signatures. Verify the native
	// source policy, then omit signatures tied to the old representation.
	data, err := imagecopy.Image(ctx, policy, destination, reference, &imagecopy.Options{SourceCtx: &copySystem, DestinationCtx: &copySystem, ForceManifestMIMEType: mediaType, ImageListSelection: imagecopy.CopySystemImage, RemoveSignatures: true})
	if err != nil {
		return Result{}, fmt.Errorf("export native image: %w", err)
	}
	var after v1.Manifest
	if err := json.Unmarshal(data, &after); err != nil {
		return Result{}, err
	}
	if after.Config.Digest != before.Config.Digest || after.Config.Size != before.Config.Size || len(after.Layers) != len(before.Layers) {
		return Result{}, errors.New("native export changed image configuration or layer count")
	}
	root, err := oci.LayoutRoot(staging)
	if err != nil {
		return Result{}, err
	}
	if root.Digest != digest.FromBytes(data) || root.MediaType != mediaType {
		return Result{}, errors.New("native export layout differs from copied manifest")
	}
	// Register the emitted representation for subsequent stage/cache consumers.
	// This stores manifest metadata only; native filesystem layers already exist.
	if err := store.SetImageBigData(imageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+root.Digest.String(), data, manifest.Digest); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := os.Rename(staging, output.Path); err != nil {
		return Result{}, err
	}
	staging = ""
	return Result{ImageID: imageID, ManifestDigest: root.Digest.String(), Layout: output.Path, Reference: output.Reference}, nil
}

func exportRawDockerSource(ctx context.Context, source types.ImageSource, imageID string, output Output) (result Result, retErr error) {
	return exportRawImageSource(ctx, source, imageID, output, nil, true, none.NoCache, nil, nil)
}

func exportRawImageSource(ctx context.Context, source types.ImageSource, imageID string, output Output, selected *digest.Digest, requireDocker bool, cache types.BlobInfoCache, store storage.Store, retained map[digest.Digest]string) (result Result, retErr error) {
	manifestData, mediaType, err := source.GetManifest(ctx, selected)
	if err != nil {
		return Result{}, fmt.Errorf("read stored image manifest: %w", err)
	}
	if len(manifestData) > maxImageMetadataSize {
		return Result{}, fmt.Errorf("stored image manifest exceeds limit %d", maxImageMetadataSize)
	}
	manifestDigest := digest.FromBytes(manifestData)
	if selected != nil && manifestDigest != *selected {
		return Result{}, fmt.Errorf("stored image manifest digest %s differs from selected %s", manifestDigest, *selected)
	}
	if requireDocker && mediaType != dockerManifestMediaType {
		return Result{}, fmt.Errorf("stored image manifest has media type %q, want %q", mediaType, dockerManifestMediaType)
	}
	if mediaType != dockerManifestMediaType && mediaType != v1.MediaTypeImageManifest {
		return Result{}, fmt.Errorf("stored image manifest has unsupported media type %q", mediaType)
	}

	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return Result{}, fmt.Errorf("decode stored Docker manifest: %w", err)
	}
	if manifest.SchemaVersion != 2 {
		return Result{}, fmt.Errorf("stored Docker manifest has schemaVersion %d, want 2", manifest.SchemaVersion)
	}
	if manifest.MediaType != "" && manifest.MediaType != mediaType {
		return Result{}, fmt.Errorf("stored Docker manifest declares media type %q", manifest.MediaType)
	}
	wantConfigMediaType := v1.MediaTypeImageConfig
	if mediaType == dockerManifestMediaType {
		wantConfigMediaType = dockerImageConfigMediaType
	}
	if manifest.Config.MediaType != wantConfigMediaType {
		return Result{}, fmt.Errorf("stored image config has media type %q, want %q", manifest.Config.MediaType, wantConfigMediaType)
	}
	if err := validateRawDescriptor(manifest.Config, true); err != nil {
		return Result{}, fmt.Errorf("invalid stored Docker config descriptor: %w", err)
	}
	for index, layer := range manifest.Layers {
		if err := validateRawDescriptor(layer, false); err != nil {
			return Result{}, fmt.Errorf("invalid stored Docker layer %d descriptor: %w", index, err)
		}
	}

	parent := filepath.Dir(output.Path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return Result{}, fmt.Errorf("create OCI layout parent: %w", err)
	}
	if _, err := os.Lstat(output.Path); err == nil {
		return Result{}, fmt.Errorf("OCI layout output already exists: %s", output.Path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("inspect OCI layout output: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".coopr-raw-layout-*")
	if err != nil {
		return Result{}, fmt.Errorf("create staged OCI layout: %w", err)
	}
	defer func() {
		if staging != "" {
			retErr = errors.Join(retErr, os.RemoveAll(staging))
		}
	}()
	target, err := orasoci.NewWithContext(ctx, staging)
	if err != nil {
		return Result{}, fmt.Errorf("open staged OCI layout: %w", err)
	}

	for _, descriptor := range append([]v1.Descriptor{manifest.Config}, manifest.Layers...) {
		if err := pushRawSourceBlob(ctx, target, source, descriptor, cache, store, retained); err != nil {
			return Result{}, err
		}
	}
	root := v1.Descriptor{
		MediaType: mediaType,
		Digest:    manifestDigest,
		Size:      int64(len(manifestData)),
	}
	if err := target.Push(ctx, root, bytes.NewReader(manifestData)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return Result{}, fmt.Errorf("write raw Docker manifest: %w", err)
	}
	if output.Reference != "" {
		if err := target.Tag(ctx, root, output.Reference); err != nil {
			return Result{}, fmt.Errorf("tag raw Docker manifest: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := os.Rename(staging, output.Path); err != nil {
		return Result{}, fmt.Errorf("publish OCI layout: %w", err)
	}
	staging = ""
	return Result{
		ImageID: imageID, ManifestDigest: root.Digest.String(),
		Layout: output.Path, Reference: output.Reference,
	}, nil
}

func validateRawDescriptor(descriptor v1.Descriptor, metadata bool) error {
	if descriptor.MediaType == "" {
		return errors.New("media type is required")
	}
	if descriptor.Size < 0 {
		return fmt.Errorf("negative size %d", descriptor.Size)
	}
	if metadata && descriptor.Size > maxImageMetadataSize {
		return fmt.Errorf("size %d exceeds metadata limit %d", descriptor.Size, maxImageMetadataSize)
	}
	if err := descriptor.Digest.Validate(); err != nil {
		return fmt.Errorf("invalid digest %q: %w", descriptor.Digest, err)
	}
	return nil
}

func pushRawSourceBlob(ctx context.Context, target *orasoci.Store, source types.ImageSource, descriptor v1.Descriptor, cache types.BlobInfoCache, store storage.Store, retained map[digest.Digest]string) error {
	exists, err := target.Exists(ctx, descriptor)
	if err != nil {
		return fmt.Errorf("check raw image blob %s: %w", descriptor.Digest, err)
	}
	if exists {
		return nil
	}
	stream, reportedSize, err := source.GetBlob(ctx, types.BlobInfo{
		Digest: descriptor.Digest, Size: descriptor.Size, MediaType: descriptor.MediaType,
	}, cache)
	if errors.Is(err, os.ErrNotExist) && store != nil {
		stream, reportedSize, err = retainedLayerBlob(store, descriptor, retained)
	}
	if err != nil {
		return fmt.Errorf("read raw image blob %s: %w", descriptor.Digest, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = stream.Close()
		}
	}()
	if reportedSize >= 0 && reportedSize != descriptor.Size {
		return fmt.Errorf("raw image blob %s reported size %d, want %d", descriptor.Digest, reportedSize, descriptor.Size)
	}
	reader := &dockerExportContextReader{ctx: ctx, reader: stream}
	pushErr := target.Push(ctx, descriptor, reader)
	closeErr := stream.Close()
	closed = true
	if pushErr != nil {
		err := fmt.Errorf("write verified raw image blob %s: %w", descriptor.Digest, pushErr)
		if closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close raw image blob %s: %w", descriptor.Digest, closeErr))
		}
		return err
	}
	if closeErr != nil {
		return fmt.Errorf("close raw image blob %s: %w", descriptor.Digest, closeErr)
	}
	return nil
}

func retainedLayerBlob(store storage.Store, descriptor v1.Descriptor, retained map[digest.Digest]string) (io.ReadCloser, int64, error) {
	layerID := retained[descriptor.Digest]
	if layerID == "" {
		return nil, 0, fmt.Errorf("%w for %s (%s)", errRetainedStorageBlobUnavailable, descriptor.Digest, descriptor.MediaType)
	}
	stream, err := store.LayerBigData(layerID, descriptor.Digest.String())
	if err != nil {
		return nil, 0, err
	}
	return stream, descriptor.Size, nil
}

func retainedLayerIDs(store storage.Store, imageID string) (map[digest.Digest]string, error) {
	data, err := store.ImageBigData(imageID, retainedLayerMapBigData)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var stored map[string]string
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("decode exact stored layer map: %w", err)
	}
	result := map[digest.Digest]string{}
	for value, layerID := range stored {
		parsed, err := digest.Parse(value)
		if err != nil {
			return nil, err
		}
		result[parsed] = layerID
	}
	return result, nil
}

type dockerExportContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func convertLayoutToDockerSchema2(ctx context.Context, layout string) (v1.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return v1.Descriptor{}, err
	}
	root, err := oci.LayoutRoot(layout)
	if err != nil {
		return v1.Descriptor{}, err
	}
	manifestData, err := readVerifiedLayoutBlob(layout, root)
	if err != nil {
		return v1.Descriptor{}, fmt.Errorf("read converted image manifest: %w", err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return v1.Descriptor{}, fmt.Errorf("decode converted image manifest: %w", err)
	}
	manifest.MediaType = dockerManifestMediaType
	manifest.Config.MediaType = dockerImageConfigMediaType
	for index := range manifest.Layers {
		switch manifest.Layers[index].MediaType {
		case v1.MediaTypeImageLayer:
			manifest.Layers[index].MediaType = buildahdocker.V2S2MediaTypeUncompressedLayer
		case v1.MediaTypeImageLayerGzip:
			manifest.Layers[index].MediaType = "application/vnd.docker.image.rootfs.diff.tar.gzip"
		default:
			return v1.Descriptor{}, fmt.Errorf("docker schema 2 does not support layer media type %q", manifest.Layers[index].MediaType)
		}
	}
	manifestData, err = json.Marshal(manifest)
	if err != nil {
		return v1.Descriptor{}, err
	}
	converted := v1.Descriptor{MediaType: dockerManifestMediaType, Digest: digest.FromBytes(manifestData), Size: int64(len(manifestData))}
	blobPath := filepath.Join(layout, "blobs", converted.Digest.Algorithm().String(), converted.Digest.Encoded())
	if err := os.WriteFile(blobPath, manifestData, 0o600); err != nil {
		return v1.Descriptor{}, fmt.Errorf("write Docker schema 2 manifest: %w", err)
	}
	indexPath := filepath.Join(layout, v1.ImageIndexFile)
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		return v1.Descriptor{}, err
	}
	var index v1.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		return v1.Descriptor{}, fmt.Errorf("decode image layout index: %w", err)
	}
	if len(index.Manifests) != 1 {
		return v1.Descriptor{}, errors.New("image layout index does not contain exactly one manifest")
	}
	converted.Annotations = index.Manifests[0].Annotations
	converted.Platform = index.Manifests[0].Platform
	index.Manifests[0] = converted
	indexData, err = json.Marshal(index)
	if err != nil {
		return v1.Descriptor{}, err
	}
	temporary, err := os.CreateTemp(layout, ".coopr-docker-index-*.json")
	if err != nil {
		return v1.Descriptor{}, err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := temporary.Write(indexData); err != nil {
		_ = temporary.Close()
		return v1.Descriptor{}, err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return v1.Descriptor{}, err
	}
	if err := temporary.Close(); err != nil {
		return v1.Descriptor{}, err
	}
	if err := ctx.Err(); err != nil {
		return v1.Descriptor{}, err
	}
	if err := os.Rename(temporaryPath, indexPath); err != nil {
		return v1.Descriptor{}, err
	}
	return converted, nil
}

func (reader *dockerExportContextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
