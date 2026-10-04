package buildah

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/idtools"
	"go.podman.io/storage/pkg/system"
	"golang.org/x/sys/unix"
)

func TestCacheablePackageRootMetadataNormalizesExecutionPolicy(t *testing.T) {
	original := &PackageRootMetadata{
		Mode: 0o711, UID: 1, GID: 2, AmbientSELinux: "system_u:object_r:container_file_t:s0",
		PAXRecords: map[string]string{
			"SCHILY.xattr.user.coopr":       "value",
			"SCHILY.xattr.security.selinux": "system_u:object_r:container_file_t:s0:c1,c2",
		},
	}
	got, ok := cacheablePackageRootMetadata(original)
	if !ok {
		t.Fatal("portable root metadata was rejected")
	}
	if got.Mode != 0o711 || got.UID != 1 || got.GID != 2 || got.PAXRecords["SCHILY.xattr.user.coopr"] != "value" {
		t.Fatalf("normalized metadata = %+v", got)
	}
	if _, present := got.PAXRecords["SCHILY.xattr.security.selinux"]; present || got.AmbientSELinux != "" {
		t.Fatalf("normalized metadata retained execution policy: %+v", got)
	}
	got.PAXRecords["SCHILY.xattr.user.coopr"] = "changed"
	if original.PAXRecords["SCHILY.xattr.user.coopr"] != "value" {
		t.Fatal("normalization aliased the caller's PAX records")
	}
	for _, invalid := range []*PackageRootMetadata{
		nil,
		{Mode: -1},
		{Mode: 0o10000},
		{Mode: 0o755, UID: -1},
		{Mode: 0o755, GID: -1},
		{Mode: 0o755, UnportableXattrs: true},
	} {
		if _, ok := cacheablePackageRootMetadata(invalid); ok {
			t.Fatalf("invalid metadata was cacheable: %+v", invalid)
		}
	}
}

func TestInstructionCacheRootMetadataSidecar(t *testing.T) {
	store := newInstructionCacheTestStore(t)
	key := digest.FromString("root metadata cache")
	imageID := createInstructionCacheTestImage(t, store)
	metadata := &PackageRootMetadata{
		Mode: 0o711, UID: 1, GID: 2,
		PAXRecords: map[string]string{
			"SCHILY.xattr.user.coopr":       "value",
			"SCHILY.xattr.security.selinux": "host-policy",
		},
	}
	if err := storeInstructionCacheEntry(store, imageID, key, metadata); err != nil {
		t.Fatal(err)
	}
	entry, err := findInstructionCacheEntry(store, key)
	if err != nil {
		t.Fatal(err)
	}
	if entry.ImageID != imageID || entry.RootMetadata == nil || entry.RootMetadata.Mode != 0o711 || entry.RootMetadata.UID != 1 || entry.RootMetadata.GID != 2 {
		t.Fatalf("cache entry = %+v", entry)
	}
	if got := entry.RootMetadata.PAXRecords["SCHILY.xattr.user.coopr"]; got != "value" {
		t.Fatalf("cache root xattr = %q", got)
	}
	if _, present := entry.RootMetadata.PAXRecords["SCHILY.xattr.security.selinux"]; present {
		t.Fatalf("cache root retained SELinux mount policy: %+v", entry.RootMetadata)
	}
}

