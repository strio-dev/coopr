package oci

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// ArchiveRoot returns the single rooted image or index in an OCI archive.
// Content copying verifies the graph separately, before it becomes visible.
func ArchiveRoot(path string) (v1.Descriptor, error) { return archiveRoot(path) }

// ArchiveImageConfigDigest returns the image ID recorded by a single-image OCI
// archive. The manifest and config are checked against their descriptors before
// the digest is used to identify an image in local container storage.
func ArchiveImageConfigDigest(ctx context.Context, path string) (digest.Digest, error) {
	root, err := archiveRoot(path)
	if err != nil {
		return "", err
	}
	if root.MediaType != v1.MediaTypeImageManifest && root.MediaType != dockerManifestType {
		return "", fmt.Errorf("OCI archive root is not a single image manifest: %s", root.MediaType)
	}
	source, err := orasoci.NewFromTar(ctx, path)
	if err != nil {
		return "", fmt.Errorf("open OCI archive: %w", err)
	}
	manifestData, err := readLayerMetadata(ctx, source, root)
	if err != nil {
		return "", fmt.Errorf("read image manifest: %w", err)
	}
	if int64(len(manifestData)) != root.Size || root.Digest != root.Digest.Algorithm().FromBytes(manifestData) {
		return "", fmt.Errorf("OCI image manifest differs from index")
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil || manifest.SchemaVersion != 2 {
		return "", fmt.Errorf("invalid OCI image manifest: %v", err)
	}
	if err := checkDescriptor(manifest.Config); err != nil {
		return "", fmt.Errorf("invalid image config descriptor: %w", err)
	}
	configData, err := readLayerMetadata(ctx, source, manifest.Config)
	if err != nil {
		return "", fmt.Errorf("read image config: %w", err)
	}
	if int64(len(configData)) != manifest.Config.Size || manifest.Config.Digest != manifest.Config.Digest.Algorithm().FromBytes(configData) {
		return "", fmt.Errorf("OCI image config differs from manifest")
	}
	return manifest.Config.Digest, nil
}
