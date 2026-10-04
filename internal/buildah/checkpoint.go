package buildah

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opencontainers/go-digest"
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

// Checkpoint commits builder's current filesystem and image configuration to
// the local store, removes the working container, and returns a new builder
// based on the committed image. The committed image becomes the immutable base
// for the next operation, so a later commit retains its layers and adds a new
// layer for subsequent filesystem changes.
//
// Options describe the replacement builder. FromImage, PullPolicy, and Format
// are set by Checkpoint; NetworkInterface must be supplied explicitly so the
// replacement builder never falls back to host containers configuration.
func Checkpoint(ctx context.Context, store storage.Store, builder *upstream.Builder, options upstream.BuilderOptions) (*upstream.Builder, string, error) {
	return checkpoint(ctx, store, builder, options, false, timestampPolicy{})
}

// A linked COPY/ADD already carries its own layer and history entry. Its
// mutable working layer is empty; omit it when rebasing for the next command.
func checkpoint(ctx context.Context, store storage.Store, builder *upstream.Builder, options upstream.BuilderOptions, linked bool, policy timestampPolicy) (*upstream.Builder, string, error) {
	replacement, imageID, _, err := checkpointSelected(ctx, store, builder, options, linked, policy)
	return replacement, imageID, err
}

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

	selectedBase, err := selectedBuilderBase(ctx, store, imageID, manifestDigest)
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

// checkpointPreservingPackageRoot records and reapplies root-directory
// metadata around a containers/storage commit. The returned metadata is also
// suitable for the instruction-cache sidecar associated with imageID.
func checkpointPreservingPackageRoot(ctx context.Context, store storage.Store, builder *upstream.Builder, options upstream.BuilderOptions, linked bool, policy timestampPolicy) (*upstream.Builder, string, *PackageRootMetadata, error) {
	replacement, imageID, _, metadata, err := checkpointPreservingPackageRootSelected(ctx, store, builder, options, linked, policy)
	return replacement, imageID, metadata, err
}

func checkpointPreservingPackageRootSelected(ctx context.Context, store storage.Store, builder *upstream.Builder, options upstream.BuilderOptions, linked bool, policy timestampPolicy, keepIntermediate ...bool) (*upstream.Builder, string, digest.Digest, *PackageRootMetadata, error) {
	metadata, err := capturePackageRootMetadata(store, builder)
	if err != nil {
		return nil, "", "", nil, fmt.Errorf("capture checkpoint root metadata: %w", err)
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
