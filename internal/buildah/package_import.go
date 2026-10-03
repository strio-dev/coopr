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

	"coopr/internal/imageconfig"
	"coopr/internal/oci"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// ImportPackageSnapshot verifies a downloaded component package and imports it
// as a one-layer image in the private containers/storage store used by Buildah.
// The returned config is an exact copy of the component's authoritative raw
// OCI config and must be retained by callers which later mutate the image.
func ImportPackageSnapshot(ctx context.Context, store storage.Store, system *types.SystemContext, pkg oci.Package, tarPath string, platform v1.Platform) (string, json.RawMessage, error) {
	if ctx == nil {
		return "", nil, errors.New("package import context is nil")
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if store == nil {
		return "", nil, errors.New("package import store is nil")
	}
	if pkg.Stage == "" || tarPath == "" {
		return "", nil, errors.New("package stage and tar path are required")
	}
	if platform.OS != "linux" || platform.Architecture == "" {
		return "", nil, errors.New("package platform must specify Linux and architecture")
	}
	if pkg.Descriptor.MediaType != oci.ComponentPackageType || pkg.Descriptor.Digest.Algorithm() != digest.SHA256 || pkg.Descriptor.Digest.Validate() != nil || pkg.Descriptor.Size < 0 {
		return "", nil, errors.New("package has an invalid snapshot descriptor")
	}
	if len(pkg.Config) == 0 || len(pkg.Config) > maxPackageConfigSize {
		return "", nil, fmt.Errorf("package config size must be between 1 and %d bytes", maxPackageConfigSize)
	}
	if _, err := imageconfig.Parse(pkg.Config); err != nil {
		return "", nil, fmt.Errorf("validate package config: %w", err)
	}
	var image v1.Image
	if err := json.Unmarshal(pkg.Config, &image); err != nil {
		return "", nil, fmt.Errorf("decode package config: %w", err)
	}
	if !samePackagePlatform(image.Platform, platform) {
		return "", nil, fmt.Errorf("package config platform %s/%s does not match %s/%s", image.OS, image.Architecture, platform.OS, platform.Architecture)
	}
	if image.RootFS.Type != "layers" || len(image.RootFS.DiffIDs) != 1 || image.RootFS.DiffIDs[0] != pkg.Descriptor.Digest {
		return "", nil, errors.New("package config must describe exactly its snapshot digest")
	}

	layout, err := os.MkdirTemp("", "coopr-buildah-package-")
	if err != nil {
		return "", nil, fmt.Errorf("create package OCI layout: %w", err)
	}
	defer func() { _ = os.RemoveAll(layout) }()

	layer := pkg.Descriptor
	layer.MediaType = v1.MediaTypeImageLayer
	if err := copyVerifiedPackageBlob(ctx, tarPath, filepath.Join(layout, "blobs", layer.Digest.Algorithm().String(), layer.Digest.Encoded()), layer); err != nil {
		return "", nil, err
	}
	config := oci.Descriptor(v1.MediaTypeImageConfig, pkg.Config)
	if err := writePackageLayoutBlob(layout, config, pkg.Config); err != nil {
		return "", nil, fmt.Errorf("write package image config: %w", err)
	}
	manifestData, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    config,
		Layers:    []v1.Descriptor{layer},
	})
	if err != nil {
		return "", nil, fmt.Errorf("encode package image manifest: %w", err)
	}
	manifest := oci.Descriptor(v1.MediaTypeImageManifest, manifestData)
	if err := writePackageLayoutBlob(layout, manifest, manifestData); err != nil {
		return "", nil, fmt.Errorf("write package image manifest: %w", err)
	}
	indexData, err := json.Marshal(v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{{
			MediaType: manifest.MediaType, Digest: manifest.Digest, Size: manifest.Size,
			Platform: &platform,
		}},
	})
	if err != nil {
		return "", nil, fmt.Errorf("encode package OCI index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(layout, v1.ImageIndexFile), indexData, 0o600); err != nil {
		return "", nil, fmt.Errorf("write package OCI index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(layout, v1.ImageLayoutFile), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o600); err != nil {
		return "", nil, fmt.Errorf("write package OCI layout marker: %w", err)
	}

	imageID, err := ImportSelectedImage(ctx, store, platformSystemContext(system, platform), layout, manifest)
	if err != nil {
		return "", nil, fmt.Errorf("import package %q: %w", pkg.Stage, err)
	}
	return imageID, bytes.Clone(pkg.Config), nil
}

func copyVerifiedPackageBlob(ctx context.Context, sourcePath, destinationPath string, descriptor v1.Descriptor) error {
	info, err := os.Lstat(sourcePath)
	if err != nil {
		return fmt.Errorf("inspect package snapshot: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("package snapshot is not a regular file")
	}
	if info.Size() != descriptor.Size {
		return fmt.Errorf("package snapshot size is %d, want %d", info.Size(), descriptor.Size)
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o700); err != nil {
		return fmt.Errorf("create package OCI blob directory: %w", err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open package snapshot: %w", err)
	}
	defer func() { _ = source.Close() }()
	destination, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create package OCI layer: %w", err)
	}
	ok := false
	defer func() {
		_ = destination.Close()
		if !ok {
			_ = os.Remove(destinationPath)
		}
	}()
	verifier := descriptor.Digest.Verifier()
	written, copyErr := io.Copy(io.MultiWriter(destination, verifier), contextReader{ctx: ctx, reader: source})
	if copyErr != nil {
		return fmt.Errorf("copy package snapshot: %w", copyErr)
	}
	if written != descriptor.Size {
		return fmt.Errorf("package snapshot size is %d, want %d", written, descriptor.Size)
	}
	if !verifier.Verified() {
		return fmt.Errorf("package snapshot digest does not match %s", descriptor.Digest)
	}
	if err := destination.Sync(); err != nil {
		return fmt.Errorf("sync package OCI layer: %w", err)
	}
	if err := destination.Close(); err != nil {
		return fmt.Errorf("close package OCI layer: %w", err)
	}
	ok = true
	return nil
}

func writePackageLayoutBlob(layout string, descriptor v1.Descriptor, data []byte) error {
	path := filepath.Join(layout, "blobs", descriptor.Digest.Algorithm().String(), descriptor.Digest.Encoded())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
