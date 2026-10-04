package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"coopr/internal/oci"
	"coopr/internal/stateidentity"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/system"
)

// snapshotPortableState observes an effective filesystem and its complete
// logical configuration. A result with subsecond file mtimes is ineligible:
// the storage tar exporter can round them, which would alias distinct inputs.
// The caller owns the returned snapshot path and removes it after use.
func snapshotPortableState(ctx context.Context, store storage.Store, system *types.SystemContext, imageID string, snapshotConfig, identityConfig json.RawMessage, root *PackageRootMetadata, platform v1.Platform, dir string) (stateidentity.Identity, oci.Package, string, bool, error) {
	if root == nil || root.UnportableXattrs {
		return stateidentity.Identity{}, oci.Package{}, "", false, nil
	}
	portable, err := imageHasPortableMetadata(ctx, store, imageID, root.AmbientSELinux)
	if err != nil || !portable {
		return stateidentity.Identity{}, oci.Package{}, "", false, err
	}
	file, err := os.CreateTemp(dir, "coopr-state-*.tar")
	if err != nil {
		return stateidentity.Identity{}, oci.Package{}, "", false, err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return stateidentity.Identity{}, oci.Package{}, "", false, err
	}
	defer func() {
		if path != "" {
			_ = os.Remove(path)
		}
	}()
	pkg, err := SnapshotPackage(ctx, store, PackageSnapshotRequest{
		ImageID: imageID, Stage: "cache", TarPath: path, Platform: platform,
		SystemContext: system, Config: snapshotConfig, RootMetadata: root,
	})
	if err != nil {
		return stateidentity.Identity{}, oci.Package{}, "", false, err
	}
	snapshot, err := os.Open(path)
	if err != nil {
		return stateidentity.Identity{}, oci.Package{}, "", false, err
	}
	identity, observeErr := stateidentity.Calculate(ctx, root.tarHeader(), snapshot, identityConfig, nil)
	closeErr := snapshot.Close()
	if err := errors.Join(observeErr, closeErr); err != nil {
		return stateidentity.Identity{}, oci.Package{}, "", false, err
	}
	retained := path
	path = ""
	return identity, pkg, retained, true, nil
}

// SELinux adds per-builder MCS categories to MountLabel, while VFS roots have
// the same base context without those categories. With no allocated mount
// label, use the independently observed storage context, including its range.
func storageAmbientSELinux(mountLabel string, storageLabel []byte) string {
	if mountLabel == "" {
		return strings.TrimSuffix(string(storageLabel), "\x00")
	}
	parts := strings.SplitN(mountLabel, ":", 5)
	if len(parts) == 5 {
		return strings.Join(parts[:4], ":")
	}
	return mountLabel
}

func matchesAmbientSELinux(value, expected string) bool {
	actualParts := strings.SplitN(strings.TrimSuffix(value, "\x00"), ":", 4)
	expectedParts := strings.SplitN(expected, ":", 4)
	return len(actualParts) == 4 && len(expectedParts) == 4 &&
		actualParts[1] == expectedParts[1] && actualParts[2] == expectedParts[2] && actualParts[3] == expectedParts[3]
}

// The containers/storage tar exporter includes user.* (except user.overlay.*),
// security.capability, and security.ima. Other xattrs, including POSIX ACLs,
// are silently dropped. A matching Buildah mount label is host policy rather
// than logical image content; a different SELinux label is ineligible.
func hasPortableXattrs(path, ambientSELinux string) (bool, error) {
	attrs, err := system.Llistxattr(path)
	if err != nil {
		if errors.Is(err, system.ENOTSUP) {
			return true, nil
		}
		return false, err
	}
	for _, attr := range attrs {
		if attr == "security.selinux" {
			value, err := system.Lgetxattr(path, attr)
			if err != nil {
				return false, err
			}
			if !matchesAmbientSELinux(string(value), ambientSELinux) {
				return false, nil
			}
			continue
		}
		supported := attr == "security.capability" || attr == "security.ima" ||
			(strings.HasPrefix(attr, "user.") && !strings.HasPrefix(attr, "user.overlay."))
		if !supported {
			return false, nil
		}
		// The exporter silently skips oversized user attributes. Read each
		// accepted attribute now so that a skipped value cannot alias a cache key.
		if _, err := system.Lgetxattr(path, attr); err != nil {
			return false, err
		}
	}
	return true, nil
}

func imageHasPortableMetadata(ctx context.Context, store storage.Store, imageID, ambientSELinux string) (portable bool, retErr error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	image, err := store.Image(imageID)
	if err != nil {
		return false, fmt.Errorf("inspect cache source image: %w", err)
	}
	if image.TopLayer == "" {
		return true, nil
	}
	root, err := store.MountImage(imageID, nil, "")
	if err != nil {
		return false, fmt.Errorf("mount cache source image: %w", err)
	}
	defer func() {
		_, err := store.UnmountImage(imageID, false)
		retErr = errors.Join(retErr, err)
	}()
	portable = true
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().Nanosecond() != 0 {
			portable = false
			return filepath.SkipAll
		}
		valid, err := hasPortableXattrs(path, ambientSELinux)
		if err != nil {
			return err
		}
		if !valid {
			portable = false
			return filepath.SkipAll
		}
		return nil
	})
	return portable, err
}
