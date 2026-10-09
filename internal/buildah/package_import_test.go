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

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func TestImportPackageSnapshotRejectsMismatchedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "package.tar")
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	wanted := digest.FromString("expected")
	config, err := json.Marshal(v1.Image{
		Platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH},
		RootFS:   v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{wanted}},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store, err := storage.GetStore(storage.StoreOptions{GraphDriverName: "vfs", GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = store.Shutdown(true) })
	_, _, err = ImportPackageSnapshot(context.Background(), store, nil, oci.Package{
		Stage: "tools", Descriptor: v1.Descriptor{MediaType: oci.ComponentPackageType, Digest: wanted, Size: 7}, Config: config,
	}, path, v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("mismatched package error = %v, want digest rejection", err)
	}
	unsafeConfig := json.RawMessage(`{"architecture":"` + runtime.GOARCH + `","os":"linux","rootfs":{"type":"layers","diff_ids":["` + wanted.String() + `"],"vendor":{"unverified":true}}}`)
	_, _, err = ImportPackageSnapshot(context.Background(), store, nil, oci.Package{
		Stage: "tools", Descriptor: v1.Descriptor{MediaType: oci.ComponentPackageType, Digest: wanted, Size: 7}, Config: unsafeConfig,
	}, path, v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
	if err == nil || !strings.Contains(err.Error(), "unsupported field") {
		t.Fatalf("unsafe package config error = %v, want structural rejection", err)
	}
}

func TestImportPackageSnapshotCanBeOpenedByBuildah(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless package import in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	packagePath := filepath.Join(root, "package.tar")
	layerData := packageImportTar(t)
	if err := os.WriteFile(packagePath, layerData, 0o600); err != nil {
		t.Fatal(err)
	}
	layerDigest := digest.FromBytes(layerData)
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	config := json.RawMessage(`{"architecture":"` + platform.Architecture + `","os":"linux","config":{"Env":["COOPR_PACKAGE=yes"]},"rootfs":{"type":"layers","diff_ids":["` + layerDigest.String() + `"]},"history":[{"created_by":"coopr package snapshot"}],"coopr.test":{"preserve":true}}`)
	pkg := oci.Package{
		Stage:      "tools",
		Descriptor: v1.Descriptor{MediaType: oci.ComponentPackageType, Digest: layerDigest, Size: int64(len(layerData))},
		Config:     config,
	}
	store, err := storage.GetStore(storage.StoreOptions{GraphDriverName: "vfs", GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.Shutdown(true); err != nil {
			t.Errorf("shutdown package import store: %v", err)
		}
	})
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root}
	imageID, rawConfig, err := ImportPackageSnapshot(ctx, store, system, pkg, packagePath, platform)
	if err != nil {
		t.Fatal(err)
	}
	if imageID != digest.FromBytes(config).Encoded() {
		t.Fatalf("package image ID = %q, want %q", imageID, digest.FromBytes(config).Encoded())
	}
	if !bytes.Equal(rawConfig, config) {
		t.Fatalf("returned config changed:\n%s\nwant:\n%s", rawConfig, config)
	}
	storedConfig, err := packageImageConfig(ctx, store, imageID, system)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(storedConfig, config) {
		t.Fatalf("stored config changed:\n%s\nwant:\n%s", storedConfig, config)
	}
	builder, err := upstream.NewBuilder(ctx, store, upstream.BuilderOptions{
		FromImage: imageID, PullPolicy: define.PullNever, Isolation: define.IsolationChroot,
		Format: define.OCIv1ImageManifest, SystemContext: system,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := builder.Delete(); err != nil {
			t.Errorf("delete package import builder: %v", err)
		}
	})
	mountPoint, err := builder.Mount(builder.MountLabel)
	if err != nil {
		t.Fatal(err)
	}
	payload, readErr := os.ReadFile(filepath.Join(mountPoint, "artifact"))
	unmountErr := builder.Unmount()
	if readErr != nil || string(payload) != "package payload\n" {
		t.Fatalf("imported artifact = %q, %v", payload, readErr)
	}
	if unmountErr != nil {
		t.Fatal(unmountErr)
	}
}

func packageImportTar(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	if err := writer.WriteHeader(&tar.Header{Name: ".", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	payload := []byte("package payload\n")
	if err := writer.WriteHeader(&tar.Header{Name: "artifact", Typeflag: tar.TypeReg, Mode: 0o640, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(writer, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
