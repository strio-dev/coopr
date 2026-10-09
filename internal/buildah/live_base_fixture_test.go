package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.podman.io/storage"

	"coopr/internal/oci"
	registryserver "github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
)

const liveBusyBoxReference = "fixture.local/coopr/busybox:latest"

type liveBaseFixture struct {
	reference string
	graphRoot string
}

type liveBusyBoxGraph struct {
	store      *orasoci.Store
	layout     string
	manifest   v1.Descriptor
	configData json.RawMessage
	platform   v1.Platform
}

func newLiveBusyBoxStorage(t *testing.T, ctx context.Context, root string, options StoreOptions) liveBaseFixture {
	t.Helper()
	fixture := liveBusyBoxImage(t, ctx)
	policy := filepath.Join(root, "fixture-policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	importTestImageToNative(t, ctx, options, fixture.layout, liveBusyBoxReference, fixture.manifest, &types.SystemContext{
		SignaturePolicyPath: policy, BigFilesTemporaryDir: root,
	})
	return liveBaseFixture{reference: liveBusyBoxReference, graphRoot: options.GraphRoot}
}

func importTestImageToNative(
	t *testing.T,
	ctx context.Context,
	options StoreOptions,
	layout, reference string,
	manifest v1.Descriptor,
	system *types.SystemContext,
) string {
	t.Helper()
	lease, err := acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	imageID, importErr := ImportSelectedImage(ctx, lease.store, system, layout, manifest)
	if importErr != nil {
		_ = lease.Close()
		t.Fatal(importErr)
	}
	if err := lease.store.AddNames(imageID, []string{reference}); err != nil {
		_ = lease.Close()
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	return imageID
}

func newLiveBusyBoxRegistry(t *testing.T, ctx context.Context) (string, string) {
	t.Helper()
	fixture := liveBusyBoxImage(t, ctx)
	server := httptest.NewServer(registryserver.New())
	t.Cleanup(server.Close)
	authority := strings.TrimPrefix(server.URL, "http://")
	repository, err := remote.NewRepository(authority + "/coopr/busybox")
	if err != nil {
		t.Fatal(err)
	}
	repository.PlainHTTP = true
	successors, err := content.Successors(ctx, fixture.store, fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, desc := range successors {
		stream, err := fixture.store.Fetch(ctx, desc)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.Push(ctx, desc, stream); err != nil {
			t.Fatal(err)
		}
	}
	stream, err := fixture.store.Fetch(ctx, fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.PushReference(ctx, fixture.manifest, stream, "latest"); err != nil {
		t.Fatal(err)
	}
	return authority + "/coopr/busybox@" + fixture.manifest.Digest.String(), authority
}

func liveBusyBoxImage(t *testing.T, ctx context.Context) liveBusyBoxGraph {
	t.Helper()
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatal("live Buildah tests require the Nix dev shell's static busybox binary")
	}
	binary, err := os.ReadFile(busybox)
	if err != nil {
		t.Fatal(err)
	}
	layerData := liveBusyBoxLayer(t, binary)
	layer := descriptor(v1.MediaTypeImageLayer, layerData)
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	configData, err := json.Marshal(v1.Image{
		Platform: platform,
		Config: v1.ImageConfig{
			Env: []string{"PATH=/bin"},
			Cmd: []string{"/bin/sh"},
		},
		RootFS:  v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{digest.FromBytes(layerData)}},
		History: []v1.History{{CreatedBy: "coopr test fixture: static busybox"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := descriptor(v1.MediaTypeImageConfig, configData)
	manifestData, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    config,
		Layers:    []v1.Descriptor{layer},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := descriptor(v1.MediaTypeImageManifest, manifestData)
	layout := t.TempDir()
	store, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	for _, blob := range []struct {
		desc v1.Descriptor
		data []byte
	}{{layer, layerData}, {config, configData}, {manifest, manifestData}} {
		if err := store.Push(ctx, blob.desc, bytes.NewReader(blob.data)); err != nil {
			t.Fatal(err)
		}
	}
	return liveBusyBoxGraph{store: store, layout: layout, manifest: manifest, configData: configData, platform: platform}
}

func liveBusyBoxLayer(t *testing.T, binary []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	w := tar.NewWriter(&buffer)
	for _, directory := range []string{"bin", "tmp"} {
		if err := w.WriteHeader(&tar.Header{Name: directory, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.WriteHeader(&tar.Header{Name: "bin/busybox", Mode: 0o755, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(binary); err != nil {
		t.Fatal(err)
	}
	for _, applet := range []string{"[", "ash", "cat", "cp", "od", "seq", "sh", "sleep", "test", "touch", "tr", "true"} {
		if err := w.WriteHeader(&tar.Header{
			Name:     filepath.ToSlash(filepath.Join("bin", applet)),
			Typeflag: tar.TypeSymlink,
			Linkname: "busybox",
			Mode:     0o777,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestLiveBusyBoxRegistryFixtureResolvesWithoutExternalRegistry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the live registry fixture in short mode")
	}
	ctx := context.Background()
	reference, authority := newLiveBusyBoxRegistry(t, ctx)
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(ctx, reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, oci.Image)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(reference, "@"+resolved.Selected.Digest.String()) {
		t.Fatalf("resolved digest = %s, reference = %s", resolved.Selected.Digest, reference)
	}
	if len(resolved.Layers) != 1 {
		t.Fatalf("fixture layers = %d, want 1", len(resolved.Layers))
	}
	if resolved.ConfigData == nil {
		t.Fatal("fixture resolution lost image config")
	}
	t.Logf("resolved hermetic fixture from loopback registry %s/coopr/busybox", authority)
}

func nativeFixtureSelection(ctx context.Context, options StoreOptions, reference string, platform v1.Platform) (oci.StoredSelection, bool, error) {
	lease, err := acquireStore(options)
	if err != nil {
		return oci.StoredSelection{}, false, err
	}
	defer func() { _ = lease.Close() }()
	resolved, err := oci.ResolveStoredImage(ctx, lease.store, reference, platform)
	if errors.Is(err, storage.ErrImageUnknown) {
		return oci.StoredSelection{}, false, nil
	}
	if err != nil {
		return oci.StoredSelection{}, false, err
	}
	return oci.StoredSelection{Root: resolved.Root, Manifest: resolved.Selected, SourceManifest: resolved.SourceManifest, ImageID: resolved.StorageImageID, ConfigData: resolved.ConfigData}, true, nil
}

func nameNativeFixture(ctx context.Context, options StoreOptions, name string, selected oci.StoredSelection) error {
	lease, err := acquireStore(options)
	if err != nil {
		return err
	}
	defer func() { _ = lease.Close() }()
	if err := verifyStoredManifest(ctx, lease.store, nil, selected); err != nil {
		return err
	}
	return lease.store.AddNames(selected.ImageID, []string{name})
}
