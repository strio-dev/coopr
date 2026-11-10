package imagestore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/strio-dev/coopr/internal/imagestore"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/testutil/nativeimage"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage/manifests"
	imagecopy "go.podman.io/image/v5/copy"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/homedir"
	"go.podman.io/storage/pkg/unshare"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func testVFSStore(t *testing.T) *nativeimage.Store {
	t.Helper()
	root := t.TempDir()
	backend, err := storage.GetStore(storage.StoreOptions{
		GraphDriverName: "vfs",
		GraphRoot:       filepath.Join(root, "graph"),
		RunRoot:         filepath.Join(root, "run"),
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := &nativeimage.Store{Store: imagestore.FromStore(backend), Backend: backend, System: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root}}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close test store: %v", err)
		}
	})
	return s
}

func TestNormalizeTag(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"", ""},
		{"app", "localhost/app:latest"},
		{"app:dev", "localhost/app:dev"},
		{"team/app", "localhost/team/app:latest"},
		{"localhost/app", "localhost/app:latest"},
		{"example.com:5000/team/app:v1", "example.com:5000/team/app:v1"},
	} {
		got, err := imagestore.NormalizeTag(tc.input)
		if err != nil || got != tc.want {
			t.Errorf("NormalizeTag(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
	for _, tag := range []string{"-foo:bar", "bad tag", "app:dev\n--privileged", "app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "app:dev@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "docker://example.com/app:dev"} {
		if _, err := imagestore.NormalizeTag(tag); err == nil {
			t.Errorf("accepted %q", tag)
		}
	}
}

func TestWriteLayoutToVFSStorage(t *testing.T) {
	store := testVFSStore(t)
	layout, root, imageID := testLayout(t, "layout")
	name, err := store.SeedLayout(context.Background(), layout, root, "layout-app:dev")
	if err != nil || name != "localhost/layout-app:dev" {
		t.Fatalf("WriteLayout = %q, %v", name, err)
	}
	image, err := store.Backend.Image(imageID.Encoded())
	if err != nil || image.ID != imageID.Encoded() || len(image.Names) != 1 || image.Names[0] != name {
		t.Fatalf("layout image: %+v, %v", image, err)
	}
}

func TestResolveStoredImageByPodmanName(t *testing.T) {
	store := testVFSStore(t)
	layout, root, imageID := testLayout(t, "native")
	if _, err := store.SeedLayout(context.Background(), layout, root, "native-base:dev"); err != nil {
		t.Fatal(err)
	}
	resolved, err := oci.ResolveStoredImage(context.Background(), store.Backend, "native-base:dev", v1.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.StorageImageID != imageID.Encoded() || resolved.Selected.Digest != root.Digest || resolved.Config.Digest != imageID {
		t.Fatalf("stored resolution = %+v", resolved)
	}
	if _, err := store.Tag(imageID.Encoded(), "renamed:latest"); err != nil {
		t.Fatal(err)
	}
	if err := store.Backend.RemoveNames(imageID.Encoded(), []string{"localhost/native-base:dev"}); err != nil {
		t.Fatalf("remove old name: %v", err)
	}
	if _, err := oci.ResolveStoredImage(context.Background(), store.Backend, "native-base:dev", v1.Platform{OS: "linux", Architecture: "amd64"}); !errors.Is(err, storage.ErrImageUnknown) {
		t.Fatalf("removed name resolved: %v", err)
	}
	if _, err := oci.ResolveStoredImage(context.Background(), store.Backend, "renamed:latest", v1.Platform{OS: "linux", Architecture: "amd64"}); err != nil {
		t.Fatalf("renamed image did not resolve: %v", err)
	}
}

func TestStoreCanReopenAfterCloseInOneProcess(t *testing.T) {
	root := t.TempDir()
	options := storage.StoreOptions{GraphDriverName: "vfs", GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run")}
	layout, descriptor, imageID := testLayout(t, "reopen")
	first, err := nativeimage.NewStore(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.SeedLayout(context.Background(), layout, descriptor, "reopen"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := nativeimage.NewStore(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if resolved, err := oci.ResolveStoredImage(context.Background(), second.Backend, "reopen", v1.Platform{OS: "linux", Architecture: "amd64"}); err != nil || resolved.StorageImageID != imageID.Encoded() {
		t.Fatalf("resolve after reopen = %+v, %v", resolved, err)
	}
	if _, err := second.Tag(imageID.Encoded(), "reopened"); err != nil {
		t.Fatal(err)
	}
}

func TestShortNativeNamePrefersLocalhostBeforeDockerHub(t *testing.T) {
	store := testVFSStore(t)
	localLayout, localRoot, localID := testLayout(t, "local")
	dockerLayout, dockerRoot, dockerID := testLayout(t, "docker")
	if _, err := store.SeedLayout(context.Background(), localLayout, localRoot, "localhost/collision:latest"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SeedLayout(context.Background(), dockerLayout, dockerRoot, "docker.io/library/collision:latest"); err != nil {
		t.Fatal(err)
	}
	matched, err := store.MatchedName("collision")
	if err != nil || matched != "localhost/collision:latest" {
		t.Fatalf("short name matched %q, %v", matched, err)
	}
	resolved, err := oci.ResolveStoredImage(context.Background(), store.Backend, "collision", v1.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil || resolved.StorageImageID != localID.Encoded() {
		t.Fatalf("short name resolved %+v, %v; local=%s docker=%s", resolved, err, localID, dockerID)
	}
	if err := store.Backend.RemoveNames(localID.Encoded(), []string{matched}); err != nil {
		t.Fatalf("remove preferred local name: %v", err)
	}
	matched, err = store.MatchedName("collision")
	if err != nil || matched != "docker.io/library/collision:latest" {
		t.Fatalf("short name fallback matched %q, %v", matched, err)
	}
}

func testLayout(t *testing.T, label string) (string, v1.Descriptor, digest.Digest) {
	return testLayoutPlatform(t, label, v1.Platform{OS: "linux", Architecture: "amd64"})
}

func testLayoutPlatform(t *testing.T, label string, platform v1.Platform) (string, v1.Descriptor, digest.Digest) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := orasoci.NewWithContext(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(v1.Image{
		Platform: platform,
		RootFS:   v1.RootFS{Type: "layers"},
		Config:   v1.ImageConfig{Labels: map[string]string{"test": label}},
	})
	if err != nil {
		t.Fatal(err)
	}
	imageID := digest.FromBytes(config)
	configDesc := v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: imageID, Size: int64(len(config))}
	if err := store.Push(ctx, configDesc, bytes.NewReader(config)); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    configDesc,
	})
	if err != nil {
		t.Fatal(err)
	}
	root := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromBytes(manifest), Size: int64(len(manifest))}
	if err := store.Push(ctx, root, bytes.NewReader(manifest)); err != nil {
		t.Fatal(err)
	}
	if err := store.Tag(ctx, root, root.Digest.String()); err != nil {
		t.Fatal(err)
	}
	return dir, root, imageID
}

func TestWriteIndexLayoutResolvesEveryStoredPlatform(t *testing.T) {
	ctx := context.Background()
	store := testVFSStore(t)
	amd := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm := v1.Platform{OS: "linux", Architecture: "arm64"}
	amdLayout, amdManifest, amdID := testLayoutPlatform(t, "amd64", amd)
	armLayout, armManifest, armID := testLayoutPlatform(t, "arm64", arm)
	indexLayout := filepath.Join(t.TempDir(), "index")
	rootFixtureVariants := []oci.ImageVariant{
		{Layout: amdLayout, Manifest: amdManifest, Platform: amd},
		{Layout: armLayout, Manifest: armManifest, Platform: arm},
	}
	root, rootFixtureData, err := oci.ImageIndexDescriptor(rootFixtureVariants, "oci")
	if err != nil {
		t.Fatal(err)
	}
	writeStoredIndexFixture(t, ctx, indexLayout, root, rootFixtureData, rootFixtureVariants)
	if _, err := store.SeedIndex(ctx, indexLayout, root, "multi:latest"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		platform v1.Platform
		id       digest.Digest
		manifest v1.Descriptor
	}{{amd, amdID, amdManifest}, {arm, armID, armManifest}} {
		resolved, err := oci.ResolveStoredImage(ctx, store.Backend, "multi:latest", test.platform)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Root.Digest != root.Digest || resolved.Selected.Digest != test.manifest.Digest || resolved.StorageImageID != test.id.Encoded() {
			t.Fatalf("platform %s resolve = %+v", test.platform.Architecture, resolved)
		}
		if resolved.Config.Digest != test.id {
			t.Fatalf("platform %s config = %s, want %s", test.platform.Architecture, resolved.Config.Digest, test.id)
		}
	}
}

func TestWriteIndexLayoutRetagReusesNativeList(t *testing.T) {
	ctx := context.Background()
	store := testVFSStore(t)
	platform := v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8", OSVersion: "test-version", OSFeatures: []string{"test-feature"}}
	childLayout, child, _ := testLayoutPlatform(t, "retag", platform)
	indexLayout := filepath.Join(t.TempDir(), "index")
	rootFixtureVariants := []oci.ImageVariant{{Layout: childLayout, Manifest: child, Platform: platform}}
	root, rootFixtureData, err := oci.ImageIndexDescriptor(rootFixtureVariants, "oci")
	if err != nil {
		t.Fatal(err)
	}
	writeStoredIndexFixture(t, ctx, indexLayout, root, rootFixtureData, rootFixtureVariants)
	if _, err := store.SeedIndex(ctx, indexLayout, root, "first:latest"); err != nil {
		t.Fatal(err)
	}
	first, err := store.Backend.Image("localhost/first:latest")
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Backend.Images()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SeedIndex(ctx, indexLayout, root, "second:latest"); err != nil {
		t.Fatal(err)
	}
	second, err := store.Backend.Image("localhost/second:latest")
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.Backend.Images()
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || len(before) != len(after) {
		t.Fatalf("retag duplicated native list: first=%s second=%s, image counts %d -> %d", first.ID, second.ID, len(before), len(after))
	}
}

func TestWriteUntaggedIndexRemainsNativeAfterReopen(t *testing.T) {
	ctx := context.Background()
	store := testVFSStore(t)
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	layout, manifest, imageID := testLayoutPlatform(t, "unnamed", platform)
	if _, err := store.SeedLayout(ctx, layout, manifest, ""); err != nil {
		t.Fatal(err)
	}
	root, data, err := oci.ImageIndexDescriptor([]oci.ImageVariant{{Layout: layout, Manifest: manifest, Platform: platform}}, "oci")
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.WriteStoredIndex(ctx, root, data, map[digest.Digest]string{manifest.Digest: imageID.Encoded()}, "")
	if err != nil {
		t.Fatal(err)
	}
	options := storage.StoreOptions{GraphRoot: store.Backend.GraphRoot(), RunRoot: store.Backend.RunRoot(), GraphDriverName: "vfs"}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = nativeimage.NewStore(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	gotRoot, gotData, found, err := oci.StoredImageIndex(ctx, store.Backend, id)
	if err != nil || !found || gotRoot.Digest != root.Digest || !bytes.Equal(gotData, data) {
		t.Fatalf("reopened unnamed index: root=%+v found=%t err=%v", gotRoot, found, err)
	}
	resolved, err := oci.ResolveStoredImage(ctx, store.Backend, id, platform)
	if err != nil || resolved.StorageImageID != imageID.Encoded() {
		t.Fatalf("reopened index child: resolved=%+v err=%v", resolved, err)
	}
}

func TestTagSelectedPreservesExistingNativeAliases(t *testing.T) {
	ctx := context.Background()
	store := testVFSStore(t)
	layout, original, imageID := testLayoutPlatform(t, "alias-preservation", v1.Platform{OS: "linux", Architecture: "amd64"})
	if _, err := store.SeedLayout(ctx, layout, original, ""); err != nil {
		t.Fatal(err)
	}
	// These names are native Podman-style aliases without Coopr origin metadata.
	if err := store.Backend.AddNames(imageID.Encoded(), []string{"localhost/podman-created:latest", "localhost/another:latest", "localhost/destination:latest"}); err != nil {
		t.Fatal(err)
	}
	data, err := store.Backend.ImageBigData(imageID.Encoded(), storage.ImageDigestBigDataKey)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Annotations = map[string]string{"variant": "alternate"}
	alternateData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	alternate := oci.Descriptor(v1.MediaTypeImageManifest, alternateData)
	if err := store.Backend.SetImageBigData(imageID.Encoded(), storage.ImageDigestManifestBigDataNamePrefix+"-"+alternate.Digest.String(), alternateData, func([]byte) (digest.Digest, error) { return alternate.Digest, nil }); err != nil {
		t.Fatal(err)
	}
	originalSignature := []byte("\x00sigstore-json\n" + `{"mimeType":"application/vnd.dev.cosign.simplesigning.v1+json","payload":"b3JpZ2luYWw=","annotations":{"variant":"original"}}`)
	alternateSignature := []byte("\x00sigstore-json\n" + `{"mimeType":"application/vnd.dev.cosign.simplesigning.v1+json","payload":"YWx0ZXJuYXRl","annotations":{"variant":"alternate"}}`)
	metadata, err := json.Marshal(map[string]any{"signature-sizes": []int{len(originalSignature)}, "signatures-sizes": map[digest.Digest][]int{alternate.Digest: {len(alternateSignature)}}, "custom": "preserve"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Backend.SetMetadata(imageID.Encoded(), string(metadata)); err != nil {
		t.Fatal(err)
	}
	for key, blob := range map[string][]byte{"signatures": originalSignature, "signature-" + alternate.Digest.Encoded(): alternateSignature} {
		if err := store.Backend.SetImageBigData(imageID.Encoded(), key, blob, nil); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err := store.TagSelected(ctx, imageID.Encoded(), alternate, "destination:latest"); err != nil {
			t.Fatal(err)
		}
	}
	destination, err := store.Backend.Image("localhost/destination:latest")
	if err != nil {
		t.Fatal(err)
	}
	additionalSignature := []byte("\x00sigstore-json\n" + `{"mimeType":"application/vnd.dev.cosign.simplesigning.v1+json","payload":"ZXh0cmE=","annotations":{"variant":"additional"}}`)
	state, err := oci.CaptureStoredSignatures(store.Backend, destination.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := oci.WriteStoredManifestSignatures(store.Backend, destination.ID, alternate.Digest, true, [][]byte{alternateSignature, additionalSignature}, state); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := store.TagSelected(ctx, imageID.Encoded(), alternate, "second-destination:latest"); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]v1.Descriptor{
		"podman-created:latest":     original,
		"another:latest":            original,
		"destination:latest":        alternate,
		"second-destination:latest": alternate,
	} {
		resolved, err := oci.ResolveStoredImage(ctx, store.Backend, name, v1.Platform{OS: "linux", Architecture: "amd64"})
		if err != nil || resolved.Selected.Digest != want.Digest {
			t.Fatalf("Coopr alias %s = %+v, %v; want %s", name, resolved, err, want.Digest)
		}
		image, err := store.Backend.Image("localhost/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.StorageImageID != image.ID {
			t.Fatalf("resolved storage record = %s, want %s", resolved.StorageImageID, image.ID)
		}
		state, err := oci.CaptureStoredSignatures(store.Backend, image.ID)
		if err != nil {
			t.Fatal(err)
		}
		signatures, err := oci.StoredSignatureBlobs(state, want.Digest, true)
		wantSignature := originalSignature
		wantCount := 1
		if want.Digest == alternate.Digest {
			wantSignature = alternateSignature
			wantCount = 2
		}
		if err != nil || len(signatures) != wantCount || !bytes.Equal(signatures[0], wantSignature) {
			t.Fatalf("alias %s signature = %q, %v; want %q", name, signatures, err, wantSignature)
		}
		if wantCount == 2 && !bytes.Equal(signatures[1], additionalSignature) {
			t.Fatalf("retag removed additional signature from %s", name)
		}
		ref, err := imagestorage.Transport.NewStoreReference(store.Backend, nil, image.ID)
		if err != nil {
			t.Fatal(err)
		}
		source, err := ref.NewImageSource(ctx, store.System)
		if err != nil {
			t.Fatal(err)
		}
		data, _, err := source.GetManifest(ctx, nil)
		closeErr := source.Close()
		if err != nil || closeErr != nil || digest.FromBytes(data) != want.Digest {
			t.Fatalf("native alias %s = %s, %v/%v; want %s", name, digest.FromBytes(data), err, closeErr, want.Digest)
		}
	}
	if _, err := store.Backend.DeleteImage(imageID.Encoded(), true); err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{"destination:latest", destination.ID, "sha256:" + destination.ID} {
		if resolved, err := oci.ResolveStoredImage(ctx, store.Backend, selector, v1.Platform{OS: "linux", Architecture: "amd64"}); err != nil || resolved.Selected.Digest != alternate.Digest {
			t.Fatalf("surviving alias %s after original removal = %+v, %v", selector, resolved, err)
		}
	}
}

func TestWriteStoredIndexPreservesExactChildrenAndRetags(t *testing.T) {
	const dockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	const dockerIndex = "application/vnd.docker.distribution.manifest.list.v2+json"
	for _, indexType := range []string{v1.MediaTypeImageIndex, dockerIndex} {
		t.Run(indexType, func(t *testing.T) {
			ctx := context.Background()
			store := testVFSStore(t)
			imageIDs := make(map[digest.Digest]string)
			siblingData := make(map[digest.Digest][]byte)
			children := make([]v1.Descriptor, 0, 2)
			for _, platform := range []v1.Platform{
				{OS: "linux", Architecture: "amd64", OSVersion: "test-amd", OSFeatures: []string{"feature-amd"}},
				{OS: "linux", Architecture: "arm64", Variant: "v8", OSVersion: "test-arm", OSFeatures: []string{"feature-arm"}},
			} {
				layoutPath, original, imageID := testLayoutPlatform(t, platform.Architecture, platform)
				layout, err := orasoci.NewWithContext(ctx, layoutPath)
				if err != nil {
					t.Fatal(err)
				}
				manifestPath := filepath.Join(layoutPath, "blobs", "sha256", original.Digest.Encoded())
				data, err := os.ReadFile(manifestPath)
				if err != nil {
					t.Fatal(err)
				}
				var manifest v1.Manifest
				if err := json.Unmarshal(data, &manifest); err != nil {
					t.Fatal(err)
				}
				manifest.MediaType = dockerManifest
				manifest.Config.MediaType = "application/vnd.docker.container.image.v1+json"
				dockerData, err := json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				docker := v1.Descriptor{MediaType: dockerManifest, Digest: digest.FromBytes(dockerData), Size: int64(len(dockerData))}
				if err := layout.Push(ctx, docker, bytes.NewReader(dockerData)); err != nil {
					t.Fatal(err)
				}
				if err := layout.Tag(ctx, docker, docker.Digest.String()); err != nil {
					t.Fatal(err)
				}
				// The selected child is deliberately not the record's last imported
				// default manifest. Both formats have the same configuration/image ID.
				selected, sibling := original, docker
				otherData := dockerData
				if indexType == dockerIndex {
					selected, sibling = docker, original
					otherData = data
				}
				siblingData[selected.Digest] = otherData
				for _, child := range []v1.Descriptor{selected, sibling} {
					if _, err := store.SeedLayout(ctx, layoutPath, child, ""); err != nil {
						t.Fatal(err)
					}
				}
				selected.Platform = &platform
				selected.Annotations = map[string]string{"test-child": platform.Architecture}
				children = append(children, selected)
				imageIDs[selected.Digest] = imageID.Encoded()
			}
			makeIndex := func(label string) (v1.Descriptor, []byte) {
				t.Helper()
				data, err := json.MarshalIndent(v1.Index{
					Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: indexType,
					Manifests: children, Annotations: map[string]string{"test-root": label},
				}, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				data = append(data, '\n')
				return v1.Descriptor{MediaType: indexType, Digest: digest.FromBytes(data), Size: int64(len(data))}, data
			}
			root, data := makeIndex("original")
			nativeID, err := store.WriteStoredIndex(ctx, root, data, imageIDs, "current:latest")
			if err != nil {
				t.Fatal(err)
			}
			first, err := store.Backend.Image("localhost/current:latest")
			if err != nil {
				t.Fatal(err)
			}
			if nativeID != first.ID {
				t.Fatalf("tagged index returned %s, native ID %s", nativeID, first.ID)
			}
			if err := store.Backend.SetImageBigData(first.ID, "test-preserved", []byte("existing metadata"), nil); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"alias:latest", "alias:latest", "another:latest"} {
				if got, err := store.WriteStoredIndex(ctx, root, data, imageIDs, name); err != nil || got != first.ID {
					t.Fatalf("reused index ID=%s err=%v want=%s", got, err, first.ID)
				}
				image, err := store.Backend.Image("localhost/" + name)
				if err != nil || image.ID != first.ID {
					t.Fatalf("retag %s = %+v, %v; want %s", name, image, err, first.ID)
				}
			}
			images, err := store.Backend.Images()
			if err != nil || len(images) != 3 {
				t.Fatalf("stored images = %d, %v; want two children plus one list", len(images), err)
			}
			metadata, err := store.Backend.ImageBigData(first.ID, "test-preserved")
			if err != nil || string(metadata) != "existing metadata" {
				t.Fatalf("retag changed metadata = %q, %v", metadata, err)
			}
			// Simulate another import after publication changing the shared
			// record's default manifest. Reopen the native list afterwards.
			for _, child := range children {
				id := imageIDs[child.Digest]
				selectedData, err := store.Backend.ImageBigData(id, storage.ImageDigestManifestBigDataNamePrefix+"-"+child.Digest.String())
				if err != nil {
					t.Fatal(err)
				}
				for _, data := range [][]byte{selectedData, siblingData[child.Digest]} {
					if err := store.Backend.SetImageBigData(id, storage.ImageDigestManifestBigDataNamePrefix, data, func(data []byte) (digest.Digest, error) { return digest.FromBytes(data), nil }); err != nil {
						t.Fatal(err)
					}
				}
			}
			id, list, err := manifests.LoadFromImage(store.Backend, first.ID)
			if err != nil || id != first.ID {
				t.Fatalf("load native list = %s, %v", id, err)
			}
			ref, err := list.Reference(store.Backend, imagecopy.CopyAllImages, nil)
			if err != nil {
				t.Fatal(err)
			}
			source, err := ref.NewImageSource(ctx, store.System)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = source.Close() }()
			for _, child := range children {
				data, mediaType, err := source.GetManifest(ctx, &child.Digest)
				if err != nil || digest.FromBytes(data) != child.Digest || int64(len(data)) != child.Size || mediaType != child.MediaType {
					t.Fatalf("native child %s = %s/%s/%d, %v", child.Digest, digest.FromBytes(data), mediaType, len(data), err)
				}
			}
			for _, child := range children {
				resolved, err := oci.ResolveStoredImage(ctx, store.Backend, "alias:latest", *child.Platform)
				if err != nil || resolved.Selected.Digest != child.Digest || resolved.StorageImageID != imageIDs[child.Digest] {
					t.Fatalf("native selected child %s = %+v, %v", child.Digest, resolved, err)
				}
			}
			updated, updatedData := makeIndex("updated")
			// Failure after reading the first child must preserve the prior tag
			// and avoid leaving a partially created list record.
			childBefore, err := store.Backend.Image(imageIDs[children[0].Digest])
			if err != nil {
				t.Fatal(err)
			}
			missing := map[digest.Digest]string{children[0].Digest: imageIDs[children[0].Digest]}
			if _, err := store.WriteStoredIndex(ctx, updated, updatedData, missing, "current:latest"); err == nil {
				t.Fatal("missing child unexpectedly tagged")
			}
			if _, err := store.WriteStoredIndex(ctx, updated, updatedData, missing, "failed:latest"); err == nil {
				t.Fatal("missing child unexpectedly published a new tag")
			}
			childAfter, err := store.Backend.Image(imageIDs[children[0].Digest])
			if err != nil || !slices.Equal(childBefore.Names, childAfter.Names) {
				t.Fatalf("failed index retained child names: before=%v after=%v, %v", childBefore.Names, childAfter.Names, err)
			}
			if _, err := store.Backend.Image("localhost/failed:latest"); !errors.Is(err, storage.ErrImageUnknown) {
				t.Fatalf("failed native tag lookup = %v", err)
			}
			current, err := store.Backend.Image("localhost/current:latest")
			if err != nil || current.ID != first.ID {
				t.Fatalf("failed update retargeted tag: %+v, %v", current, err)
			}
			images, err = store.Backend.Images()
			if err != nil || len(images) != 3 {
				t.Fatalf("failed update left images = %d, %v", len(images), err)
			}
			if _, err := store.WriteStoredIndex(ctx, updated, updatedData, imageIDs, "current:latest"); err != nil {
				t.Fatal(err)
			}
			for _, expected := range []struct {
				name string
				data []byte
			}{{"localhost/alias:latest", data}, {"localhost/current:latest", updatedData}} {
				image, err := store.Backend.Image(expected.name)
				if err != nil {
					t.Fatal(err)
				}
				stored, err := store.Backend.ImageBigData(image.ID, storage.ImageDigestManifestBigDataNamePrefix)
				if err != nil || !bytes.Equal(stored, expected.data) {
					t.Fatalf("native index %s changed exact bytes: %v", expected.name, err)
				}
			}
		})
	}
}

func TestWriteIndexLayoutAcceptsOmittedMediaType(t *testing.T) {
	ctx := context.Background()
	store := testVFSStore(t)
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	layoutPath, child, _ := testLayoutPlatform(t, "omitted-media-type", platform)
	child.Platform = &platform
	data, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []v1.Descriptor{child}})
	if err != nil {
		t.Fatal(err)
	}
	root := v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: digest.FromBytes(data), Size: int64(len(data))}
	layout, err := orasoci.NewWithContext(ctx, layoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Push(ctx, root, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SeedIndex(ctx, layoutPath, root, "omitted:latest"); err != nil {
		t.Fatal(err)
	}
	image, err := store.Backend.Image("localhost/omitted:latest")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Backend.ImageBigData(image.ID, storage.ImageDigestManifestBigDataNamePrefix)
	if err != nil || !bytes.Equal(stored, data) {
		t.Fatalf("index with omitted mediaType changed bytes: %v", err)
	}
}

// pausedIndexNameStore makes a failing writer pause after creating the first
// canonical child name, while a second writer targets the same repository.
type pausedIndexNameStore struct {
	storage.Store
	once   sync.Once
	added  chan struct{}
	resume chan struct{}
}

func (s *pausedIndexNameStore) AddNames(imageID string, names []string) error {
	err := s.Store.AddNames(imageID, names)
	if err == nil && len(names) == 1 && strings.HasPrefix(names[0], "localhost/race@") {
		pause := false
		s.once.Do(func() { pause = true })
		if pause {
			close(s.added)
			<-s.resume
		}
	}
	return err
}

func TestWriteStoredIndexFailedWriterPreservesConcurrentIndexNames(t *testing.T) {
	ctx := context.Background()
	store := testVFSStore(t)
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	layout, child, imageID := testLayoutPlatform(t, "concurrent-child", platform)
	if _, err := store.SeedLayout(ctx, layout, child, ""); err != nil {
		t.Fatal(err)
	}
	child.Platform = &platform
	second := child
	second.Digest = digest.FromString("missing second child")
	second.Platform = &v1.Platform{OS: "linux", Architecture: "arm64"}
	makeIndex := func(children []v1.Descriptor) (v1.Descriptor, []byte) {
		t.Helper()
		data, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: children})
		if err != nil {
			t.Fatal(err)
		}
		return v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: digest.FromBytes(data), Size: int64(len(data))}, data
	}
	failedRoot, failedData := makeIndex([]v1.Descriptor{child, second})
	goodRoot, goodData := makeIndex([]v1.Descriptor{child})
	paused := &pausedIndexNameStore{Store: store.Backend, added: make(chan struct{}), resume: make(chan struct{})}
	store.Backend = paused
	store.Store = imagestore.FromStore(paused)
	failed := make(chan error, 1)
	good := make(chan error, 1)
	imageIDs := map[digest.Digest]string{child.Digest: imageID.Encoded()}
	go func() {
		_, err := store.WriteStoredIndex(ctx, failedRoot, failedData, imageIDs, "race:failed")
		failed <- err
	}()
	<-paused.added
	go func() {
		_, err := store.WriteStoredIndex(ctx, goodRoot, goodData, imageIDs, "race:good")
		good <- err
	}()
	// An unlocked writer can publish immediately and then lose its name when
	// the paused writer rolls back. A repository lock makes it wait instead.
	var goodErr error
	completed := false
	select {
	case goodErr = <-good:
		completed = true
	case <-time.After(time.Second):
	}
	close(paused.resume)
	if err := <-failed; err == nil {
		t.Fatal("incomplete index unexpectedly succeeded")
	}
	if !completed {
		goodErr = <-good
	}
	if goodErr != nil {
		t.Fatalf("concurrent good index: %v", goodErr)
	}
	resolved, err := oci.ResolveStoredImage(ctx, store.Backend, "race:good", platform)
	if err != nil || resolved.Root.Digest != goodRoot.Digest || resolved.Selected.Digest != child.Digest {
		t.Fatalf("failed writer broke concurrent native index: %+v, %v", resolved, err)
	}
	if _, err := store.Backend.Image("localhost/race:failed"); !errors.Is(err, storage.ErrImageUnknown) {
		t.Fatalf("failed native index name exists: %v", err)
	}
}

// pausedIndexManifestStore pauses a creator once its exact root is visible,
// before cancellation can trigger deletion of that tentative native record.
type pausedIndexManifestStore struct {
	storage.Store
	root   digest.Digest
	once   sync.Once
	saved  chan struct{}
	resume chan struct{}
}

func (s *pausedIndexManifestStore) SetImageBigData(imageID, key string, data []byte, digester func([]byte) (digest.Digest, error)) error {
	err := s.Store.SetImageBigData(imageID, key, data, digester)
	if err == nil && key == storage.ImageDigestManifestBigDataNamePrefix && digest.FromBytes(data) == s.root {
		pause := false
		s.once.Do(func() { pause = true })
		if pause {
			close(s.saved)
			<-s.resume
		}
	}
	return err
}

func TestWriteStoredIndexFailedCreatorPreservesCrossRepositoryRetag(t *testing.T) {
	ctx := context.Background()
	store := testVFSStore(t)
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	layout, child, imageID := testLayoutPlatform(t, "cross-repository-child", platform)
	if _, err := store.SeedLayout(ctx, layout, child, ""); err != nil {
		t.Fatal(err)
	}
	child.Platform = &platform
	data, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{child}})
	if err != nil {
		t.Fatal(err)
	}
	root := v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: digest.FromBytes(data), Size: int64(len(data))}
	paused := &pausedIndexManifestStore{Store: store.Backend, root: root.Digest, saved: make(chan struct{}), resume: make(chan struct{})}
	store.Backend = paused
	store.Store = imagestore.FromStore(paused)
	failedCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	failed := make(chan error, 1)
	good := make(chan error, 1)
	imageIDs := map[digest.Digest]string{child.Digest: imageID.Encoded()}
	go func() {
		_, err := store.WriteStoredIndex(failedCtx, root, data, imageIDs, "creator:failed")
		failed <- err
	}()
	<-paused.saved
	go func() {
		_, err := store.WriteStoredIndex(ctx, root, data, imageIDs, "reuser:good")
		good <- err
	}()
	var goodErr error
	completed := false
	select {
	case goodErr = <-good:
		completed = true
	case <-time.After(time.Second):
	}
	cancel()
	close(paused.resume)
	if err := <-failed; !errors.Is(err, context.Canceled) {
		t.Fatalf("creator error = %v, want canceled", err)
	}
	if !completed {
		goodErr = <-good
	}
	if goodErr != nil {
		t.Fatalf("cross-repository index = %v", goodErr)
	}
	resolved, err := oci.ResolveStoredImage(ctx, store.Backend, "reuser:good", platform)
	if err != nil || resolved.Root.Digest != root.Digest || resolved.Selected.Digest != child.Digest {
		t.Fatalf("creator rollback deleted successful retag: %+v, %v", resolved, err)
	}
	images, err := store.Backend.Images()
	if err != nil || len(images) != 2 {
		t.Fatalf("native records after failed creator/successful retag = %d, %v", len(images), err)
	}
	childImage, err := store.Backend.Image(imageID.Encoded())
	if err != nil || slices.Contains(childImage.Names, "localhost/creator@"+child.Digest.String()) {
		t.Fatalf("failed creator retained child names: %+v, %v", childImage, err)
	}
}

