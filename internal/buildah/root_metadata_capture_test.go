package buildah

import (
	"archive/tar"
	"encoding/binary"
	"reflect"
	"syscall"
	"testing"

	storagearchive "go.podman.io/storage/pkg/archive"
	"go.podman.io/storage/pkg/chrootarchive"
	"go.podman.io/storage/pkg/idtools"
	"golang.org/x/sys/unix"
)

func TestNormalizeRootCapabilityIDMatchesStorageArchiveRules(t *testing.T) {
	value := make([]byte, 24)
	binary.LittleEndian.PutUint32(value[:4], 0x03000001)
	uidMap := []idtools.IDMap{{ContainerID: 0, HostID: 100000, Size: 65536}}
	for _, test := range []struct {
		name         string
		rootID       uint32
		wantSize     int
		wantRevision uint32
		wantRootID   uint32
	}{
		{"container root", 100000, 20, 0x02000000, 0},
		{"mapped nonroot", 100001, 24, 0x03000000, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			binary.LittleEndian.PutUint32(value[20:24], test.rootID)
			got, err := normalizeRootCapabilityID(value, uidMap, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != test.wantSize || binary.LittleEndian.Uint32(got[:4])&0xff000000 != test.wantRevision {
				t.Fatalf("normalized capability size=%d revision=%#x", len(got), binary.LittleEndian.Uint32(got[:4])&0xff000000)
			}
			if len(got) == 24 && binary.LittleEndian.Uint32(got[20:24]) != test.wantRootID {
				t.Fatalf("normalized capability root ID = %d, want %d", binary.LittleEndian.Uint32(got[20:24]), test.wantRootID)
			}
		})
	}
	binary.LittleEndian.PutUint32(value[20:24], 200000)
	if _, err := normalizeRootCapabilityID(value, uidMap, nil); err == nil {
		t.Fatal("unmappable capability root ID was accepted")
	}
}

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

func BenchmarkPackageRootMetadataCapture(b *testing.B) {
	root := b.TempDir()
	if err := unix.Setxattr(root, "user.coopr", []byte("root-metadata"), 0); err != nil {
		b.Skipf("user xattrs unavailable: %v", err)
	}
	b.Run("direct", func(b *testing.B) {
		for range b.N {
			if _, err := packageRootMetadataFromMount(root, nil, nil, "", ""); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("archive", func(b *testing.B) {
		for range b.N {
			stream, err := chrootarchive.Tar(root, &storagearchive.TarOptions{
				Compression: storagearchive.Uncompressed, IncludeSourceDir: true,
			}, root)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := tar.NewReader(stream).Next(); err != nil {
				_ = stream.Close()
				b.Fatal(err)
			}
			if err := stream.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
