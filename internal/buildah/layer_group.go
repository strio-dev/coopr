package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"coopr/internal/imageconfig"
	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
)

// A group retains its starting image while child instructions use ordinary
// checkpoints. Closing it replaces those child layers with their net delta.
type layerGroupState struct {
	imageID  string
	manifest digest.Digest
	root     *PackageRootMetadata
}

func (executor *graphExecutor) startLayerGroup(ctx context.Context, current *upstream.Builder, options upstream.BuilderOptions, logical *imageconfig.Config) (*upstream.Builder, upstream.BuilderOptions, digest.Digest, *layerGroupState, error) {
	changed, err := componentBuilderHasFilesystemChanges(executor.store, current)
	if err != nil {
		return nil, options, "", nil, err
	}
	next, id, manifest, root, err := checkpointPreservingPackageRootSelected(ctx, executor.store, current, options, !changed, timestampPolicyFromOptions(executor.options), executor.options.Lifecycle.KeepIntermediate)
	if err != nil {
		return nil, options, "", nil, fmt.Errorf("start layer group: %w", err)
	}
	options.FromImage = id
	if err := adoptCommittedConfig(ctx, executor.store, id, options.SystemContext, logical); err != nil {
		return next, options, manifest, nil, err
	}
	return next, options, manifest, &layerGroupState{imageID: id, manifest: manifest, root: root}, nil
}

func (executor *graphExecutor) finishLayerGroup(ctx context.Context, current *upstream.Builder, options upstream.BuilderOptions, logical *imageconfig.Config, group *layerGroupState) (_ *upstream.Builder, _ upstream.BuilderOptions, _ digest.Digest, _ *PackageRootMetadata, retErr error) {
	if group == nil {
		return current, options, "", nil, errors.New("missing layer group start")
	}
	root, err := capturePackageRootMetadata(executor.store, current)
	if err != nil {
		return current, options, "", nil, err
	}
	changed, err := componentBuilderHasFilesystemChanges(executor.store, current)
	if err != nil {
		return current, options, "", nil, err
	}
	selectedID, _, err := commitStoredSnapshotSelected(ctx, current, options.SystemContext, options.Format, !changed, timestampPolicyFromOptions(executor.options))
	if err != nil {
		return current, options, "", nil, fmt.Errorf("checkpoint layer group result: %w", err)
	}
	selected, err := oci.SelectedStoredImage(ctx, executor.store, group.imageID, group.manifest)
	if err != nil {
		return current, options, "", nil, err
	}
	options.FromImage, options.PullPolicy, options.PreserveBaseImageAnns = selected, define.PullNever, true
	options.Container = executor.builderContainerName("layer")
	combined, err := upstream.NewBuilder(ctx, executor.store, options)
	if err != nil {
		return current, options, "", nil, err
	}
	combined.FromImage, combined.FromImageID = group.imageID, group.imageID
	keep := false
	defer func() {
		if !keep {
			retErr = errors.Join(retErr, combined.Delete())
		}
	}()
	if err := applyComponentDeltaMutable(ctx, executor.store, group.imageID, selectedID, combined, os.TempDir()); err != nil {
		return current, options, "", nil, fmt.Errorf("compact layer group: %w", err)
	}
	if err := restorePackageRootMetadata(executor.store, combined, root); err != nil {
		return current, options, "", nil, err
	}
	if err := syncBuilderConfig(combined, logical); err != nil {
		return current, options, "", nil, err
	}
	// Grouping changes layer boundaries, not the record of authored steps.
	startRaw, err := packageImageConfig(ctx, executor.store, group.imageID, options.SystemContext)
	if err != nil {
		return current, options, "", nil, err
	}
	resultRaw, err := packageImageConfig(ctx, executor.store, selectedID, options.SystemContext)
	if err != nil {
		return current, options, "", nil, err
	}
	var startImage, resultImage v1.Image
	if err := json.Unmarshal(startRaw, &startImage); err != nil {
		return current, options, "", nil, err
	}
	if err := json.Unmarshal(resultRaw, &resultImage); err != nil {
		return current, options, "", nil, err
	}
	if len(resultImage.History) < len(startImage.History) {
		return current, options, "", nil, errors.New("layer group result lost starting history")
	}
	for _, entry := range resultImage.History[len(startImage.History):] {
		combined.AddPrependedEmptyLayer(entry.Created, entry.CreatedBy, entry.Author, entry.Comment)
	}
	combined.SetCreatedBy("LAYER")
	changed, err = componentBuilderHasFilesystemChanges(executor.store, combined)
	if err != nil {
		return current, options, "", nil, err
	}
	next, id, manifest, root, err := checkpointPreservingPackageRootSelected(ctx, executor.store, combined, options, !changed, timestampPolicyFromOptions(executor.options), executor.options.Lifecycle.KeepIntermediate)
	if err != nil {
		return current, options, "", nil, fmt.Errorf("commit layer group: %w", err)
	}
	keep = true
	if err := current.Delete(); err != nil {
		return next, options, manifest, root, err
	}
	options.FromImage = id
	if err := adoptCommittedConfig(ctx, executor.store, id, options.SystemContext, logical); err != nil {
		return next, options, manifest, root, err
	}
	return next, options, manifest, root, nil
}
