package buildah

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func TestContainerSourcePathConfinesSourcesToImageRoot(t *testing.T) {
	tests := []struct {
		source, want, message string
	}{
		{"/usr/bin/tool", "usr/bin/tool", ""},
		{"usr/lib/*.so", "usr/lib/*.so", ""},
		{"/", "", ""},
		{"../etc/passwd", "etc/passwd", ""},
		{"/usr/../../etc/passwd", "etc/passwd", ""},
		{"usr/../etc/passwd", "etc/passwd", ""},
		{"https://example.invalid/file", "https:/example.invalid/file", ""},
		{"git@example.invalid:repo", "git@example.invalid:repo", ""},
		{"", "", "empty"},
	}
	for _, test := range tests {
		t.Run(test.source, func(t *testing.T) {
			got, err := containerSourcePath(test.source)
			if test.message == "" {
				if err != nil || got != test.want {
					t.Fatalf("containerSourcePath(%q) = %q, %v; want %q", test.source, got, err, test.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("containerSourcePath(%q) error = %v, want %q", test.source, err, test.message)
			}
		})
	}
}

func TestContainerSourcePathForCopyPreservesParentsPivot(t *testing.T) {
	for _, test := range []struct {
		source string
		want   string
	}{
		{source: "/usr/./lib/tool", want: "usr/./lib/tool"},
		{source: "prefix/./nested/file", want: "prefix/./nested/file"},
		{source: "../outside/./../safe/file", want: "outside/./safe/file"},
		{source: "/./nested/file", want: "././nested/file"},
		{source: "prefix/./", want: "prefix/./."},
		{source: "/plain/file", want: "plain/file"},
	} {
		got, err := containerSourcePathForCopy(test.source, true)
		if err != nil || got != test.want {
			t.Errorf("containerSourcePathForCopy(%q) = %q, %v; want %q", test.source, got, err, test.want)
		}
	}
	got, err := containerSourcePathForCopy("prefix/./nested/file", false)
	if err != nil || got != "prefix/nested/file" {
		t.Fatalf("parents-disabled source = %q, %v", got, err)
	}
}

func TestCopyFromImageTranslatesMappedNonzeroOwners(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah COPY --from mapping test")
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
			t.Errorf("shutdown mapped COPY --from store: %v", err)
		}
	})
	network, err := newNetworkInterface(store)
	if err != nil {
		t.Fatal(err)
	}
	newMappedBuilder := func(uidHost, gidHost uint32) *upstream.Builder {
		mapping := &define.IDMappingOptions{
			UIDMap: []specs.LinuxIDMapping{{ContainerID: 0, HostID: uidHost, Size: 1024}},
			GIDMap: []specs.LinuxIDMapping{{ContainerID: 0, HostID: gidHost, Size: 1024}},
		}
		builder, err := upstream.NewBuilder(ctx, store, upstream.BuilderOptions{
			FromImage: "scratch", PullPolicy: define.PullNever, Isolation: define.IsolationChroot,
			Format: define.OCIv1ImageManifest, NetworkInterface: network, IDMappingOptions: mapping,
			SystemContext: &types.SystemContext{BigFilesTemporaryDir: root},
		})
		if err != nil {
			t.Fatal(err)
		}
		return builder
	}

	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "owned"), []byte("mapped\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := newMappedBuilder(1024, 3072)
	if err := source.Add("/owned", false, upstream.AddAndCopyOptions{ContextDir: contextDir, Chown: "123:456"}, "owned"); err != nil {
		t.Fatal(err)
	}
	sourceImageID, _, _, err := source.Commit(ctx, nil, upstream.CommitOptions{PreferredManifestType: define.OCIv1ImageManifest})
	if err != nil {
		t.Fatal(err)
	}
	sourceMapping := source.IDMappingOptions
	if len(sourceMapping.UIDMap) == 0 || len(sourceMapping.GIDMap) == 0 {
		t.Fatalf("source builder lost ID maps: UID=%v GID=%v", sourceMapping.UIDMap, sourceMapping.GIDMap)
	}
	if err := source.Delete(); err != nil {
		t.Fatal(err)
	}

	destination := newMappedBuilder(5120, 7168)
	defer func() {
		if err := destination.Delete(); err != nil {
			t.Errorf("delete mapped destination builder: %v", err)
		}
	}()
	if err := CopyFromImage(store, sourceImageID, destination, "/copied", false, upstream.AddAndCopyOptions{
		IDMappingOptions: &sourceMapping,
	}, "/owned"); err != nil {
		t.Fatal(err)
	}
	containers, err := store.Containers()
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 || containers[0].ID != destination.ContainerID {
		t.Fatalf("COPY --from temporary builder leaked: %+v", containers)
	}
	destinationRoot, err := destination.Mount(destination.MountLabel)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(destinationRoot, "copied"))
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if got := mappedContainerID(destination.IDMappingOptions.UIDMap, uint32(stat.Uid)); got != 123 {
		t.Fatalf("copied UID = %d (host %d), want 123", got, stat.Uid)
	}
	if got := mappedContainerID(destination.IDMappingOptions.GIDMap, uint32(stat.Gid)); got != 456 {
		t.Fatalf("copied GID = %d (host %d), want 456", got, stat.Gid)
	}
	if err := destination.Unmount(); err != nil {
		t.Fatal(err)
	}
}

