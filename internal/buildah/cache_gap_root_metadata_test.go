package buildah

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/types"
	"golang.org/x/sys/unix"
)

// This comparator exercises the pinned public Buildah builder and native storage
// directly, without Coopr planning, commits, caches or output adapters.
func TestCacheGapNativeCommitRootMetadataComparator(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	lease, err := acquireStore(cacheTestStore(root))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}()
	system := &types.SystemContext{SignaturePolicyPath: writeComponentTestPolicy(t, root), BigFilesTemporaryDir: root}
	builder, err := upstream.NewBuilder(ctx, lease.store, upstream.BuilderOptions{FromImage: "scratch", PullPolicy: define.PullNever, Isolation: define.IsolationChroot, Format: define.OCIv1ImageManifest, SystemContext: system})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := builder.Delete(); err != nil {
			t.Error(err)
		}
	}()
	mounted, err := builder.Mount(builder.MountLabel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mounted, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(mounted, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(mounted, "user.coopr", []byte("root-metadata"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mounted, "control"), []byte("native child\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := builder.Unmount(); err != nil {
		t.Fatal(err)
	}
	before, err := capturePackageRootMetadata(lease.store, builder)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode&0o7777 != 0o711 || before.UID != 1 || before.GID != 2 || before.PAXRecords["SCHILY.xattr.user.coopr"] != "root-metadata" {
		t.Fatalf("native input metadata=%+v", before)
	}
	id, _, _, err := builder.Commit(ctx, nil, upstream.CommitOptions{PreferredManifestType: define.OCIv1ImageManifest, SystemContext: system})
	if err != nil {
		t.Fatal(err)
	}
	child, err := upstream.NewBuilder(ctx, lease.store, upstream.BuilderOptions{FromImage: id, PullPolicy: define.PullNever, Isolation: define.IsolationChroot, Format: define.OCIv1ImageManifest, SystemContext: system})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := child.Delete(); err != nil {
			t.Error(err)
		}
	}()
	after, err := capturePackageRootMetadata(lease.store, child)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pinned native commit root metadata: before=%+v after=%+v", before, after)
	// A native fix must change the documented limitation rather than leave this
	// comparator silently certifying an obsolete exception.
	if after.Mode&0o7777 == before.Mode&0o7777 && after.UID == before.UID && after.GID == before.GID && after.PAXRecords["SCHILY.xattr.user.coopr"] == before.PAXRecords["SCHILY.xattr.user.coopr"] {
		t.Fatal("native metadata now survives: retire documented upstream limitation")
	}
	mounted, err = child.Mount(child.MountLabel)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := os.ReadFile(filepath.Join(mounted, "control"))
	unmountErr := child.Unmount()
	if readErr != nil || unmountErr != nil || string(data) != "native child\n" {
		t.Fatalf("native child control=%q read=%v unmount=%v", data, readErr, unmountErr)
	}
}
