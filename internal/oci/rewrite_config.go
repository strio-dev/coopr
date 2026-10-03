package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

type verifiedImageLayout struct {
	index         map[string]json.RawMessage
	root          v1.Descriptor
	store         *orasoci.Store
	manifest      v1.Manifest
	manifestBytes []byte
}

// ReadImageConfigLayout returns the descriptor-verified config bytes from the
// sole image manifest in an OCI image layout.
func ReadImageConfigLayout(ctx context.Context, layout string) ([]byte, error) {
	image, err := openVerifiedImageLayout(ctx, layout)
	if err != nil {
		return nil, err
	}
	config, err := readLayerMetadata(ctx, image.store, image.manifest.Config)
	if err != nil {
		return nil, fmt.Errorf("read image config: %w", err)
	}
	return config, nil
}

// ReplaceImageConfigLayout replaces the config of the sole image manifest in
// an OCI image layout. Existing manifest and index extensions are retained;
// the index is changed only after the new immutable blobs are present.
func ReplaceImageConfigLayout(ctx context.Context, layout string, rawConfig []byte) (configDigest, manifestDigest digest.Digest, err error) {
	if layout == "" {
		return "", "", errors.New("OCI layout is required")
	}
	if len(rawConfig) == 0 || len(rawConfig) > maxMetadataBytes {
		return "", "", fmt.Errorf("OCI image config must contain between 1 and %d bytes", maxMetadataBytes)
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}

	imageLayout, err := openVerifiedImageLayout(ctx, layout)
	if err != nil {
		return "", "", err
	}
	manifest := imageLayout.manifest

	emittedConfig, err := readLayerMetadata(ctx, imageLayout.store, manifest.Config)
	if err != nil {
		return "", "", fmt.Errorf("read emitted image config: %w", err)
	}
	var emittedImage v1.Image
	if err := json.Unmarshal(emittedConfig, &emittedImage); err != nil || emittedImage.RootFS.Type != "layers" || len(emittedImage.RootFS.DiffIDs) != len(manifest.Layers) {
		return "", "", fmt.Errorf("emitted OCI image config rootfs does not describe %d manifest layers: %v", len(manifest.Layers), err)
	}
	var image v1.Image
	if err := json.Unmarshal(rawConfig, &image); err != nil {
		return "", "", fmt.Errorf("invalid OCI image config: %w", err)
	}
	if image.RootFS.Type != "layers" || len(image.RootFS.DiffIDs) != len(manifest.Layers) {
		return "", "", fmt.Errorf("OCI image config rootfs has %d diff IDs for %d manifest layers", len(image.RootFS.DiffIDs), len(manifest.Layers))
	}
	for i, diffID := range image.RootFS.DiffIDs {
		if err := diffID.Validate(); err != nil {
			return "", "", fmt.Errorf("OCI image config rootfs diff ID %d is invalid: %w", i, err)
		}
		if diffID != emittedImage.RootFS.DiffIDs[i] {
			return "", "", fmt.Errorf("OCI image config rootfs diff ID %d differs from emitted image", i)
		}
	}

	manifestRaw, err := decodeLayerObject(imageLayout.manifestBytes)
	if err != nil {
		return "", "", fmt.Errorf("decode image manifest: %w", err)
	}
	configRaw, err := decodeLayerObject(manifestRaw["config"])
	if err != nil {
		return "", "", fmt.Errorf("decode image config descriptor: %w", err)
	}
	newConfig := Descriptor(manifest.Config.MediaType, rawConfig)
	if configRaw["mediaType"], err = json.Marshal(newConfig.MediaType); err != nil {
		return "", "", err
	}
	if configRaw["digest"], err = json.Marshal(newConfig.Digest); err != nil {
		return "", "", err
	}
	if configRaw["size"], err = json.Marshal(newConfig.Size); err != nil {
		return "", "", err
	}
	delete(configRaw, "data")
	delete(configRaw, "urls")
	if manifestRaw["config"], err = json.Marshal(configRaw); err != nil {
		return "", "", err
	}
	newManifestBytes, err := json.Marshal(manifestRaw)
	if err != nil {
		return "", "", err
	}
	newRoot := Descriptor(imageLayout.root.MediaType, newManifestBytes)

	if err := writeNormalizedLayout(ctx, imageLayout.store, layout, imageLayout.index, newRoot, newConfig, rawConfig, newManifestBytes); err != nil {
		return "", "", fmt.Errorf("commit rewritten OCI layout: %w", err)
	}
	return newConfig.Digest, newRoot.Digest, nil
}

func openVerifiedImageLayout(ctx context.Context, layout string) (verifiedImageLayout, error) {
	if layout == "" {
		return verifiedImageLayout{}, errors.New("OCI layout is required")
	}
	if err := ctx.Err(); err != nil {
		return verifiedImageLayout{}, err
	}
	index, root, err := layoutRoot(layout)
	if err != nil {
		return verifiedImageLayout{}, err
	}
	var schemaVersion int
	if err := json.Unmarshal(index["schemaVersion"], &schemaVersion); err != nil || schemaVersion != 2 {
		return verifiedImageLayout{}, fmt.Errorf("invalid OCI index schema version: %v", err)
	}
	if root.MediaType != v1.MediaTypeImageManifest && root.MediaType != dockerManifestType {
		return verifiedImageLayout{}, fmt.Errorf("OCI layout root is not an image manifest: %q", root.MediaType)
	}
	store, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		return verifiedImageLayout{}, err
	}
	store.AutoSaveIndex = false
	if err := verifyArchiveGraph(ctx, store, root); err != nil {
		return verifiedImageLayout{}, fmt.Errorf("verify OCI layout: %w", err)
	}
	manifestBytes, err := readLayerMetadata(ctx, store, root)
	if err != nil {
		return verifiedImageLayout{}, fmt.Errorf("read image manifest: %w", err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil || manifest.SchemaVersion != 2 {
		return verifiedImageLayout{}, fmt.Errorf("invalid OCI image manifest: %v", err)
	}
	if manifest.MediaType != "" && manifest.MediaType != root.MediaType {
		return verifiedImageLayout{}, fmt.Errorf("image manifest media type %q differs from descriptor %q", manifest.MediaType, root.MediaType)
	}
	if manifest.Config.MediaType != v1.MediaTypeImageConfig && manifest.Config.MediaType != dockerConfigType {
		return verifiedImageLayout{}, fmt.Errorf("unsupported image config media type %q", manifest.Config.MediaType)
	}
	return verifiedImageLayout{index: index, root: root, store: store, manifest: manifest, manifestBytes: manifestBytes}, nil
}
