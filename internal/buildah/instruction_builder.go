package buildah

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/opencontainers/go-digest"
	"github.com/strio-dev/coopr/internal/oci"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/storage"
)

// importInstructionBuilder opens image metadata without a working container.
// The selected alias keeps ImportBuilderFromImage on the exact cached manifest.
func importInstructionBuilder(ctx context.Context, store storage.Store, options upstream.BuilderOptions, imageID string, manifest digest.Digest) (*upstream.Builder, error) {
	selected, err := oci.SelectedStoredImage(ctx, store, imageID, manifest)
	if err != nil {
		return nil, err
	}
	view, err := upstream.ImportBuilderFromImage(ctx, store, upstream.ImportFromImageOptions{
		Image: selected, SystemContext: options.SystemContext,
	})
	if err != nil {
		return nil, fmt.Errorf("import cached image metadata: %w", err)
	}
	if view.FromImageDigest != manifest.String() || digest.FromBytes(view.Manifest) != manifest {
		return nil, fmt.Errorf("cached image metadata differs from selected manifest %s", manifest)
	}
	view.FromImage, view.FromImageID = imageID, imageID
	view.Format = options.Format
	view.Args = maps.Clone(options.Args)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return view, nil
}

// realizeInstructionBuilder replaces a metadata view with a working container.
// Image metadata moves with the view; runtime and container state comes from
// NewBuilder. Callers replace the view with the returned builder on success.
func realizeInstructionBuilder(ctx context.Context, store storage.Store, view *upstream.Builder, options upstream.BuilderOptions, root *PackageRootMetadata) (working *upstream.Builder, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if view == nil {
		return nil, errors.New("cached image metadata builder is nil")
	}
	if view.ContainerID != "" {
		return view, nil
	}
	manifest, err := digest.Parse(view.FromImageDigest)
	if err != nil || digest.FromBytes(view.Manifest) != manifest {
		return nil, errors.New("cached image metadata has no exact selected manifest")
	}
	selected, err := oci.SelectedStoredImage(ctx, store, view.FromImageID, manifest)
	if err != nil {
		return nil, err
	}
	options.FromImage, options.PullPolicy = selected, define.PullNever
	options.PreserveBaseImageAnns = true
	working, err = upstream.NewBuilder(ctx, store, options)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, working.Delete())
			working = nil
		}
	}()
	if err := ctx.Err(); err != nil {
		return working, err
	}
	hostname := working.Hostname()
	working.OCIv1, working.Docker = view.OCIv1, view.Docker
	if view.Docker.Config != nil {
		working.Docker.ContainerConfig = *view.Docker.Config
	}
	working.Docker.Config = &working.Docker.ContainerConfig
	if working.Hostname() == "" && hostname != "" {
		working.SetHostname(hostname)
	}
	working.Config, working.Manifest = view.Config, view.Manifest
	working.FromImage, working.FromImageID, working.FromImageDigest = view.FromImage, view.FromImageID, view.FromImageDigest
	working.ImageAnnotations = maps.Clone(view.ImageAnnotations)
	working.ImageCreatedBy, working.ImageHistoryComment = view.ImageCreatedBy, view.ImageHistoryComment
	working.Args = maps.Clone(view.Args)
	working.PrependedEmptyLayers = slices.Clone(view.PrependedEmptyLayers)
	working.AppendedEmptyLayers = slices.Clone(view.AppendedEmptyLayers)
	working.PrependedLinkedLayers = slices.Clone(view.PrependedLinkedLayers)
	working.AppendedLinkedLayers = slices.Clone(view.AppendedLinkedLayers)
	if err := restorePackageRootMetadata(store, working, root); err != nil {
		return working, err
	}
	if err := ctx.Err(); err != nil {
		return working, err
	}
	if err := working.Save(); err != nil {
		return working, err
	}
	return working, ctx.Err()
}
