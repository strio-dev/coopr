package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/oci"
	"coopr/internal/stateidentity"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"golang.org/x/sys/unix"
)

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
