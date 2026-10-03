package buildah

import (
	"archive/tar"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"coopr/internal/stateidentity"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"golang.org/x/sys/unix"
)

func setTestPOSIXACL(t *testing.T, path string) {
	t.Helper()
	// Linux POSIX ACL xattr v2 with one named user. A basic owner/group/other
	// mode ACL would be folded into the mode and removed by the kernel.
	acl := make([]byte, 4+5*8)
	binary.LittleEndian.PutUint32(acl, 2)
	for index, entry := range []struct {
		tag, permission uint16
		id              uint32
	}{
		{1, 6, ^uint32(0)},  // owner
		{2, 4, 12345},       // named user
		{4, 4, ^uint32(0)},  // group
		{16, 4, ^uint32(0)}, // mask
		{32, 0, ^uint32(0)}, // other
	} {
		offset := 4 + index*8
		binary.LittleEndian.PutUint16(acl[offset:], entry.tag)
		binary.LittleEndian.PutUint16(acl[offset+2:], entry.permission)
		binary.LittleEndian.PutUint32(acl[offset+4:], entry.id)
	}
	if err := unix.Setxattr(path, "system.posix_acl_access", acl, 0); err != nil {
		if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EPERM) {
			t.Skipf("POSIX ACL xattr unavailable: %v", err)
		}
		t.Fatal(err)
	}
}

func TestPackageSnapshotPreservesPortableStateAcrossStores(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live rootless snapshot fidelity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	openStore := func(name string) storage.Store {
		t.Helper()
		store, err := storage.GetStore(storage.StoreOptions{
			GraphDriverName: "vfs", GraphRoot: filepath.Join(root, name, "graph"), RunRoot: filepath.Join(root, name, "run"),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := store.Shutdown(true); err != nil {
				t.Errorf("shutdown %s store: %v", name, err)
			}
		})
		return store
	}
	firstStore, secondStore := openStore("first"), openStore("second")
	network, err := newNetworkInterface(firstStore)
	if err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{BigFilesTemporaryDir: root}
	builder, err := upstream.NewBuilder(ctx, firstStore, upstream.BuilderOptions{
		FromImage: "scratch", PullPolicy: define.PullNever, Isolation: define.IsolationChroot,
		Format: define.OCIv1ImageManifest, NetworkInterface: network, SystemContext: system,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = builder.Delete() }()
	mount, err := builder.Mount(builder.MountLabel)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(mount, "nested")
	if err := os.Mkdir(dataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	dataPath := filepath.Join(dataDir, "data")
	if err := os.WriteFile(dataPath, []byte("portable state\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(dataPath, filepath.Join(dataDir, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(dataPath, "user.coopr", []byte("file-xattr"), 0); err != nil {
		t.Fatal(err)
	}
	fileTime := time.Unix(1_700_000_000, 123_456_789)
	dirTime := time.Unix(1_700_000_100, 987_654_321)
	if err := os.Chtimes(dataPath, fileTime, fileTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dataDir, dirTime, dirTime); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(dataPath); err != nil || !info.ModTime().Equal(fileTime) {
		t.Fatalf("source file mtime before commit = %v, %v", info, err)
	}
	if err := builder.Unmount(); err != nil {
		t.Fatal(err)
	}
	rootMetadata, err := capturePackageRootMetadata(firstStore, builder)
	if err != nil {
		t.Fatal(err)
	}
	firstID, _, _, err := builder.Commit(ctx, nil, upstream.CommitOptions{
		PreferredManifestType: define.OCIv1ImageManifest, SystemContext: system,
	})
	if err != nil {
		t.Fatal(err)
	}
	committedRoot, err := firstStore.MountImage(firstID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	committedInfo, statErr := os.Lstat(filepath.Join(committedRoot, "nested", "data"))
	_, unmountErr := firstStore.UnmountImage(firstID, false)
	if statErr != nil || unmountErr != nil || !committedInfo.ModTime().Equal(fileTime.Truncate(time.Second)) {
		t.Fatalf("committed file mtime = %v, stat=%v unmount=%v", committedInfo, statErr, unmountErr)
	}
	config, err := packageImageConfig(ctx, firstStore, firstID, system)
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	firstPath := filepath.Join(root, "first.tar")
	first, err := SnapshotPackage(ctx, firstStore, PackageSnapshotRequest{
		ImageID: firstID, Stage: "cache", TarPath: firstPath, Platform: platform,
		SystemContext: system, Config: config, RootMetadata: rootMetadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := os.Open(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(archive)
	seen := map[string]bool{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "./nested/" {
			seen["directory"] = header.ModTime.Equal(dirTime.Truncate(time.Second))
		}
		if header.Name == "./nested/data" {
			seen["file"] = header.ModTime.Equal(fileTime.Truncate(time.Second)) && header.PAXRecords["SCHILY.xattr.user.coopr"] == "file-xattr"
		}
		if header.Name == "./nested/hardlink" {
			seen["hardlink"] = header.Typeflag == tar.TypeLink && header.Linkname == "./nested/data"
		}
	}
	_ = archive.Close()
	if !seen["directory"] || !seen["file"] || !seen["hardlink"] {
		t.Fatalf("snapshot lost source metadata: %+v", seen)
	}
	firstIdentity := snapshotTestIdentity(t, ctx, firstPath, rootMetadata, config)
	secondID, secondConfig, err := ImportPackageSnapshot(ctx, secondStore, system, first, firstPath, platform)
	if err != nil {
		t.Fatal(err)
	}
	secondPath := filepath.Join(root, "second.tar")
	if _, err := SnapshotPackage(ctx, secondStore, PackageSnapshotRequest{
		ImageID: secondID, Stage: "cache", TarPath: secondPath, Platform: platform,
		SystemContext: system, Config: secondConfig, RootMetadata: rootMetadata,
	}); err != nil {
		t.Fatal(err)
	}
	secondIdentity := snapshotTestIdentity(t, ctx, secondPath, rootMetadata, secondConfig)
	if firstIdentity != secondIdentity {
		t.Fatalf("portable state differs after independent import: before=%+v after=%+v", firstIdentity, secondIdentity)
	}
	// The storage tar exporter cannot encode POSIX ACLs. A cache snapshot
	// must refuse this state rather than silently create a warm output with
	// weaker permissions than the cold committed image.
	committedRoot, err = firstStore.MountImage(firstID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	setTestPOSIXACL(t, filepath.Join(committedRoot, "nested", "data"))
	portable, checkErr := hasPortableXattrs(filepath.Join(committedRoot, "nested", "data"), rootMetadata.AmbientSELinux)
	_, unmountErr = firstStore.UnmountImage(firstID, false)
	if checkErr != nil || unmountErr != nil || portable {
		t.Fatalf("ACL was not recognized as unportable: portable=%v check=%v unmount=%v", portable, checkErr, unmountErr)
	}
	_, _, cachePath, eligible, err := snapshotPortableState(ctx, firstStore, system, firstID, config, config, rootMetadata, platform, root)
	if cachePath != "" {
		_ = os.Remove(cachePath)
	}
	if err != nil || eligible {
		t.Fatalf("ACL-bearing image was cacheable: eligible=%v err=%v", eligible, err)
	}
}

func snapshotTestIdentity(t *testing.T, ctx context.Context, path string, root *PackageRootMetadata, config []byte) stateidentity.Identity {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	identity, err := stateidentity.Calculate(ctx, root.tarHeader(), file, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}
