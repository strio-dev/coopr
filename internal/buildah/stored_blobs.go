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

	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

const retainedLayerMapBigData = oci.StoredLayerBlobsKey

// retainStoredLayoutLayers keeps exact compressed layer representations in
// containers/storage. The graph keeps the unpacked layer for execution; layer
// big data keeps the OCI bytes needed to reproduce the committed manifest.
func retainStoredLayoutLayers(store storage.Store, imageID, layout string, manifestDigest digest.Digest) error {
	manifestPath := filepath.Join(layout, "blobs", manifestDigest.Algorithm().String(), manifestDigest.Encoded())
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read committed image manifest: %w", err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("decode committed image manifest: %w", err)
	}
	image, err := store.Image(imageID)
	if err != nil {
		// Buildah may write a destination-only image without retaining an
		// untagged containers/storage record when the committed filesystem is
		// empty or duplicates an existing image. The OCI layout is still the
		// authoritative output and is imported after config reconciliation.
		if errors.Is(err, storage.ErrImageUnknown) {
			return nil
		}
		return fmt.Errorf("inspect committed storage image: %w", err)
	}
	layers := make([]storage.Layer, 0, len(manifest.Layers))
	for layerID := image.TopLayer; layerID != ""; {
		layer, err := store.Layer(layerID)
		if err != nil {
			return fmt.Errorf("inspect committed storage layer %s: %w", layerID, err)
		}
		layers = append(layers, *layer)
		layerID = layer.Parent
	}
	slices.Reverse(layers)
	if len(layers) != len(manifest.Layers) {
		return fmt.Errorf("committed image has %d storage layers and %d manifest layers", len(layers), len(manifest.Layers))
	}
	retained := make(map[string]string)
	for index, descriptor := range manifest.Layers {
		if descriptor.Digest == layers[index].UncompressedDigest {
			continue
		}
		blobPath := filepath.Join(layout, "blobs", descriptor.Digest.Algorithm().String(), descriptor.Digest.Encoded())
		blob, err := os.Open(blobPath)
		if err != nil {
			return fmt.Errorf("open committed layer %s: %w", descriptor.Digest, err)
		}
		verifier := descriptor.Digest.Verifier()
		setErr := store.SetLayerBigData(layers[index].ID, descriptor.Digest.String(), io.TeeReader(blob, verifier))
		closeErr := blob.Close()
		if setErr != nil || closeErr != nil {
			return fmt.Errorf("retain committed layer %s: %w", descriptor.Digest, errors.Join(setErr, closeErr))
		}
		if !verifier.Verified() {
			return fmt.Errorf("committed layer content does not match %s", descriptor.Digest)
		}
		retained[descriptor.Digest.String()] = layers[index].ID
	}
	if len(retained) > 0 {
		data, err := json.Marshal(retained)
		if err != nil {
			return err
		}
		if err := store.SetImageBigData(imageID, retainedLayerMapBigData, data, nil); err != nil {
			return fmt.Errorf("record exact stored layer blobs: %w", err)
		}
	}
	return nil
}

func restoreSquashBaseLayers(ctx context.Context, sourceLayout, targetLayout string) (v1.Descriptor, error) {
	sourceRoot, err := oci.LayoutRoot(sourceLayout)
	if err != nil {
		return v1.Descriptor{}, err
	}
	targetRoot, err := oci.LayoutRoot(targetLayout)
	if err != nil {
		return v1.Descriptor{}, err
	}
	sourceData, err := readVerifiedLayoutBlob(sourceLayout, sourceRoot)
	if err != nil {
		return v1.Descriptor{}, err
	}
	targetData, err := readVerifiedLayoutBlob(targetLayout, targetRoot)
	if err != nil {
		return v1.Descriptor{}, err
	}
	var sourceManifest, targetManifest v1.Manifest
	if err := json.Unmarshal(sourceData, &sourceManifest); err != nil {
		return v1.Descriptor{}, err
	}
	if err := json.Unmarshal(targetData, &targetManifest); err != nil {
		return v1.Descriptor{}, err
	}
	if len(targetManifest.Layers) == 0 {
		return v1.Descriptor{}, errors.New("squashed image has no filesystem layer")
	}
	baseLayers := len(targetManifest.Layers) - 1
	if len(sourceManifest.Layers) < baseLayers {
		return v1.Descriptor{}, fmt.Errorf("unsquashed image has %d layers, fewer than squash base %d", len(sourceManifest.Layers), baseLayers)
	}
	sourceConfigData, err := oci.ReadImageConfigLayout(ctx, sourceLayout)
	if err != nil {
		return v1.Descriptor{}, err
	}
	targetConfigData, err := oci.ReadImageConfigLayout(ctx, targetLayout)
	if err != nil {
		return v1.Descriptor{}, err
	}
	var sourceConfig, targetConfig v1.Image
	if err := json.Unmarshal(sourceConfigData, &sourceConfig); err != nil {
		return v1.Descriptor{}, err
	}
	if err := json.Unmarshal(targetConfigData, &targetConfig); err != nil {
		return v1.Descriptor{}, err
	}
	if len(sourceConfig.RootFS.DiffIDs) < baseLayers || len(targetConfig.RootFS.DiffIDs) < baseLayers || !slices.Equal(sourceConfig.RootFS.DiffIDs[:baseLayers], targetConfig.RootFS.DiffIDs[:baseLayers]) {
		return v1.Descriptor{}, errors.New("squashed image filesystem does not share the unsquashed base")
	}
	source, err := orasoci.NewWithContext(ctx, sourceLayout)
	if err != nil {
		return v1.Descriptor{}, err
	}
	target, err := orasoci.NewWithContext(ctx, targetLayout)
	if err != nil {
		return v1.Descriptor{}, err
	}
	target.AutoSaveIndex = false
	for index := 0; index < baseLayers; index++ {
		descriptor := sourceManifest.Layers[index]
		exists, err := target.Exists(ctx, descriptor)
		if err != nil {
			return v1.Descriptor{}, err
		}
		if !exists {
			stream, err := source.Fetch(ctx, descriptor)
			if err != nil {
				return v1.Descriptor{}, err
			}
			pushErr := target.Push(ctx, descriptor, stream)
			closeErr := stream.Close()
			if pushErr != nil || closeErr != nil {
				return v1.Descriptor{}, errors.Join(pushErr, closeErr)
			}
		}
		targetManifest.Layers[index] = descriptor
	}
	targetData, err = json.Marshal(targetManifest)
	if err != nil {
		return v1.Descriptor{}, err
	}
	updated := v1.Descriptor{MediaType: targetRoot.MediaType, Digest: digest.FromBytes(targetData), Size: int64(len(targetData)), Annotations: targetRoot.Annotations, Platform: targetRoot.Platform}
	if err := target.Push(ctx, updated, bytes.NewReader(targetData)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return v1.Descriptor{}, err
	}
	indexPath := filepath.Join(targetLayout, v1.ImageIndexFile)
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		return v1.Descriptor{}, err
	}
	var index v1.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		return v1.Descriptor{}, err
	}
	if len(index.Manifests) != 1 {
		return v1.Descriptor{}, errors.New("image layout index does not contain exactly one manifest")
	}
	index.Manifests[0] = updated
	indexData, err = json.Marshal(index)
	if err != nil {
		return v1.Descriptor{}, err
	}
	temporary, err := os.CreateTemp(targetLayout, ".coopr-squash-index-*.json")
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
	return updated, nil
}
