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
	"testing"
	"time"

	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
)

const dockerLayerMediaType = "application/vnd.docker.image.rootfs.diff.tar"

func TestExportRawDockerSourcePreservesBytesAndMediaTypes(t *testing.T) {
	ctx := context.Background()
	fixture := newRawDockerFixture(t)
	output := Output{Path: filepath.Join(t.TempDir(), "layout"), Reference: "raw-docker"}
	source := &rawDockerSource{manifest: fixture.manifestData, blobs: fixture.blobs}

	result, err := exportRawDockerSource(ctx, source, "image-id", output)
	if err != nil {
		t.Fatal(err)
	}
	if result.ManifestDigest != fixture.manifest.Digest.String() {
		t.Fatalf("manifest digest = %q, want %q", result.ManifestDigest, fixture.manifest.Digest)
	}
	if source.blobsClosed != len(fixture.blobs) {
		t.Fatalf("closed blobs = %d, want %d", source.blobsClosed, len(fixture.blobs))
	}

	index := readRawDockerIndex(t, output.Path)
	if len(index.Manifests) != 1 {
		t.Fatalf("layout roots = %d, want 1", len(index.Manifests))
	}
	root := index.Manifests[0]
	if root.MediaType != dockerManifestMediaType {
		t.Fatalf("root media type = %q", root.MediaType)
	}
	if root.Annotations[v1.AnnotationRefName] != output.Reference {
		t.Fatalf("root reference = %q", root.Annotations[v1.AnnotationRefName])
	}
	if got := readRawDockerBlob(t, output.Path, root); !bytes.Equal(got, fixture.manifestData) {
		t.Fatal("export changed Docker manifest bytes")
	}
	if got := readRawDockerBlob(t, output.Path, fixture.config); !bytes.Equal(got, fixture.configData) {
		t.Fatal("export changed Docker config bytes")
	}
}

func TestExportRawDockerSourceRejectsChangedBlobAndCleansStaging(t *testing.T) {
	fixture := newRawDockerFixture(t)
	fixture.blobs[fixture.config.Digest] = []byte("changed")
	root := t.TempDir()
	output := Output{Path: filepath.Join(root, "layout")}

	_, err := exportRawDockerSource(context.Background(), &rawDockerSource{
		manifest: fixture.manifestData, blobs: fixture.blobs,
	}, "image-id", output)
	if err == nil {
		t.Fatal("export accepted a blob that differs from its descriptor")
	}
	if _, statErr := os.Lstat(output.Path); !os.IsNotExist(statErr) {
		t.Fatalf("failed output remains: %v", statErr)
	}
	matches, globErr := filepath.Glob(filepath.Join(root, ".coopr-docker-layout-*"))
	if globErr != nil || len(matches) != 0 {
		t.Fatalf("staged layouts remain: %v, %v", matches, globErr)
	}
}