func TestWriteStoredIndexAcceptsEmptyIndex(t *testing.T) {
	store := testVFSStore(t)
	data := []byte(`{"schemaVersion":2,"manifests":[]}`)
	root := v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: digest.FromBytes(data), Size: int64(len(data))}
	if _, err := store.WriteStoredIndex(context.Background(), root, data, nil, "empty:latest"); err != nil {
		t.Fatal(err)
	}
	image, err := store.Backend.Image("localhost/empty:latest")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Backend.ImageBigData(image.ID, storage.ImageDigestManifestBigDataNamePrefix)
	if err != nil || !bytes.Equal(stored, data) {
		t.Fatalf("empty native index changed exact bytes: %v", err)
	}
}

func TestDirectDigestLookupWithMutableAliases(t *testing.T) {
	ctx := context.Background()
	store := testVFSStore(t)
	layout, manifest, imageID := testLayout(t, "visible-names")
	if _, err := store.SeedLayout(ctx, layout, manifest, "visible:dev"); err != nil {
		t.Fatal(err)
	}
	canonical := "localhost/visible@" + manifest.Digest.String()
	if err := store.Backend.AddNames(imageID.Encoded(), []string{canonical, "localhost/name-only", "localhost/second:latest"}); err != nil {
		t.Fatal(err)
	}
	matched, err := store.MatchedName(canonical)
	if err != nil || matched != canonical {
		t.Fatalf("direct digest name = %q, %v", matched, err)
	}
	resolved, err := oci.ResolveStoredImage(ctx, store.Backend, canonical, v1.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil || resolved.Selected.Digest != manifest.Digest || resolved.StorageImageID != imageID.Encoded() {
		t.Fatalf("direct digest resolve = %+v, %v", resolved, err)
	}
}

func TestDefaultStoreOptionsHonorsNativeConfiguration(t *testing.T) {
	config := filepath.Join(t.TempDir(), "storage.conf")
	// Parse only: no store is opened, and these paths need not exist.
	data := `[storage]
driver = "vfs"
graphroot = "/var/lib/containers/storage"
runroot = "/run/containers/storage"
[storage.options]
additionalimagestores = ["/usr/lib/containers/storage"]
`
	if err := os.WriteFile(config, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_STORAGE_CONF", config)
	t.Setenv("STORAGE_OPTS", "")
	opts, err := imagestore.DefaultStoreOptions()
	if err != nil {
		t.Fatal(err)
	}
	if opts.GraphRoot != "/var/lib/containers/storage" || opts.RunRoot != "/run/containers/storage" {
		t.Fatalf("configured native paths overridden: %+v", opts)
	}
	// STORAGE_OPTS overrides the configured list, exactly as native parsing does.
	t.Setenv("STORAGE_OPTS", "imagestore=/usr/lib/containers/storage")
	opts, err = imagestore.DefaultStoreOptions()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(opts.GraphDriverOptions, "imagestore=/usr/lib/containers/storage") {
		t.Fatalf("configured image store removed: %v", opts.GraphDriverOptions)
	}
}

func TestDefaultStoreOptionsUsesNativeDefaults(t *testing.T) {
	config := filepath.Join(t.TempDir(), "storage.conf")
	t.Setenv("CONTAINERS_STORAGE_CONF", config)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("STORAGE_OPTS", "")
	wantGraph, wantRun := "/var/lib/containers/storage", "/run/containers/storage"
	if unshare.IsRootless() && unshare.GetRootlessUID() != 0 {
		data, err := homedir.GetDataHome()
		if err != nil {
			t.Fatal(err)
		}
		// Native GetRuntimeDir caches its first resolution. Use that public value,
		// rather than changing XDG_RUNTIME_DIR after another test initialized it.
		runtime, err := homedir.GetRuntimeDir()
		if err != nil {
			t.Fatal(err)
		}
		wantGraph = filepath.Join(data, "containers", "storage")
		wantRun = filepath.Join(runtime, "containers")
	}
	for _, tc := range []struct{ name, paths string }{
		{"omitted", ""},
		{"empty", "graphroot = \"\"\nrunroot = \"\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(config, []byte("[storage]\ndriver = \"vfs\"\n"+tc.paths), 0600); err != nil {
				t.Fatal(err)
			}
			options, err := imagestore.DefaultStoreOptions()
			if err != nil {
				t.Fatal(err)
			}
			if options.GraphRoot != wantGraph || options.RunRoot != wantRun {
				t.Fatalf("native defaults = graph %q run %q, want %q/%q", options.GraphRoot, options.RunRoot, wantGraph, wantRun)
			}
		})
	}
}

func writeStoredIndexFixture(t *testing.T, ctx context.Context, path string, root v1.Descriptor, data []byte, variants []oci.ImageVariant) {
	t.Helper()
	target, err := orasoci.NewWithContext(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := orasoci.NewStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range variants {
		source, err := orasoci.NewWithContext(ctx, variant.Layout)
		if err != nil {
			t.Fatal(err)
		}
		if err := oras.CopyGraph(ctx, source, blobs, variant.Manifest, oras.CopyGraphOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := target.Push(ctx, root, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := target.Tag(ctx, root, root.Digest.String()); err != nil {
		t.Fatal(err)
	}
}
