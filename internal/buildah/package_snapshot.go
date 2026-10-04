package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"coopr/internal/imageconfig"
	"coopr/internal/oci"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	storagearchive "go.podman.io/storage/pkg/archive"
	"go.podman.io/storage/pkg/chrootarchive"
)

const maxPackageConfigSize = 16 << 20

// PackageSnapshotRequest identifies one committed package-stage image and the
// existing Coopr package blob destination.
type PackageSnapshotRequest struct {
	ImageID       string
	Stage         string
	TarPath       string
	Platform      v1.Platform
	SystemContext *types.SystemContext
	// Config is the reconciled raw OCI config retained alongside the stage.
	// A Buildah commit alone loses unknown inherited config fields.
	Config           json.RawMessage
	RootMetadata     *PackageRootMetadata
	SourceDateEpoch  *int64
	Timestamp        *int64
	RewriteTimestamp bool
}

// PackageRootMetadata is captured from the mutable builder before commit;
// containers/storage uses its layer root as storage infrastructure and does
// not retain changes to / in the committed image mount.
type PackageRootMetadata struct {
	Mode       int64
	UID        int
	GID        int
	PAXRecords map[string]string
	// The archive exporter omits some xattrs. This flag is used only to
	// disqualify portable cache snapshots; ordinary package export is unchanged.
	UnportableXattrs bool
	// Host policy allocated by Buildah or independently observed in storage.
	// A different file label makes portable snapshots ineligible.
	AmbientSELinux string
}

