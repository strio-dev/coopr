package buildah

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.podman.io/storage/pkg/archive"
	"go.podman.io/storage/pkg/idtools"
	"go.podman.io/storage/pkg/system"
)

// packageRootMetadataFromMount reads only the root directory metadata that a
// full containers/storage tar walk would place in its first header. Keeping
// the archive package's header and ID-mapping primitives preserves the exact
// ownership and mode semantics without traversing the rootfs.
func packageRootMetadataFromMount(root string, uidMap, gidMap []idtools.IDMap, mountLabel, ambientSELinux string) (*PackageRootMetadata, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("package builder root %q is not a directory", root)
	}
	header, err := archive.FileInfoHeader(".", info, "")
	if err != nil {
		return nil, err
	}
	if len(uidMap) != 0 || len(gidMap) != 0 {
		header.Uid, err = idtools.RawToContainer(header.Uid, uidMap)
		if err != nil {
			return nil, err
		}
		header.Gid, err = idtools.RawToContainer(header.Gid, gidMap)
		if err != nil {
			return nil, err
		}
	}
	header.PAXRecords, err = rootArchiveXattrs(root, header.PAXRecords, uidMap, gidMap)
	if err != nil {
		return nil, err
	}
	if err := archive.ReadFileFlagsToTarHeader(root, header); err != nil {
		return nil, err
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

// addRootArchiveXattrs mirrors containers/storage's tar writer: it exports
// security.capability, security.ima, and user.* except user.overlay.*.
func rootArchiveXattrs(path string, records map[string]string, uidMap, gidMap []idtools.IDMap) (map[string]string, error) {
	add := func(name string, value []byte) {
		if records == nil {
			records = make(map[string]string)
		}
		records[archive.PaxSchilyXattr+name] = string(value)
	}
	for _, name := range []string{"security.capability", "security.ima"} {
		value, err := system.Lgetxattr(path, name)
		if err != nil && !errors.Is(err, system.ENOTSUP) && err != system.ErrNotSupportedPlatform {
			return nil, fmt.Errorf("failed to read %q attribute from %q: %w", name, path, err)
		}
		if value != nil {
			if name == "security.capability" {
				value, err = normalizeRootCapabilityID(value, uidMap, gidMap)
				if err != nil {
					return nil, fmt.Errorf("normalize %q attribute from %q: %w", name, path, err)
				}
			}
			add(name, value)
		}
	}
	names, err := system.Llistxattr(path)
	if err != nil && !errors.Is(err, system.ENOTSUP) && err != system.ErrNotSupportedPlatform {
		return nil, err
	}
	for _, name := range names {
		if !strings.HasPrefix(name, "user.") || strings.HasPrefix(name, "user.overlay.") {
			continue
		}
		value, err := system.Lgetxattr(path, name)
		if err != nil {
			if err == system.E2BIG {
				continue
			}
			return nil, err
		}
		add(name, value)
	}
	return records, nil
}

// containers/storage's tar writer maps v3 capability root IDs into the
// container namespace and downgrades root-owned values to v2. Match that
// header transformation for root metadata without starting an archive walk.
func normalizeRootCapabilityID(value []byte, uidMap, gidMap []idtools.IDMap) ([]byte, error) {
	const (
		revisionMask = 0xff000000
		revisionV2   = 0x02000000
		revisionV3   = 0x03000000
	)
	if len(uidMap) == 0 && len(gidMap) == 0 || len(value) != 24 {
		return value, nil
	}
	magic := binary.LittleEndian.Uint32(value[:4])
	if magic&revisionMask != revisionV3 {
		return value, nil
	}
	hostID := binary.LittleEndian.Uint32(value[20:24])
	containerID, err := idtools.RawToContainer(int(hostID), uidMap)
	if err != nil {
		return nil, err
	}
	if containerID == 0 {
		result := make([]byte, 20)
		copy(result, value[:20])
		binary.LittleEndian.PutUint32(result[:4], magic&^revisionMask|revisionV2)
		return result, nil
	}
	result := append([]byte(nil), value...)
	binary.LittleEndian.PutUint32(result[20:24], uint32(containerID))
	return result, nil
}
