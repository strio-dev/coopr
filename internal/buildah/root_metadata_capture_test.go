package buildah

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	storagearchive "go.podman.io/storage/pkg/archive"
	"go.podman.io/storage/pkg/idtools"
	"golang.org/x/sys/unix"
)

func TestPackageRootMetadataFromMountMatchesArchiveHeader(t *testing.T) {
	root := t.TempDir()
	if err := unix.Chmod(root, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(root, "user.coopr", []byte("root-metadata"), 0); err != nil {
		t.Skipf("user xattrs unavailable: %v", err)
	}
	var stat syscall.Stat_t
	if err := syscall.Lstat(root, &stat); err != nil {
		t.Fatal(err)
	}
	uidMap := []idtools.IDMap{{ContainerID: 123, HostID: int(stat.Uid), Size: 1}}
	gidMap := []idtools.IDMap{{ContainerID: 456, HostID: int(stat.Gid), Size: 1}}

	got, err := packageRootMetadataFromMount(root, uidMap, gidMap, "", "")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := storagearchive.TarWithOptions(root, &storagearchive.TarOptions{
		Compression: storagearchive.Uncompressed, IncludeSourceDir: true,
		UIDMaps: uidMap, GIDMaps: gidMap,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stream.Close(); err != nil {
			t.Error(err)
		}
	})
	want, err := tar.NewReader(stream).Next()
	if err != nil {
		t.Fatal(err)
	}
	if want.Typeflag != tar.TypeDir {
		t.Fatalf("archive root type = %d, want directory", want.Typeflag)
	}
	if got.Mode != want.Mode || got.UID != want.Uid || got.GID != want.Gid || !reflect.DeepEqual(got.PAXRecords, want.PAXRecords) {
		t.Fatalf("direct root metadata = %+v, archive header mode=%#o uid=%d gid=%d pax=%v", got, want.Mode, want.Uid, want.Gid, want.PAXRecords)
	}
	portable, portableErr := hasPortableXattrs(root, "")
	if got.UnportableXattrs != (portableErr != nil || !portable) {
		t.Fatalf("direct root portability = %v, hasPortableXattrs=(%v, %v)", got.UnportableXattrs, portable, portableErr)
	}
}

func TestPackageRootMetadataFromMountRejectsNonDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(root, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := packageRootMetadataFromMount(root, nil, nil, "", ""); err == nil {
		t.Fatal("non-directory root accepted")
	}
	if _, err := packageRootMetadataFromMount(root+"-missing", nil, nil, "", ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing root error = %v", err)
	}
}