func TestExportStoredImageRawHealthcheckSurvivesNativeImport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live rootless Docker image export in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	fixture := newRawDockerFixture(t)
	firstLayout := filepath.Join(root, "source")
	if _, err := exportRawDockerSource(ctx, &rawDockerSource{
		manifest: fixture.manifestData, blobs: fixture.blobs,
	}, "fixture", Output{Path: firstLayout}); err != nil {
		t.Fatal(err)
	}

	options := StoreOptions{
		RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
	}
	lease, err := acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	})
	imageID, err := ImportSelectedImage(ctx, lease.store, &types.SystemContext{BigFilesTemporaryDir: root}, firstLayout, fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	if err := nameNativeFixture(ctx, options, "fixture.local/health:latest", oci.StoredSelection{
		Root: fixture.manifest, Manifest: fixture.manifest, ImageID: imageID, ConfigData: fixture.configData,
	}); err != nil {
		t.Fatal(err)
	}
	selection, found, err := nativeFixtureSelection(ctx, options, "fixture.local/health:latest", platform)
	if err != nil || !found || selection.ImageID != imageID {
		t.Fatalf("native lookup = (%+v, %v, %v)", selection, found, err)
	}

	secondLayout := filepath.Join(root, "exported")
	result, err := exportStoredImageRaw(ctx, lease.store, imageID, Output{Path: secondLayout, Reference: "health"}, &types.SystemContext{BigFilesTemporaryDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if result.ManifestDigest != fixture.manifest.Digest.String() {
		t.Fatalf("raw re-export digest = %q, want %q", result.ManifestDigest, fixture.manifest.Digest)
	}
	index := readRawDockerIndex(t, secondLayout)
	exportedManifest := readRawDockerBlob(t, secondLayout, index.Manifests[0])
	if !bytes.Equal(exportedManifest, fixture.manifestData) {
		t.Fatal("storage round trip changed Docker manifest bytes")
	}
	exportedConfig := readRawDockerBlob(t, secondLayout, fixture.config)
	if !bytes.Equal(exportedConfig, fixture.configData) {
		t.Fatal("storage round trip changed Docker config bytes")
	}
	var config struct {
		Config struct {
			Healthcheck struct {
				Test []string
			}
		}
	}
	if err := json.Unmarshal(exportedConfig, &config); err != nil {
		t.Fatal(err)
	}
	if got := config.Config.Healthcheck.Test; len(got) != 2 || got[0] != "CMD" || got[1] != "true" {
		t.Fatalf("healthcheck = %v", got)
	}
}

type rawDockerFixture struct {
	manifest     v1.Descriptor
	config       v1.Descriptor
	manifestData []byte
	configData   []byte
	blobs        map[digest.Digest][]byte
}

func newRawDockerFixture(t *testing.T) rawDockerFixture {
	t.Helper()
	var layer bytes.Buffer
	writer := tar.NewWriter(&layer)
	contents := []byte("raw docker export\n")
	if err := writer.WriteHeader(&tar.Header{Name: "proof", Mode: 0o644, Size: int64(len(contents))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	layerData := layer.Bytes()
	layerDigest := digest.FromBytes(layerData)
	configData, err := json.Marshal(map[string]any{
		"architecture": runtime.GOARCH,
		"os":           "linux",
		"config": map[string]any{
			"Healthcheck": map[string]any{"Test": []string{"CMD", "true"}, "Interval": int64(time.Second)},
		},
		"rootfs":  map[string]any{"type": "layers", "diff_ids": []digest.Digest{layerDigest}},
		"history": []map[string]any{{"created_by": "coopr raw export test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := v1.Descriptor{MediaType: dockerImageConfigMediaType, Digest: digest.FromBytes(configData), Size: int64(len(configData))}
	manifestData, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: dockerManifestMediaType,
		Config: config,
		Layers: []v1.Descriptor{{MediaType: dockerLayerMediaType, Digest: layerDigest, Size: int64(len(layerData))}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := v1.Descriptor{MediaType: dockerManifestMediaType, Digest: digest.FromBytes(manifestData), Size: int64(len(manifestData))}
	return rawDockerFixture{
		manifest: manifest, config: config, manifestData: manifestData, configData: configData,
		blobs: map[digest.Digest][]byte{config.Digest: configData, layerDigest: layerData},
	}
}

type rawDockerSource struct {
	manifest    []byte
	blobs       map[digest.Digest][]byte
	blobsClosed int
}

func (source *rawDockerSource) Reference() types.ImageReference { return nil }
func (source *rawDockerSource) Close() error                    { return nil }
func (source *rawDockerSource) GetManifest(context.Context, *digest.Digest) ([]byte, string, error) {
	return append([]byte(nil), source.manifest...), dockerManifestMediaType, nil
}
func (source *rawDockerSource) GetBlob(_ context.Context, info types.BlobInfo, _ types.BlobInfoCache) (io.ReadCloser, int64, error) {
	data := source.blobs[info.Digest]
	return &countingReadCloser{Reader: bytes.NewReader(data), closed: &source.blobsClosed}, int64(len(data)), nil
}
func (source *rawDockerSource) HasThreadSafeGetBlob() bool { return true }
func (source *rawDockerSource) GetSignatures(context.Context, *digest.Digest) ([][]byte, error) {
	return nil, nil
}
func (source *rawDockerSource) LayerInfosForCopy(context.Context, *digest.Digest) ([]types.BlobInfo, error) {
	return nil, nil
}

type countingReadCloser struct {
	*bytes.Reader
	closed *int
}

func (reader *countingReadCloser) Close() error {
	*reader.closed++
	return nil
}

func readRawDockerIndex(t *testing.T, layout string) v1.Index {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(layout, v1.ImageIndexFile))
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	return index
}

func readRawDockerBlob(t *testing.T, layout string, descriptor v1.Descriptor) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(layout, "blobs", descriptor.Digest.Algorithm().String(), descriptor.Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
