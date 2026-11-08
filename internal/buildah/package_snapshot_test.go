package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/stateidentity"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"golang.org/x/sys/unix"
)

func TestWritePackageTarWaitsForProducerCompletion(t *testing.T) {
	archive, root := packageStreamFixture(t)
	for _, cancelDrain := range []bool{false, true} {
		name := "delayed EOF"
		if cancelDrain {
			name = "canceled drain"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			tail := packageStreamReadFunc(func(buffer []byte) (int, error) {
				close(entered)
				select {
				case <-release:
					return 0, io.EOF
				case <-ctx.Done():
					// Let the blocked read finish, then require contextReader to
					// reject the next read rather than accepting archive success.
					buffer[0] = 0
					return 1, nil
				}
			})
			done := make(chan error, 1)
			go func() {
				_, err := writePackageTar(ctx, io.Discard, io.MultiReader(bytes.NewReader(archive), tail), root, nil, nil)
				done <- err
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("returned before producer completion: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("did not read beyond tar EOF")
			}
			select {
			case err := <-done:
				t.Fatalf("returned while producer was still pending: %v", err)
			default:
			}
			var want error
			if cancelDrain {
				want = context.Canceled
				cancel()
			} else {
				release <- struct{}{}
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("producer completion = %v, want %v", err, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("producer completion was not observed")
			}
		})
	}
}

func TestWritePackageTarPropagatesProducerError(t *testing.T) {
	archive, root := packageStreamFixture(t)
	want := errors.New("archive producer failed after tar EOF")
	for _, metadata := range []*tar.Header{nil, root} {
		source := io.MultiReader(bytes.NewReader(archive), iotest.ErrReader(want))
		if _, err := writePackageTar(context.Background(), io.Discard, source, metadata, nil, nil); !errors.Is(err, want) {
			t.Fatalf("producer error with root rewrite %t = %v, want %v", metadata != nil, err, want)
		}
	}
}

func TestWritePackageTarPaddingPreservesOutput(t *testing.T) {
	archive, root := packageStreamFixture(t)
	padded := append(bytes.Clone(archive), make([]byte, 8192)...)
	for _, metadata := range []*tar.Header{nil, root} {
		var expected bytes.Buffer
		if _, err := writePackageTar(context.Background(), &expected, bytes.NewReader(archive), metadata, nil, nil); err != nil {
			t.Fatal(err)
		}
		if metadata == nil {
			expected.Write(make([]byte, 8192))
		}
		source := bytes.NewReader(padded)
		var actual bytes.Buffer
		size, err := writePackageTar(context.Background(), &actual, source, metadata, nil, nil)
		if err != nil || size != int64(actual.Len()) || !bytes.Equal(actual.Bytes(), expected.Bytes()) {
			t.Fatalf("padding changed output with root rewrite %t: size=%d, bytes=%d, err=%v", metadata != nil, size, actual.Len(), err)
		}
		if source.Len() != 0 {
			t.Fatalf("left %d producer bytes unread", source.Len())
		}
	}
}

type packageStreamReadFunc func([]byte) (int, error)

func (read packageStreamReadFunc) Read(buffer []byte) (int, error) { return read(buffer) }

func packageStreamFixture(t *testing.T) ([]byte, *tar.Header) {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	root := &tar.Header{Name: ".", Typeflag: tar.TypeDir, Mode: 0o755}
	if err := writer.WriteHeader(root); err != nil {
		t.Fatal(err)
	}
	payload := "package payload\n"
	if err := writer.WriteHeader(&tar.Header{Name: "artifact", Mode: 0o640, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	root.Mode = 0o711
	return archive.Bytes(), root
}

func TestSnapshotPackageValidatesRequestBeforeWriting(t *testing.T) {
	output := filepath.Join(t.TempDir(), "package.tar")
	if err := os.WriteFile(output, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := SnapshotPackage(context.Background(), nil, PackageSnapshotRequest{
		ImageID: "image", Stage: "pkg", TarPath: output,
		Platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH},
	})
	if err == nil {
		t.Fatal("nil store was accepted")
	}
	data, readErr := os.ReadFile(output)
	if readErr != nil || string(data) != "unchanged" {
		t.Fatalf("existing output changed to %q: %v", data, readErr)
	}
}

func TestSnapshotPackageFlattensCommittedImage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah package snapshot in short mode")
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
			t.Errorf("shutdown package snapshot store: %v", err)
		}
	})
	network, err := newNetworkInterface(store)
	if err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{BigFilesTemporaryDir: root}
	builder, err := upstream.NewBuilder(ctx, store, upstream.BuilderOptions{
		FromImage: "scratch", PullPolicy: define.PullNever,
		Isolation: define.IsolationChroot, Format: define.OCIv1ImageManifest,
		NetworkInterface: network, SystemContext: system,
	})
	if err != nil {
		t.Fatal(err)
	}
	deleteBuilder := true
	defer func() {
		if deleteBuilder {
			if err := builder.Delete(); err != nil {
				t.Errorf("delete package builder: %v", err)
			}
		}
	}()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "artifact"), []byte("package payload\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(filepath.Join(contextDir, "artifact"), fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}
	if err := builder.Add("/artifact", false, upstream.AddAndCopyOptions{ContextDir: contextDir}, "artifact"); err != nil {
		t.Fatal(err)
	}
	mountPoint, err := builder.Mount(builder.MountLabel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mountPoint, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(mountPoint, "user.coopr", []byte("root-metadata"), 0); err != nil {
		t.Fatal(err)
	}
	if err := builder.Unmount(); err != nil {
		t.Fatal(err)
	}
	builder.SetEnv("COOPR_PACKAGE", "yes")
	builder.SetCreatedBy("COPY artifact /artifact")
	rootMetadata, err := capturePackageRootMetadata(store, builder)
	if err != nil {
		t.Fatal(err)
	}
	imageID, _, _, err := builder.Commit(ctx, nil, upstream.CommitOptions{
		PreferredManifestType: define.OCIv1ImageManifest,
		SystemContext:         system,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.Delete(); err != nil {
		t.Fatal(err)
	}
	deleteBuilder = false
	rawConfig, err := packageImageConfig(ctx, store, imageID, system)
	if err != nil {
		t.Fatal(err)
	}
	var rawDocument map[string]json.RawMessage
	if err := json.Unmarshal(rawConfig, &rawDocument); err != nil {
		t.Fatal(err)
	}
	rawDocument["future"] = json.RawMessage(`{"keep":true}`)
	rawConfig, err = json.Marshal(rawDocument)
	if err != nil {
		t.Fatal(err)
	}

	platform := v1.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	firstPath := filepath.Join(root, "first", "package.tar")
	_, err = SnapshotPackage(ctx, store, PackageSnapshotRequest{
		ImageID: imageID, Stage: "tools", TarPath: firstPath, Platform: platform, SystemContext: system,
		Config: rawConfig,
	})
	if err == nil || !strings.Contains(err.Error(), "root metadata captured before commit is required") {
		t.Fatalf("snapshot without root metadata: %v", err)
	}
	_, err = SnapshotPackage(ctx, store, PackageSnapshotRequest{
		ImageID: imageID, Stage: "tools", TarPath: firstPath, Platform: platform, SystemContext: system,
		RootMetadata: rootMetadata,
	})
	if err == nil || !strings.Contains(err.Error(), "authoritative raw OCI config sidecar") {
		t.Fatalf("snapshot without raw config sidecar: %v", err)
	}
	wrongRoot := map[string]any{"type": "layers", "diff_ids": []string{digest.FromString("different-rootfs").String()}}
	wrongRootJSON, err := json.Marshal(wrongRoot)
	if err != nil {
		t.Fatal(err)
	}
	rawDocument["rootfs"] = wrongRootJSON
	wrongConfig, err := json.Marshal(rawDocument)
	if err != nil {
		t.Fatal(err)
	}
	_, err = SnapshotPackage(ctx, store, PackageSnapshotRequest{
		ImageID: imageID, Stage: "tools", TarPath: firstPath, Platform: platform, SystemContext: system,
		Config: wrongConfig, RootMetadata: rootMetadata,
	})
	if err == nil || !strings.Contains(err.Error(), "committed rootfs provenance") {
		t.Fatalf("mismatched config sidecar: %v", err)
	}
	first, err := SnapshotPackage(ctx, store, PackageSnapshotRequest{
		ImageID: imageID, Stage: "tools", TarPath: firstPath, Platform: platform, SystemContext: system,
		Config: rawConfig, RootMetadata: rootMetadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Stage != "tools" || first.Descriptor.MediaType != oci.ComponentPackageType || first.Descriptor.Size <= 0 {
		t.Fatalf("package metadata = %+v", first)
	}
	importedID, importedConfig, err := ImportPackageSnapshot(ctx, store, system, first, firstPath, platform)
	if err != nil {
		t.Fatalf("import package through Buildah consumer: %v", err)
	}
	if importedID == "" || !bytes.Equal(importedConfig, first.Config) {
		t.Fatalf("imported package ID=%q config=%s; want exact config sidecar", importedID, importedConfig)
	}
	firstFile, err := os.Open(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := stateidentity.Calculate(ctx, rootMetadata.tarHeader(), firstFile, first.Config, nil)
	_ = firstFile.Close()
	if err != nil {
		t.Fatalf("identity of original snapshot: %v", err)
	}
	importedPath := filepath.Join(root, "imported.tar")
	if _, err := SnapshotPackage(ctx, store, PackageSnapshotRequest{
		ImageID: importedID, Stage: "tools", TarPath: importedPath, Platform: platform, SystemContext: system,
		Config: importedConfig, RootMetadata: rootMetadata,
	}); err != nil {
		t.Fatalf("snapshot imported package: %v", err)
	}
	importedFile, err := os.Open(importedPath)
	if err != nil {
		t.Fatal(err)
	}
	after, err := stateidentity.Calculate(ctx, rootMetadata.tarHeader(), importedFile, importedConfig, nil)
	_ = importedFile.Close()
	if err != nil {
		t.Fatalf("identity of imported snapshot: %v", err)
	}
	if before != after {
		t.Fatalf("package snapshot/import changed logical state: before=%+v after=%+v", before, after)
	}
	observed, _, observedPath, eligible, err := snapshotPortableState(ctx, store, system, imageID, rawConfig, rawConfig, rootMetadata, platform, root)
	if err != nil || !eligible {
		t.Fatalf("original package is not cache observable: eligible=%v err=%v", eligible, err)
	}
	defer func() { _ = os.Remove(observedPath) }()
	restored, _, restoredPath, eligible, err := snapshotPortableState(ctx, store, system, importedID, importedConfig, importedConfig, rootMetadata, platform, root)
	if err != nil || !eligible {
		t.Fatalf("imported package is not cache observable: eligible=%v err=%v", eligible, err)
	}
	defer func() { _ = os.Remove(restoredPath) }()
	if observed != restored {
		t.Fatalf("portable observation changed across import: before=%+v after=%+v", observed, restored)
	}
	file, err := os.Open(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	actualDigest, err := digest.FromReader(file)
	_ = file.Close()
	if err != nil || actualDigest != first.Descriptor.Digest {
		t.Fatalf("package digest = %s, %v; want %s", actualDigest, err, first.Descriptor.Digest)
	}
	assertPackageTarFile(t, firstPath, "artifact", "package payload\n")
	rootHeader := packageTarRootHeader(t, firstPath)
	if rootHeader.Mode&0o7777 != 0o711 || rootHeader.Uid != 0 || rootHeader.Gid != 0 {
		t.Fatalf("package root metadata mode=%#o uid=%d gid=%d", rootHeader.Mode&0o7777, rootHeader.Uid, rootHeader.Gid)
	}
	if got := rootHeader.PAXRecords["SCHILY.xattr.user.coopr"]; got != "root-metadata" {
		t.Fatalf("package root xattr = %q, want root-metadata; pax=%v", got, rootHeader.PAXRecords)
	}

	var config v1.Image
	if err := json.Unmarshal(first.Config, &config); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(first.Config, []byte(`"future":{"keep":true}`)) {
		t.Fatalf("package lost raw config sidecar extension: %s", first.Config)
	}
	if config.OS != platform.OS || config.Architecture != platform.Architecture || len(config.RootFS.DiffIDs) != 1 || config.RootFS.DiffIDs[0] != first.Descriptor.Digest {
		t.Fatalf("package config = %+v", config)
	}
	if len(config.Config.Env) != 1 || config.Config.Env[0] != "COOPR_PACKAGE=yes" {
		t.Fatalf("package env = %#v", config.Config.Env)
	}
	nonemptyHistory := 0
	for _, entry := range config.History {
		if !entry.EmptyLayer {
			nonemptyHistory++
		}
	}
	if nonemptyHistory != 1 {
		t.Fatalf("package history represents %d layers: %+v", nonemptyHistory, config.History)
	}

	second, err := SnapshotPackage(ctx, store, PackageSnapshotRequest{
		ImageID: imageID, Stage: "tools", TarPath: filepath.Join(root, "second.tar"), Platform: platform, SystemContext: system,
		Config: rawConfig, RootMetadata: rootMetadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Descriptor.Digest != first.Descriptor.Digest || second.Descriptor.Size != first.Descriptor.Size || second.Descriptor.MediaType != first.Descriptor.MediaType || !bytes.Equal(second.Config, first.Config) {
		t.Fatalf("repeated snapshot differs: first=%+v second=%+v", first.Descriptor, second.Descriptor)
	}

	emptyBuilder, err := upstream.NewBuilder(ctx, store, upstream.BuilderOptions{
		FromImage: "scratch", PullPolicy: define.PullNever,
		Isolation: define.IsolationChroot, Format: define.OCIv1ImageManifest,
		NetworkInterface: network, SystemContext: system,
	})
	if err != nil {
		t.Fatal(err)
	}
	emptyImageID, _, _, err := emptyBuilder.Commit(ctx, nil, upstream.CommitOptions{
		PreferredManifestType: define.OCIv1ImageManifest,
		EmptyLayerIfEmptyDiff: true,
		SystemContext:         system,
	})
	if err != nil {
		_ = emptyBuilder.Delete()
		t.Fatal(err)
	}
	if err := emptyBuilder.Delete(); err != nil {
		t.Fatal(err)
	}
	emptyConfig, err := packageImageConfig(ctx, store, emptyImageID, system)
	if err != nil {
		t.Fatal(err)
	}
	emptyPath := filepath.Join(root, "empty.tar")
	empty, err := SnapshotPackage(ctx, store, PackageSnapshotRequest{
		ImageID: emptyImageID, Stage: "empty", TarPath: emptyPath, Platform: platform, SystemContext: system,
		Config: emptyConfig,
	})
	if err != nil {
		t.Fatal(err)
	}
	if headers := packageTarHeaders(t, emptyPath); len(headers) != 0 {
		t.Fatalf("empty package has filesystem entries: %v", headers)
	}
	if empty.Descriptor.Size <= 0 || empty.Descriptor.Digest == "" {
		t.Fatalf("empty package descriptor = %+v", empty.Descriptor)
	}

	stored, err := store.Image(imageID)
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := store.Mounted(stored.TopLayer)
	if err != nil || mounted != 0 {
		t.Fatalf("package source mounted=%d, err=%v", mounted, err)
	}
}

func packageTarHeaders(t *testing.T, archivePath string) []string {
	t.Helper()
	file, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	var headers []string
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return headers
		}
		if err != nil {
			t.Fatal(err)
		}
		headers = append(headers, header.Name)
	}
}

func assertPackageTarFile(t *testing.T, archivePath, name, contents string) {
	t.Helper()
	file, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if filepath.ToSlash(header.Name) != name && filepath.ToSlash(header.Name) != "./"+name {
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != contents {
			t.Fatalf("tar %s contents = %q", name, data)
		}
		return
	}
	t.Fatalf("tar contains no %s", name)
}

func packageTarRootHeader(t *testing.T, archivePath string) *tar.Header {
	t.Helper()
	file, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			t.Fatal("tar contains no root directory header")
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "." || header.Name == "./" {
			return header
		}
	}
}