func TestCheckpointPreservingPackageRootRestoresMetadata(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless package checkpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	store, err := storage.GetStore(storage.StoreOptions{
		GraphDriverName: "vfs", GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.Shutdown(true); err != nil {
			t.Errorf("shutdown package checkpoint store: %v", err)
		}
	})
	network, err := newNetworkInterface(store)
	if err != nil {
		t.Fatal(err)
	}
	options := upstream.BuilderOptions{
		FromImage: "scratch", PullPolicy: define.PullNever,
		Isolation: define.IsolationChroot, Format: define.OCIv1ImageManifest,
		NetworkInterface: network,
		SystemContext:    &types.SystemContext{BigFilesTemporaryDir: root},
	}
	builder, err := upstream.NewBuilder(ctx, store, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreRootMetadataFixture(store, builder); err != nil {
		t.Fatal(err)
	}
	replacement, _, before, err := checkpointPreservingPackageRoot(ctx, store, builder, options, false, timestampPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := replacement.Delete(); err != nil {
			t.Errorf("delete replacement builder: %v", err)
		}
	}()
	after, err := capturePackageRootMetadata(store, replacement)
	if err != nil {
		t.Fatal(err)
	}
	for name, metadata := range map[string]*PackageRootMetadata{"before": before, "after": after} {
		if metadata == nil {
			t.Fatalf("%s checkpoint root metadata is nil", name)
		}
		if got := metadata.Mode & 0o7777; got != 0o711 || metadata.UID != 1 || metadata.GID != 2 {
			t.Fatalf("%s checkpoint root metadata = mode %#o uid %d gid %d, want 0711/1/2", name, got, metadata.UID, metadata.GID)
		}
		if got := metadata.PAXRecords["SCHILY.xattr.user.coopr"]; got != "root-metadata" {
			t.Fatalf("%s checkpoint root xattr = %q, want root-metadata", name, got)
		}
	}
}

func TestPackageRootCacheWithInheritedSELinuxAndEmptyMountLabel(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live inherited SELinux cache coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	store, err := storage.GetStore(storage.StoreOptions{
		GraphDriverName: "vfs", GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.Shutdown(true); err != nil {
			t.Errorf("shutdown inherited SELinux store: %v", err)
		}
	})
	network, err := newNetworkInterface(store)
	if err != nil {
		t.Fatal(err)
	}
	systemContext := &types.SystemContext{BigFilesTemporaryDir: root}
	options := upstream.BuilderOptions{
		FromImage: "scratch", PullPolicy: define.PullNever, Isolation: define.IsolationChroot,
		Format: define.OCIv1ImageManifest, NetworkInterface: network, SystemContext: systemContext,
	}
	builder, err := upstream.NewBuilder(ctx, store, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = builder.Delete() })
	if builder.MountLabel != "" {
		t.Skipf("Buildah supplies a native SELinux mount label: %q", builder.MountLabel)
	}
	// Read the inherited context independently of production classification.
	// This fixture never changes SELinux labels or global SELinux state.
	graphLabelBytes, err := system.Lgetxattr(store.GraphRoot(), "security.selinux")
	if errors.Is(err, system.ENOTSUP) || errors.Is(err, unix.ENODATA) || errors.Is(err, system.ErrNotSupportedPlatform) {
		t.Skipf("storage graphroot SELinux context unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	graphLabel := strings.TrimSuffix(string(graphLabelBytes), "\x00")
	if graphLabel == "" {
		t.Skip("storage graphroot has no inherited SELinux context")
	}
	mount, err := builder.Mount(builder.MountLabel)
	if err != nil {
		t.Fatal(err)
	}
	rootLabelBytes, labelErr := system.Lgetxattr(mount, "security.selinux")
	unmountErr := builder.Unmount()
	if labelErr != nil || unmountErr != nil {
		t.Fatalf("read fresh scratch root context: label=%v unmount=%v", labelErr, unmountErr)
	}
	t.Logf("independent SELinux baseline: MountLabel=%q graphroot=%q fresh scratch root=%q", builder.MountLabel, graphLabel, strings.TrimSuffix(string(rootLabelBytes), "\x00"))
	if rootLabel := strings.TrimSuffix(string(rootLabelBytes), "\x00"); rootLabel != graphLabel {
		t.Fatalf("fresh scratch root context = %q, want full graphroot context %q", rootLabel, graphLabel)
	}
	if err := restoreRootMetadataFixture(store, builder); err != nil {
		t.Fatal(err)
	}
	mount, err = builder.Mount(builder.MountLabel)
	if err != nil {
		t.Fatal(err)
	}
	dataPath := filepath.Join(mount, "data")
	writeErr := os.WriteFile(dataPath, []byte("inherited SELinux descendant\n"), 0o640)
	unmountErr = builder.Unmount()
	if writeErr != nil || unmountErr != nil {
		t.Fatalf("create descendant: write=%v unmount=%v", writeErr, unmountErr)
	}
	metadata, err := capturePackageRootMetadata(store, builder)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.AmbientSELinux != graphLabel || metadata.UnportableXattrs {
		t.Fatalf("inherited root context was not cacheable: metadata=%+v graphroot=%q", metadata, graphLabel)
	}
	normalized, cacheable := cacheablePackageRootMetadata(metadata)
	if !cacheable {
		t.Fatal("root metadata with inherited SELinux context was rejected by cache normalization")
	}
	if _, present := normalized.PAXRecords["SCHILY.xattr.security.selinux"]; present || normalized.AmbientSELinux != "" {
		t.Fatalf("normalized cache metadata retained SELinux policy: %+v", normalized)
	}
	replacement, imageID, before, err := checkpointPreservingPackageRoot(ctx, store, builder, options, false, timestampPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replacement.Delete() })
	after, err := capturePackageRootMetadata(store, replacement)
	if err != nil {
		t.Fatal(err)
	}
	for name, metadata := range map[string]*PackageRootMetadata{"before": before, "after": after} {
		if metadata == nil || metadata.Mode&0o7777 != 0o711 || metadata.UID != 1 || metadata.GID != 2 || metadata.PAXRecords["SCHILY.xattr.user.coopr"] != "root-metadata" || metadata.UnportableXattrs || metadata.AmbientSELinux != graphLabel {
			t.Fatalf("%s checkpoint lost portable root metadata: %+v", name, metadata)
		}
	}
	config, err := packageImageConfig(ctx, store, imageID, systemContext)
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	t.Run("descendant_snapshot", func(t *testing.T) {
		_, _, path, eligible, err := snapshotPortableState(ctx, store, systemContext, imageID, config, config, before, platform, root)
		if path != "" {
			t.Cleanup(func() {
				if err := os.Remove(path); err != nil {
					t.Errorf("remove descendant snapshot: %v", err)
				}
			})
		}
		if err != nil || !eligible || path == "" {
			t.Fatalf("inherited-context descendant snapshot: eligible=%v path=%q err=%v", eligible, path, err)
		}
	})
	t.Run("descendant_acl_is_ineligible", func(t *testing.T) {
		mount, err := store.MountImage(imageID, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if _, err := store.UnmountImage(imageID, false); err != nil {
					t.Errorf("unmount ACL fixture: %v", err)
				}
			}()
			setTestPOSIXACL(t, filepath.Join(mount, "data"))
		}()
		_, _, path, eligible, err := snapshotPortableState(ctx, store, systemContext, imageID, config, config, before, platform, root)
		if path != "" {
			t.Cleanup(func() {
				if err := os.Remove(path); err != nil {
					t.Errorf("remove ACL descendant snapshot: %v", err)
				}
			})
		}
		if err != nil || eligible || path != "" {
			t.Fatalf("ACL-bearing descendant snapshot: eligible=%v path=%q err=%v", eligible, path, err)
		}
	})
}

func restoreRootMetadataFixture(store storage.Store, builder *upstream.Builder) (retErr error) {
	container, err := store.Container(builder.ContainerID)
	if err != nil {
		return err
	}
	layer, err := store.Layer(container.LayerID)
	if err != nil {
		return err
	}
	hostUID, err := idtools.RawToHost(1, layer.UIDMap)
	if err != nil {
		return err
	}
	hostGID, err := idtools.RawToHost(2, layer.GIDMap)
	if err != nil {
		return err
	}
	root, err := builder.Mount(builder.MountLabel)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, builder.Unmount()) }()
	if err := os.Chmod(root, 0o711); err != nil {
		return err
	}
	if err := os.Chown(root, hostUID, hostGID); err != nil {
		return err
	}
	return unix.Setxattr(root, "user.coopr", []byte("root-metadata"), 0)
}
