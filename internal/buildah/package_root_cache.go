package buildah

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"maps"
	"strings"

	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/copier"
	"go.podman.io/storage"
)

// cacheablePackageRootMetadata returns the logical root-directory metadata
// that can survive a package instruction-cache checkpoint.  The per-mount
// SELinux label is execution policy, not image content, so it is deliberately
// excluded from both cache keys and restored sidecars.
func cacheablePackageRootMetadata(metadata *PackageRootMetadata) (*PackageRootMetadata, bool) {
	if metadata == nil || metadata.UnportableXattrs || metadata.Mode < 0 || metadata.Mode&^0o7777 != 0 || metadata.UID < 0 || metadata.GID < 0 {
		return nil, false
	}
	result := &PackageRootMetadata{
		Mode: metadata.Mode, UID: metadata.UID, GID: metadata.GID,
		PAXRecords: maps.Clone(metadata.PAXRecords),
	}
	for key := range result.PAXRecords {
		if strings.TrimPrefix(key, "SCHILY.xattr.") == "security.selinux" || strings.TrimPrefix(key, "LIBARCHIVE.xattr.") == "security.selinux" {
			delete(result.PAXRecords, key)
		}
	}
	return result, true
}

// restorePackageRootMetadata reapplies the root-directory metadata omitted by
// containers/storage commits. copier.Put performs the same user-namespace ID
// mapping and safe chrooted extraction used by Buildah COPY/ADD.
func restorePackageRootMetadata(store storage.Store, builder *upstream.Builder, metadata *PackageRootMetadata) (retErr error) {
	if store == nil {
		return errors.New("restore package root metadata store is nil")
	}
	if builder == nil {
		return errors.New("restore package root metadata builder is nil")
	}
	portable, ok := cacheablePackageRootMetadata(metadata)
	if !ok {
		return errors.New("package root metadata is not cacheable")
	}
	container, err := store.Container(builder.ContainerID)
	if err != nil {
		return fmt.Errorf("inspect package cache builder container: %w", err)
	}
	layer, err := store.Layer(container.LayerID)
	if err != nil {
		return fmt.Errorf("inspect package cache builder layer: %w", err)
	}
	mountPoint, err := builder.Mount(builder.MountLabel)
	if err != nil {
		return fmt.Errorf("mount package cache builder rootfs: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, builder.Unmount()) }()

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(portable.tarHeader()); err != nil {
		return fmt.Errorf("encode package cache root metadata: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish package cache root metadata: %w", err)
	}
	if err := copier.Put(mountPoint, mountPoint, copier.PutOptions{
		UIDMap: layer.UIDMap, GIDMap: layer.GIDMap,
	}, bytes.NewReader(archive.Bytes())); err != nil {
		return fmt.Errorf("restore package cache root metadata: %w", err)
	}
	return nil
}
