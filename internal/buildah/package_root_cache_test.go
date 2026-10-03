package buildah

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/idtools"
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
