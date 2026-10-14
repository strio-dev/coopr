package buildah

import (
	"archive/tar"
	"errors"
	"fmt"
	"os"

	"go.podman.io/storage/pkg/archive"
	"go.podman.io/storage/pkg/idtools"
)

// packageRootMetadataFromMount uses the native archive header for root ownership,
// mode, xattrs and file flags. Closing after the first header stops the archive
// producer without draining the archive.
func packageRootMetadataFromMount(root string, uidMap, gidMap []idtools.IDMap, mountLabel, ambientSELinux string) (*PackageRootMetadata, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("package builder root %q is not a directory", root)
	}
	stream, err := archive.TarWithOptions(root, &archive.TarOptions{
		Compression: archive.Uncompressed, IncludeSourceDir: true,
		UIDMaps: uidMap, GIDMaps: gidMap,
	})
	if err != nil {
		return nil, err
	}
	header, readErr := tar.NewReader(stream).Next()
	closeErr := stream.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if header.Typeflag != tar.TypeDir || header.Name != "./" {
		return nil, fmt.Errorf("package builder root %q has invalid archive root header %q", root, header.Name)
	}
	portable, portableErr := hasPortableXattrs(root, ambientSELinux)
	if portableErr == nil && !portable && mountLabel != "" && mountLabel != ambientSELinux {
		// Buildah can apply an MCS category to the live builder mount. Accept
		// only that exact category as transient executor policy.
		portable, portableErr = hasPortableXattrs(root, mountLabel)
	}
	return &PackageRootMetadata{
		Mode: header.Mode, UID: header.Uid, GID: header.Gid,
		PAXRecords:       header.PAXRecords,
		UnportableXattrs: portableErr != nil || !portable,
		AmbientSELinux:   ambientSELinux,
	}, nil
}