func mappedContainerID(mappings []specs.LinuxIDMapping, host uint32) uint32 {
	for _, mapping := range mappings {
		if host >= mapping.HostID && host < mapping.HostID+mapping.Size {
			return mapping.ContainerID + host - mapping.HostID
		}
	}
	return host
}

func TestCopyFromImagePreservesMetadataExcludesAndUnmounts(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah COPY --from test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	store, err := storage.GetStore(storage.StoreOptions{
		GraphDriverName: "vfs",
		GraphRoot:       filepath.Join(root, "graph"),
		RunRoot:         filepath.Join(root, "run"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.Shutdown(true); err != nil {
			t.Errorf("shutdown COPY --from store: %v", err)
		}
	})
	network, err := newNetworkInterface(store)
	if err != nil {
		t.Fatal(err)
	}
	builderOptions := upstream.BuilderOptions{
		FromImage: "scratch", PullPolicy: define.PullNever,
		Isolation: define.IsolationChroot, Format: define.OCIv1ImageManifest,
		NetworkInterface: network,
		SystemContext:    &types.SystemContext{BigFilesTemporaryDir: root},
	}
	newBuilder := func() *upstream.Builder {
		builder, err := upstream.NewBuilder(ctx, store, builderOptions)
		if err != nil {
			t.Fatal(err)
		}
		return builder
	}

	contextDir := filepath.Join(root, "context")
	if err := os.MkdirAll(filepath.Join(contextDir, "payload"), 0o700); err != nil {
		t.Fatal(err)
	}
	keepSource := filepath.Join(contextDir, "payload", "keep")
	if err := os.WriteFile(keepSource, []byte("kept\n"), 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "payload", "drop.tmp"), []byte("excluded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(contextDir, "payload", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"keep", "drop.tmp"} {
		if err := os.WriteFile(filepath.Join(contextDir, "payload", "nested", name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../../../../etc", filepath.Join(contextDir, "payload", "escape")); err != nil {
		t.Fatal(err)
	}

	source := newBuilder()
	if err := source.Add("/payload/", false, upstream.AddAndCopyOptions{
		ContextDir: contextDir, FollowSymlink: types.OptionalBoolFalse,
	}, "payload/keep", "payload/drop.tmp", "payload/escape"); err != nil {
		t.Fatal(err)
	}
	sourceBuilderRoot, err := source.Mount(source.MountLabel)
	if err != nil {
		t.Fatal(err)
	}
	urlLikePath := filepath.Join(sourceBuilderRoot, "https:", "example.invalid", "file")
	if err := os.MkdirAll(filepath.Dir(urlLikePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(urlLikePath, []byte("image path, not a download\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"keep", "drop.tmp"} {
		path := filepath.Join(sourceBuilderRoot, "payload", "nested", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.Unmount(); err != nil {
		t.Fatal(err)
	}
	sourceImageID, _, _, err := source.Commit(ctx, nil, upstream.CommitOptions{
		PreferredManifestType: define.OCIv1ImageManifest,
		SystemContext:         builderOptions.SystemContext,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Delete(); err != nil {
		t.Fatal(err)
	}

	sourceRoot, err := store.MountImage(sourceImageID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	sourceMounted := true
	defer func() {
		if sourceMounted {
			if _, err := store.UnmountImage(sourceImageID, false); err != nil {
				t.Errorf("unmount source image after failed test: %v", err)
			}
		}
	}()
	sourceInfo, err := os.Stat(filepath.Join(sourceRoot, "payload", "keep"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UnmountImage(sourceImageID, false); err != nil {
		t.Fatal(err)
	}
	sourceMounted = false

	destination := newBuilder()
	defer func() {
		if err := destination.Delete(); err != nil {
			t.Errorf("delete destination builder: %v", err)
		}
	}()
	if err := CopyFromImage(store, sourceImageID, destination, "/copied/", false, upstream.AddAndCopyOptions{
		Excludes: []string{"payload/*.tmp", "payload/escape"},
	}, "/payload/"); err != nil {
		t.Fatal(err)
	}
	if err := CopyFromImage(store, sourceImageID, destination, "/normalized/", false, upstream.AddAndCopyOptions{},
		"../payload/keep", "https://example.invalid/file"); err != nil {
		t.Fatal(err)
	}
	if err := CopyFromImage(store, sourceImageID, destination, "/relative/", false, upstream.AddAndCopyOptions{
		Excludes: []string{"nested/*.tmp"},
	}, "/payload"); err != nil {
		t.Fatal(err)
	}
	if err := CopyFromImage(store, sourceImageID, destination, "/parents/", false, upstream.AddAndCopyOptions{
		Parents: true,
	}, "/payload/./nested/keep"); err != nil {
		t.Fatal(err)
	}

	destinationRoot, err := destination.Mount(destination.MountLabel)
	if err != nil {
		t.Fatal(err)
	}
	keepPath := filepath.Join(destinationRoot, "copied", "keep")
	data, err := os.ReadFile(keepPath)
	if err != nil || string(data) != "kept\n" {
		t.Fatalf("copied file = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(destinationRoot, "copied", "drop.tmp")); !os.IsNotExist(err) {
		t.Fatalf("excluded file stat error = %v", err)
	}
	for name, want := range map[string]string{
		filepath.Join("normalized", "keep"):         "kept\n",
		filepath.Join("normalized", "file"):         "image path, not a download\n",
		filepath.Join("relative", "nested", "keep"): "keep\n",
		filepath.Join("parents", "nested", "keep"):  "keep\n",
	} {
		data, err := os.ReadFile(filepath.Join(destinationRoot, name))
		if err != nil || string(data) != want {
			t.Fatalf("normalized source %s = %q, %v; want %q", name, data, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(destinationRoot, "relative", "nested", "drop.tmp")); !os.IsNotExist(err) {
		t.Fatalf("source-relative excluded file stat error = %v", err)
	}
	destinationInfo, err := os.Stat(keepPath)
	if err != nil {
		t.Fatal(err)
	}
	if destinationInfo.Mode().Perm() != sourceInfo.Mode().Perm() {
		t.Fatalf("copied mode = %o, want %o", destinationInfo.Mode().Perm(), sourceInfo.Mode().Perm())
	}
	sourceStat := sourceInfo.Sys().(*syscall.Stat_t)
	destinationStat := destinationInfo.Sys().(*syscall.Stat_t)
	if destinationStat.Uid != sourceStat.Uid || destinationStat.Gid != sourceStat.Gid {
		t.Fatalf("copied owner = %d:%d, want %d:%d", destinationStat.Uid, destinationStat.Gid, sourceStat.Uid, sourceStat.Gid)
	}
	if err := destination.Unmount(); err != nil {
		t.Fatal(err)
	}

	if err := CopyFromImage(store, sourceImageID, destination, "/leak", false, upstream.AddAndCopyOptions{}, "/payload/escape/passwd"); err == nil {
		t.Fatal("symlink source escaped the source image root")
	}
	image, err := store.Image(sourceImageID)
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := store.Mounted(image.TopLayer)
	if err != nil {
		t.Fatal(err)
	}
	if mounted != 0 {
		t.Fatalf("source image remains mounted %d times", mounted)
	}
}
