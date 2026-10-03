package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
)

const selectedBaseAliasDomain = "coopr.buildah.selected-base.v1"

// selectedBuilderBase returns a deterministic containers/storage image record
// whose default manifest is selected. The record shares the original image's
// top layer, so Buildah can initialize from an exact manifest without copying
// the root filesystem or changing the original image's mutable default.
//
// Aliases are retained in Coopr's private graph store. There is at most one
// small image record per source-image/manifest pair, but each record keeps its
// shared TopLayer reachable until the graph store is pruned or removed. We do
// not delete aliases after a build: another concurrent builder may have
// resolved the deterministic alias but not created its container yet.
func selectedBuilderBase(ctx context.Context, store storage.Store, imageID string, selected digest.Digest) (string, error) {
	if ctx == nil {
		return "", errors.New("selected base context is nil")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if store == nil {
		return "", errors.New("selected base store is nil")
	}
	if imageID == "" {
		return "", errors.New("selected base image ID is empty")
	}
	if err := selected.Validate(); err != nil {
		return "", fmt.Errorf("selected base manifest digest %q: %w", selected, err)
	}

	source, err := store.Image(imageID)
	if err != nil {
		return "", fmt.Errorf("inspect selected base image %q: %w", imageID, err)
	}
	manifestKey := storage.ImageDigestManifestBigDataNamePrefix + "-" + selected.String()
	manifestData, err := store.ImageBigData(source.ID, manifestKey)
	if err != nil {
		return "", fmt.Errorf("read selected base manifest %s from %s: %w", selected, source.ID, err)
	}
	if got := digest.FromBytes(manifestData); got != selected {
		return "", fmt.Errorf("selected base manifest content digest is %s, want %s", got, selected)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return "", fmt.Errorf("decode selected base manifest %s: %w", selected, err)
	}
	if err := manifest.Config.Digest.Validate(); err != nil {
		return "", fmt.Errorf("selected base config digest %q: %w", manifest.Config.Digest, err)
	}
	configData, err := store.ImageBigData(source.ID, manifest.Config.Digest.String())
	if err != nil {
		return "", fmt.Errorf("read selected base config %s from %s: %w", manifest.Config.Digest, source.ID, err)
	}
	if got := digest.FromBytes(configData); got != manifest.Config.Digest {
		return "", fmt.Errorf("selected base config content digest is %s, want %s", got, manifest.Config.Digest)
	}

	aliasID := digest.FromString(selectedBaseAliasDomain + "\x00" + source.ID + "\x00" + selected.String()).Encoded()
	options := &storage.ImageOptions{
		Digest:  selected,
		Digests: []digest.Digest{selected},
		BigData: []storage.ImageBigDataOption{
			{Key: storage.ImageDigestBigDataKey, Data: manifestData, Digest: selected},
			{Key: manifestKey, Data: manifestData, Digest: selected},
			{Key: manifest.Config.Digest.String(), Data: configData, Digest: manifest.Config.Digest},
		},
		Flags: map[string]any{"coopr.selected-base": selected.String(), "coopr.source-image": source.ID},
	}
	if _, err := store.CreateImage(aliasID, nil, source.TopLayer, "", options); err != nil && !errors.Is(err, storage.ErrDuplicateID) {
		return "", fmt.Errorf("create selected base alias for %s: %w", selected, err)
	}
	alias, err := store.Image(aliasID)
	if err != nil {
		return "", fmt.Errorf("inspect selected base alias %s: %w", aliasID, err)
	}
	if alias.TopLayer != source.TopLayer {
		return "", fmt.Errorf("selected base alias %s top layer is %s, want %s", aliasID, alias.TopLayer, source.TopLayer)
	}
	stored, err := store.ImageBigData(alias.ID, storage.ImageDigestBigDataKey)
	if err != nil {
		return "", fmt.Errorf("read selected base alias %s default manifest: %w", alias.ID, err)
	}
	if got := digest.FromBytes(stored); got != selected {
		return "", fmt.Errorf("selected base alias %s default manifest is %s, want %s", alias.ID, got, selected)
	}
	return alias.ID, nil
}
