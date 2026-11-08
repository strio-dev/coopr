package buildah

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/strio-dev/coopr/internal/oci"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/storage"
)

func historyTimestamp(policy timestampPolicy) *time.Time {
	epoch := policy.createdEpoch()
	if epoch == nil {
		return nil
	}
	created := time.Unix(*epoch, 0).UTC()
	return &created
}

// checkpointSelected commits and rebases through the exact selected manifest.
// Builder settings and an explicit network interface belong to the caller.
func checkpointSelected(ctx context.Context, store storage.Store, builder *upstream.Builder, options upstream.BuilderOptions, linked bool, policy timestampPolicy, keepIntermediate ...bool) (*upstream.Builder, string, digest.Digest, error) {
	if ctx == nil {
		return nil, "", "", errors.New("checkpoint context is nil")
	}
	if store == nil {
		return nil, "", "", errors.New("checkpoint store is nil")
	}
	if builder == nil {
		return nil, "", "", errors.New("checkpoint builder is nil")
	}
	if options.NetworkInterface == nil {
		return nil, "", "", errors.New("checkpoint network interface is required")
	}

	commitOptions := upstream.CommitOptions{
		PreferredManifestType: options.Format,
		EmptyLayerIfEmptyDiff: keepEmptyFilesystemLayer(options.Format),
		OmitLayerHistoryEntry: linked,
		SystemContext:         options.SystemContext,
	}
	policy.apply(&commitOptions)
	imageID, _, manifestDigest, err := builder.Commit(ctx, nil, commitOptions)
	if err != nil {
		return nil, "", "", fmt.Errorf("commit Buildah checkpoint: %w", err)
	}
	if len(keepIntermediate) == 0 || !keepIntermediate[0] {
		if err := builder.Delete(); err != nil {
			return nil, imageID, manifestDigest, fmt.Errorf("delete checkpointed Buildah builder: %w", err)
		}
	}

	selectedBase, err := oci.SelectedStoredImage(ctx, store, imageID, manifestDigest)
	if err != nil {
		return nil, imageID, manifestDigest, fmt.Errorf("select Buildah checkpoint manifest: %w", err)
	}
	if len(keepIntermediate) != 0 && keepIntermediate[0] && options.Container != "" {
		id, err := newWorkerJobID()
		if err != nil {
			return nil, imageID, manifestDigest, err
		}
		options.Container += "-" + id[:12]
	}
	options.FromImage = selectedBase
	options.PullPolicy = define.PullNever
	// This is an internal rebase, not a new user-selected base image.
	options.PreserveBaseImageAnns = true
	replacement, err := upstream.NewBuilder(ctx, store, options)
	if err != nil {
		return nil, imageID, manifestDigest, fmt.Errorf("create Buildah builder from checkpoint %q: %w", imageID, err)
	}
	// The alias selects the exact manifest for initialization. The committed
	// image ID remains the stage's public identity. Cache inputs use its rootfs.
	replacement.FromImageID = imageID
	replacement.FromImage = imageID
	return replacement, imageID, manifestDigest, nil
}

// checkpointPreservingPackageRootSelected restores root metadata after rebasing.
// metadata may reuse a capture from the current filesystem operation; nil captures it here.
func checkpointPreservingPackageRootSelected(ctx context.Context, store storage.Store, builder *upstream.Builder, options upstream.BuilderOptions, linked bool, policy timestampPolicy, metadata *PackageRootMetadata, keepIntermediate ...bool) (*upstream.Builder, string, digest.Digest, *PackageRootMetadata, error) {
	if metadata == nil {
		var err error
		metadata, err = capturePackageRootMetadata(store, builder)
		if err != nil {
			return nil, "", "", nil, fmt.Errorf("capture checkpoint root metadata: %w", err)
		}
	}
	if _, ok := cacheablePackageRootMetadata(metadata); !ok {
		return nil, "", "", nil, errors.New("checkpoint root metadata is not portable")
	}
	replacement, imageID, manifestDigest, err := checkpointSelected(ctx, store, builder, options, linked, policy, keepIntermediate...)
	if err != nil {
		return nil, imageID, manifestDigest, nil, err
	}
	if err := restorePackageRootMetadata(store, replacement, metadata); err != nil {
		return nil, imageID, manifestDigest, nil, errors.Join(err, replacement.Delete())
	}
	return replacement, imageID, manifestDigest, metadata, nil
}
