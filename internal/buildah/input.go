package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"coopr/internal/imagestore"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	ocilayout "go.podman.io/image/v5/oci/layout"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

const (
	dockerManifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"
	maxImageMetadataSize    = 16 << 20
)

// ImportSelectedImage copies one verified image manifest from an OCI layout
// into native containers/storage. selected does not
// need to be listed in the layout's index; this is required for a platform
// manifest selected from a multi-platform image.
//
// The returned value is the local image ID accepted by Buildah with PullNever.
func ImportSelectedImage(ctx context.Context, store storage.Store, system *types.SystemContext, layoutPath string, selected v1.Descriptor) (string, error) {
	if store == nil {
		return "", errors.New("nil containers/storage store")
	}
	if err := validateSelectedManifest(selected); err != nil {
		return "", err
	}
	layout, err := filepath.Abs(layoutPath)
	if err != nil {
		return "", fmt.Errorf("resolve Coopr OCI layout: %w", err)
	}
	layout, err = filepath.EvalSymlinks(layout)
	if err != nil {
		return "", fmt.Errorf("resolve Coopr OCI layout links: %w", err)
	}
	manifestData, err := readVerifiedLayoutBlob(layout, selected)
	if err != nil {
		return "", fmt.Errorf("read selected image manifest: %w", err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return "", fmt.Errorf("decode selected image manifest: %w", err)
	}
	if manifest.SchemaVersion != 2 {
		return "", fmt.Errorf("selected image manifest has schemaVersion %d, want 2", manifest.SchemaVersion)
	}
	if manifest.Config.MediaType != v1.MediaTypeImageConfig && manifest.Config.MediaType != "application/vnd.docker.container.image.v1+json" {
		return "", fmt.Errorf("selected image config has unsupported media type %q", manifest.Config.MediaType)
	}
	if manifest.Config.Digest.Algorithm() != digest.SHA256 {
		return "", fmt.Errorf("containers/storage image config must use sha256, got %s", manifest.Config.Digest.Algorithm())
	}
	if _, err := readVerifiedLayoutBlob(layout, manifest.Config); err != nil {
		return "", fmt.Errorf("read selected image config: %w", err)
	}

	view, err := selectedLayoutView(layout, selected)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(view) }()
	src, err := ocilayout.NewIndexReference(view, 0)
	if err != nil {
		return "", fmt.Errorf("open selected OCI image view: %w", err)
	}
	imageID := manifest.Config.Digest.Encoded()
	dst, err := imagestorage.Transport.NewStoreReference(store, nil, imageID)
	if err != nil {
		return "", fmt.Errorf("create Buildah input destination: %w", err)
	}
	if err := imagestore.Copy(ctx, system, src, dst, true); err != nil {
		return "", fmt.Errorf("copy selected image into Buildah store: %w", err)
	}
	return imageID, nil
}

func validateSelectedManifest(selected v1.Descriptor) error {
	if selected.MediaType != v1.MediaTypeImageManifest && selected.MediaType != dockerManifestMediaType {
		return fmt.Errorf("selected image has unsupported media type %q", selected.MediaType)
	}
	if err := selected.Digest.Validate(); err != nil {
		return fmt.Errorf("selected image has invalid digest %q: %w", selected.Digest, err)
	}
	if selected.Size < 0 {
		return fmt.Errorf("selected image has invalid size %d", selected.Size)
	}
	return nil
}

func readVerifiedLayoutBlob(layout string, descriptor v1.Descriptor) ([]byte, error) {
	if err := descriptor.Digest.Validate(); err != nil {
		return nil, fmt.Errorf("invalid blob digest %q: %w", descriptor.Digest, err)
	}
	if descriptor.Size < 0 || descriptor.Size > maxImageMetadataSize {
		return nil, fmt.Errorf("OCI metadata blob %s has invalid size %d (limit %d)", descriptor.Digest, descriptor.Size, maxImageMetadataSize)
	}
	path := filepath.Join(layout, "blobs", descriptor.Digest.Algorithm().String(), descriptor.Digest.Encoded())
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("OCI blob is not a regular file: %s", path)
	}
	if info.Size() > maxImageMetadataSize {
		return nil, fmt.Errorf("OCI metadata blob %s is %d bytes (limit %d)", descriptor.Digest, info.Size(), maxImageMetadataSize)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxImageMetadataSize+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > maxImageMetadataSize {
		return nil, fmt.Errorf("OCI metadata blob %s exceeds limit %d", descriptor.Digest, maxImageMetadataSize)
	}
	if int64(len(data)) != descriptor.Size {
		return nil, fmt.Errorf("OCI blob %s has size %d, want %d", descriptor.Digest, len(data), descriptor.Size)
	}
	if got := descriptor.Digest.Algorithm().FromBytes(data); got != descriptor.Digest {
		return nil, fmt.Errorf("OCI blob content digest is %s, want %s", got, descriptor.Digest)
	}
	return data, nil
}

func selectedLayoutView(layout string, selected v1.Descriptor) (string, error) {
	view, err := os.MkdirTemp("", "coopr-buildah-input-")
	if err != nil {
		return "", fmt.Errorf("create selected OCI image view: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(view)
		}
	}()
	if err := os.Symlink(filepath.Join(layout, "blobs"), filepath.Join(view, "blobs")); err != nil {
		return "", fmt.Errorf("link selected OCI image blobs: %w", err)
	}
	indexData, err := json.Marshal(v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{selected},
	})
	if err != nil {
		return "", fmt.Errorf("encode selected OCI image index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(view, v1.ImageIndexFile), indexData, 0o600); err != nil {
		return "", fmt.Errorf("write selected OCI image index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(view, v1.ImageLayoutFile), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o600); err != nil {
		return "", fmt.Errorf("write selected OCI layout marker: %w", err)
	}
	ok = true
	return view, nil
}