// SnapshotPackage flattens a committed local image rootfs into the existing
// single-uncompressed-tar component package format. The source image is
// mounted read-only and the output is atomically replaced only after the tar
// digest and rewritten one-layer OCI config are complete.
func SnapshotPackage(ctx context.Context, store storage.Store, request PackageSnapshotRequest) (pkg oci.Package, retErr error) {
	if ctx == nil {
		return oci.Package{}, errors.New("package snapshot context is nil")
	}
	if err := ctx.Err(); err != nil {
		return oci.Package{}, err
	}
	if store == nil {
		return oci.Package{}, errors.New("package snapshot store is nil")
	}
	if request.ImageID == "" || request.Stage == "" || request.TarPath == "" {
		return oci.Package{}, errors.New("package image ID, stage, and tar path are required")
	}
	if request.Platform.OS != "linux" || request.Platform.Architecture == "" {
		return oci.Package{}, fmt.Errorf("package platform must specify Linux and architecture")
	}
	if len(request.Config) == 0 {
		return oci.Package{}, errors.New("package snapshot requires an authoritative raw OCI config sidecar")
	}
	logical, err := imageconfig.Parse(request.Config)
	if err != nil {
		return oci.Package{}, fmt.Errorf("validate package image config: %w", err)
	}
	var typed v1.Image
	if err := json.Unmarshal(request.Config, &typed); err != nil {
		return oci.Package{}, fmt.Errorf("decode package image config: %w", err)
	}
	if !samePackagePlatform(typed.Platform, request.Platform) {
		return oci.Package{}, fmt.Errorf("package config platform %s/%s does not match %s/%s", typed.OS, typed.Architecture, request.Platform.OS, request.Platform.Architecture)
	}
	emitted, err := packageImageConfig(ctx, store, request.ImageID, request.SystemContext)
	if err != nil {
		return oci.Package{}, err
	}
	var committed v1.Image
	if err := json.Unmarshal(emitted, &committed); err != nil {
		return oci.Package{}, fmt.Errorf("decode committed package image config: %w", err)
	}
	if typed.RootFS.Type != committed.RootFS.Type || !slices.Equal(typed.RootFS.DiffIDs, committed.RootFS.DiffIDs) {
		return oci.Package{}, errors.New("package config sidecar does not match committed rootfs provenance")
	}
	storedImage, err := store.Image(request.ImageID)
	if err != nil {
		return oci.Package{}, fmt.Errorf("inspect package image %q: %w", request.ImageID, err)
	}
	var stream io.ReadCloser
	var rootHeader *tar.Header
	if storedImage.TopLayer == "" {
		var empty bytes.Buffer
		writer := tar.NewWriter(&empty)
		if err := writer.Close(); err != nil {
			return oci.Package{}, fmt.Errorf("create empty package rootfs: %w", err)
		}
		stream = io.NopCloser(bytes.NewReader(empty.Bytes()))
	} else {
		if request.RootMetadata == nil {
			return oci.Package{}, errors.New("package root metadata captured before commit is required")
		}
		rootHeader = request.RootMetadata.tarHeader()
		layer, err := store.Layer(storedImage.TopLayer)
		if err != nil {
			return oci.Package{}, fmt.Errorf("inspect package image layer %q: %w", storedImage.TopLayer, err)
		}
		mountPoint, err := store.MountImage(request.ImageID, nil, "")
		if err != nil {
			return oci.Package{}, fmt.Errorf("mount package image %q: %w", request.ImageID, err)
		}
		defer func() {
			_, unmountErr := store.UnmountImage(request.ImageID, false)
			if unmountErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("unmount package image %q: %w", request.ImageID, unmountErr))
			}
		}()
		stream, err = chrootarchive.Tar(mountPoint, &storagearchive.TarOptions{
			Compression: storagearchive.Uncompressed, IncludeSourceDir: true,
			CopyPass: true,
			UIDMaps:  layer.UIDMap, GIDMaps: layer.GIDMap,
		}, mountPoint)
		if err != nil {
			return oci.Package{}, fmt.Errorf("archive package rootfs: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(request.TarPath), 0o755); err != nil {
		return oci.Package{}, fmt.Errorf("create package output directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(request.TarPath), ".coopr-package-*")
	if err != nil {
		return oci.Package{}, fmt.Errorf("create package output: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()

	verifier := digest.Canonical.Digester()
	var epoch *time.Time
	createdEpoch := request.SourceDateEpoch
	if request.Timestamp != nil {
		createdEpoch = request.Timestamp
	}
	if createdEpoch != nil {
		value := time.Unix(*createdEpoch, 0).UTC()
		epoch = &value
	}
	var rewriteAfter *time.Time
	if request.RewriteTimestamp {
		rewriteAfter = epoch
	}
	var forceTimestamp *time.Time
	if request.Timestamp != nil {
		forceTimestamp = epoch
	}
	size, copyErr := writePackageTar(ctx, io.MultiWriter(temporary, verifier.Hash()), stream, rootHeader, rewriteAfter, forceTimestamp)
	closeErr := stream.Close()
	if copyErr != nil {
		return oci.Package{}, fmt.Errorf("write package rootfs: %w", copyErr)
	}
	if closeErr != nil {
		return oci.Package{}, fmt.Errorf("close package rootfs archive: %w", closeErr)
	}
	if err := ctx.Err(); err != nil {
		return oci.Package{}, err
	}
	layerDigest := verifier.Digest()
	if err := logical.FlattenPackage(layerDigest, epoch); err != nil {
		return oci.Package{}, err
	}
	config, err := logical.MarshalJSON()
	if err != nil {
		return oci.Package{}, err
	}
	if err := temporary.Sync(); err != nil {
		return oci.Package{}, fmt.Errorf("sync package output: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return oci.Package{}, fmt.Errorf("close package output: %w", err)
	}
	if err := os.Rename(temporaryPath, request.TarPath); err != nil {
		return oci.Package{}, fmt.Errorf("install package output: %w", err)
	}
	return oci.Package{
		Stage: request.Stage,
		Descriptor: v1.Descriptor{
			MediaType: oci.ComponentPackageType,
			Digest:    layerDigest,
			Size:      size,
		},
		Config: config,
	}, nil
}

func (metadata *PackageRootMetadata) tarHeader() *tar.Header {
	return &tar.Header{
		Name: ".", Typeflag: tar.TypeDir, Mode: metadata.Mode,
		Uid: metadata.UID, Gid: metadata.GID,
		PAXRecords: maps.Clone(metadata.PAXRecords),
	}
}

func writePackageTar(ctx context.Context, destination io.Writer, source io.Reader, root *tar.Header, rewriteAfter, forceTimestamp *time.Time) (int64, error) {
	if root == nil && rewriteAfter == nil && forceTimestamp == nil {
		return io.Copy(destination, contextReader{ctx: ctx, reader: source})
	}
	counter := &countingWriter{writer: destination}
	reader := tar.NewReader(contextReader{ctx: ctx, reader: source})
	writer := tar.NewWriter(counter)
	rootWritten := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = writer.Close()
			return counter.count, err
		}
		if root != nil && (header.Name == "." || header.Name == "./") {
			header = root
			rootWritten = true
		}
		if rewriteAfter != nil {
			header = cloneTarHeader(header)
			clampTarTime(&header.ModTime, *rewriteAfter)
			clampTarTime(&header.AccessTime, *rewriteAfter)
			clampTarTime(&header.ChangeTime, *rewriteAfter)
		}
		if forceTimestamp != nil {
			header = cloneTarHeader(header)
			header.ModTime = *forceTimestamp
			header.AccessTime = time.Time{}
			header.ChangeTime = time.Time{}
			delete(header.PAXRecords, "mtime")
			delete(header.PAXRecords, "atime")
			delete(header.PAXRecords, "ctime")
		}
		if err := writer.WriteHeader(header); err != nil {
			return counter.count, err
		}
		if _, err := io.Copy(writer, contextReader{ctx: ctx, reader: reader}); err != nil {
			return counter.count, err
		}
	}
	if root != nil && !rootWritten {
		return counter.count, errors.New("package archive omitted root directory metadata")
	}
	if err := writer.Close(); err != nil {
		return counter.count, err
	}
	return counter.count, nil
}

func cloneTarHeader(header *tar.Header) *tar.Header {
	cloned := *header
	cloned.PAXRecords = maps.Clone(header.PAXRecords)
	return &cloned
}

func clampTarTime(value *time.Time, maximum time.Time) {
	if !value.IsZero() && value.After(maximum) {
		*value = maximum
	}
}

type countingWriter struct {
	writer io.Writer
	count  int64
}

func (writer *countingWriter) Write(data []byte) (int, error) {
	written, err := writer.writer.Write(data)
	writer.count += int64(written)
	return written, err
}

func packageImageConfig(ctx context.Context, store storage.Store, imageID string, system *types.SystemContext) ([]byte, error) {
	reference, err := imagestorage.Transport.NewStoreReference(store, nil, imageID)
	if err != nil {
		return nil, fmt.Errorf("open package image %q: %w", imageID, err)
	}
	source, err := reference.NewImageSource(ctx, system)
	if err != nil {
		return nil, fmt.Errorf("open package image source %q: %w", imageID, err)
	}
	defer func() { _ = source.Close() }()
	manifestData, mediaType, err := source.GetManifest(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("read package image manifest %q: %w", imageID, err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, fmt.Errorf("decode package image manifest %q: %w", imageID, err)
	}
	validManifest := mediaType == "" || mediaType == v1.MediaTypeImageManifest || mediaType == dockerManifestMediaType
	validDeclaredManifest := manifest.MediaType == "" || manifest.MediaType == v1.MediaTypeImageManifest || manifest.MediaType == dockerManifestMediaType
	validConfig := manifest.Config.MediaType == v1.MediaTypeImageConfig || manifest.Config.MediaType == dockerImageConfigMediaType
	if !validManifest || !validDeclaredManifest || manifest.SchemaVersion != 2 || !validConfig {
		return nil, fmt.Errorf("package image %q has unsupported manifest or config media types %q and %q", imageID, mediaType, manifest.Config.MediaType)
	}
	if manifest.Config.Size < 0 || manifest.Config.Size > maxPackageConfigSize || manifest.Config.Digest.Algorithm() != digest.SHA256 || manifest.Config.Digest.Validate() != nil {
		return nil, fmt.Errorf("package image %q has invalid config descriptor", imageID)
	}
	stream, reportedSize, err := source.GetBlob(ctx, types.BlobInfo{
		Digest: manifest.Config.Digest, Size: manifest.Config.Size, MediaType: manifest.Config.MediaType,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("read package image config %q: %w", imageID, err)
	}
	defer func() { _ = stream.Close() }()
	if reportedSize >= 0 && reportedSize != manifest.Config.Size {
		return nil, fmt.Errorf("package image config reported size %d, want %d", reportedSize, manifest.Config.Size)
	}
	data, err := io.ReadAll(io.LimitReader(stream, maxPackageConfigSize+1))
	if err != nil {
		return nil, fmt.Errorf("read package image config %q: %w", imageID, err)
	}
	if int64(len(data)) != manifest.Config.Size || int64(len(data)) > maxPackageConfigSize {
		return nil, fmt.Errorf("package image config size %d, want %d", len(data), manifest.Config.Size)
	}
	if actual := digest.FromBytes(data); actual != manifest.Config.Digest {
		return nil, fmt.Errorf("package image config digest %s, want %s", actual, manifest.Config.Digest)
	}
	// Buildah omits rootfs from a zero-layer Docker schema-2 configuration.
	// Coopr's executor-neutral model requires the equivalent explicit empty
	// rootfs so it can reconcile provenance without weakening validation for
	// Docker images that actually contain layers.
	if manifest.Config.MediaType == dockerImageConfigMediaType && len(manifest.Layers) == 0 {
		var document map[string]json.RawMessage
		if err := json.Unmarshal(data, &document); err != nil {
			return nil, fmt.Errorf("decode zero-layer Docker image config: %w", err)
		}
		var rootfs map[string]json.RawMessage
		if raw := document["rootfs"]; len(raw) != 0 {
			if err := json.Unmarshal(raw, &rootfs); err != nil {
				return nil, fmt.Errorf("decode zero-layer Docker rootfs: %w", err)
			}
		} else {
			rootfs = map[string]json.RawMessage{"type": json.RawMessage(`"layers"`)}
		}
		if _, ok := rootfs["diff_ids"]; !ok {
			rootfs["diff_ids"] = json.RawMessage(`[]`)
		}
		document["rootfs"], err = json.Marshal(rootfs)
		if err != nil {
			return nil, fmt.Errorf("encode zero-layer Docker rootfs: %w", err)
		}
		data, err = json.Marshal(document)
		if err != nil {
			return nil, fmt.Errorf("normalize zero-layer Docker image config: %w", err)
		}
	}
	return data, nil
}

func samePackagePlatform(actual, expected v1.Platform) bool {
	normalizeVariant := func(platform v1.Platform) string {
		if platform.Architecture == "arm64" && (platform.Variant == "" || platform.Variant == "v8") {
			return ""
		}
		return platform.Variant
	}
	if actual.OS != expected.OS || actual.Architecture != expected.Architecture || normalizeVariant(actual) != normalizeVariant(expected) || actual.OSVersion != expected.OSVersion {
		return false
	}
	actualFeatures := slices.Clone(actual.OSFeatures)
	expectedFeatures := slices.Clone(expected.OSFeatures)
	slices.Sort(actualFeatures)
	slices.Sort(expectedFeatures)
	return slices.Equal(actualFeatures, expectedFeatures)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
