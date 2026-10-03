package buildah

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	upstream "go.podman.io/buildah"
	"go.podman.io/storage"
	storagearchive "go.podman.io/storage/pkg/archive"
)

// ApplyComponentDelta places the net filesystem change between a caller image
// and an executed component image on a fresh builder based on that caller.
// The caller commits the builder once to give the component one OCI layer.
// Image-configuration changes are applied separately from the logical config.
func ApplyComponentDelta(ctx context.Context, store storage.Store, callerImageID, componentImageID string, builder *upstream.Builder, scratchDir string) (retErr error) {
	return applyComponentDelta(ctx, store, callerImageID, componentImageID, builder, scratchDir, false)
}

// applyComponentDeltaMutable applies the delta through the mounted writable
// rootfs instead of containers/storage ApplyDiff. ApplyDiff records tar-split
// metadata for the layer; later instructions in a --layers=false build would
// then be omitted when that stale tar-split is committed. The mounted path
// keeps the layer mutable for instructions and later component boundaries.
func applyComponentDeltaMutable(ctx context.Context, store storage.Store, callerImageID, componentImageID string, builder *upstream.Builder, scratchDir string) error {
	return applyComponentDelta(ctx, store, callerImageID, componentImageID, builder, scratchDir, true)
}

func applyComponentDelta(ctx context.Context, store storage.Store, callerImageID, componentImageID string, builder *upstream.Builder, scratchDir string, mutable bool) (retErr error) {
	if ctx == nil {
		return errors.New("component delta context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(scratchDir) {
		return fmt.Errorf("component delta scratch directory must be absolute: %q", scratchDir)
	}
	if store == nil || builder == nil || callerImageID == "" || componentImageID == "" {
		return errors.New("component delta requires store, builder, caller image, and component image")
	}
	if builder.FromImageID != callerImageID {
		return fmt.Errorf("component delta builder base %q differs from caller image %q", builder.FromImageID, callerImageID)
	}
	caller, err := store.Image(callerImageID)
	if err != nil {
		return fmt.Errorf("inspect caller image: %w", err)
	}
	component, err := store.Image(componentImageID)
	if err != nil {
		return fmt.Errorf("inspect component result image: %w", err)
	}
	container, err := store.Container(builder.ContainerID)
	if err != nil {
		return fmt.Errorf("inspect component output builder: %w", err)
	}
	if container.LayerID == "" {
		return errors.New("component delta requires a writable destination layer")
	}
	destination, err := store.Layer(container.LayerID)
	if err != nil {
		return fmt.Errorf("inspect component output layer: %w", err)
	}
	if destination.Parent != caller.TopLayer {
		return fmt.Errorf("component output layer is not based on caller layer")
	}
	changes, err := store.Changes(caller.TopLayer, destination.ID)
	if err != nil {
		return fmt.Errorf("inspect component output changes: %w", err)
	}
	if len(changes) != 0 {
		return fmt.Errorf("component output builder is not fresh: %d filesystem changes", len(changes))
	}
	if caller.TopLayer == "" && component.TopLayer == "" {
		return nil
	}
	if component.TopLayer == "" {
		return errors.New("component result has no filesystem layer but caller does")
	}
	fromLayer := caller.TopLayer
	if fromLayer == "" {
		// Diff("", to) means "from to.Parent" in containers/storage, so it
		// would omit earlier component layers. Use a real empty root with the
		// result layer's on-disk ID mapping instead.
		resultLayer, err := store.Layer(component.TopLayer)
		if err != nil {
			return fmt.Errorf("inspect component result layer: %w", err)
		}
		empty, err := store.CreateLayer("", "", nil, "", false, &storage.LayerOptions{
			IDMappingOptions: storage.LayerIDMappingOptions{
				HostUIDMapping: len(resultLayer.UIDMap) == 0, HostGIDMapping: len(resultLayer.GIDMap) == 0,
				UIDMap: slices.Clone(resultLayer.UIDMap), GIDMap: slices.Clone(resultLayer.GIDMap),
			},
		})
		if err != nil {
			return fmt.Errorf("create empty component comparison layer: %w", err)
		}
		defer func() {
			retErr = errors.Join(retErr, store.DeleteLayer(empty.ID))
		}()
		fromLayer = empty.ID
	}

	compression := storagearchive.Uncompressed
	diff, err := store.Diff(fromLayer, component.TopLayer, &storage.DiffOptions{Compression: &compression})
	if err != nil {
		return fmt.Errorf("calculate component filesystem delta: %w", err)
	}
	// containers/storage holds its layer-store lock until Diff is closed.
	// Spool before ApplyDiff, which needs the same lock; stream-copying the open
	// Diff reader directly into ApplyDiff would deadlock.
	file, err := os.CreateTemp(scratchDir, ".coopr-component-delta-*")
	if err != nil {
		return errors.Join(fmt.Errorf("create component delta spool: %w", err), diff.Close())
	}
	defer func() {
		retErr = errors.Join(retErr, file.Close(), os.Remove(file.Name()))
	}()
	_, copyErr := io.Copy(file, contextReader{ctx: ctx, reader: diff})
	closeErr := diff.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("spool component filesystem delta: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind component filesystem delta: %w", err)
	}
	if mutable {
		rootfs, err := builder.Mount(builder.MountLabel)
		if err != nil {
			return fmt.Errorf("mount mutable component output: %w", err)
		}
		defer func() { retErr = errors.Join(retErr, builder.Unmount()) }()
		if _, err := storagearchive.ApplyUncompressedLayer(rootfs, contextReader{ctx: ctx, reader: file}, &storagearchive.TarOptions{
			UIDMaps: destination.UIDMap, GIDMaps: destination.GIDMap,
		}); err != nil {
			return fmt.Errorf("apply mutable component filesystem delta: %w", err)
		}
	} else if _, err := store.ApplyDiff(container.LayerID, contextReader{ctx: ctx, reader: file}); err != nil {
		return fmt.Errorf("apply component filesystem delta: %w", err)
	}
	return ctx.Err()
}
