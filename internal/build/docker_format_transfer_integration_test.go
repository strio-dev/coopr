package build

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/buildah"
	"coopr/internal/oci"
	"coopr/internal/transfer"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	dockerSchema2ManifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"
	dockerImageConfigMediaType     = "application/vnd.docker.container.image.v1+json"
)

func TestDockerFormatSurvivesCatalogCopyToArchiveAndRegistry(t *testing.T) {
	if testing.Short() || os.Getenv("COOPR_TEST_BUILDAH_REGISTRY") == "" {
		t.Skip("set COOPR_TEST_BUILDAH_REGISTRY=1 for a live rootless Buildah registry test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	root := t.TempDir()
	definition := filepath.Join(root, "app.coopr")
	if err := os.WriteFile(definition, []byte(`from "scratch"
healthcheck interval="30s" retries=2 { exec "/bin/check" }
onbuild { env FROM_PARENT="yes" }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(root, "images")
	const tag = "localhost/coopr-docker-transfer:latest"
	if got, err := Run(ctx, Options{
		File: definition, Platform: "linux/" + runtime.GOARCH, Format: "docker", Tag: tag,
		BuildStore: nativeBuildTestStore(storeDir),
	}); err != nil {
		t.Fatalf("build Docker-format image: %v", err)
	} else if got != tag {
		t.Fatalf("build result = %q, want %q", got, tag)
	}

	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	selection, found, err := testStoredImageSelection(ctx, nativeBuildTestStore(storeDir), tag, platform)
	if err != nil || !found {
		t.Fatalf("lookup built image: found=%t err=%v", found, err)
	}
	if selection.Manifest.MediaType != dockerSchema2ManifestMediaType {
		t.Fatalf("catalog manifest media type = %q, want %q", selection.Manifest.MediaType, dockerSchema2ManifestMediaType)
	}
	assertDockerTransferConfig(t, selection.ConfigData)

	archive := filepath.Join(root, "copied.oci.tar")
	archiveDestination, err := transfer.ParseDestination("oci-archive:"+archive, oci.Image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transfer.Copy(ctx, oci.Image, tag, archiveDestination, transfer.Options{BuildStore: nativeBuildTestStore(storeDir)}); err != nil {
		t.Fatalf("copy catalog image to OCI archive: %v", err)
	}
	archiveRoot, archiveManifest, archiveConfig := readDockerTransferArchive(t, archive)
	assertDockerTransferIdentity(t, selection.Manifest, archiveRoot, archiveManifest)
	assertDockerTransferConfig(t, archiveConfig)

	server := httptest.NewServer(registry.New())
	defer server.Close()
	registryRef := strings.TrimPrefix(server.URL, "http://") + "/coopr/docker-transfer:latest"
	registryDestination, err := transfer.ParseDestination("registry:"+registryRef, oci.Image)
	if err != nil {
		t.Fatal(err)
	}
	immutable, err := transfer.Copy(ctx, oci.Image, tag, registryDestination, transfer.Options{
		BuildStore: nativeBuildTestStore(storeDir), TLSVerify: new(false),
	})
	if err != nil {
		t.Fatalf("copy catalog image to registry: %v", err)
	}
	wantImmutable := strings.TrimSuffix(registryRef, ":latest") + "@" + selection.Manifest.Digest.String()
	if immutable != wantImmutable {
		t.Fatalf("registry result = %q, want %q", immutable, wantImmutable)
	}
	resolver, err := oci.NewResolver(oci.Options{
		TLSVerify: new(false), NativeStore: buildah.NativeStoreOptions(nativeBuildTestStore(filepath.Join(root, "empty-image-store"))),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(ctx, immutable, platform, oci.Image)
	if err != nil {
		t.Fatalf("resolve copied registry image: %v", err)
	}
	assertDockerTransferIdentity(t, selection.Manifest, resolved.Root, resolved.Manifest)
	assertDockerTransferConfig(t, resolved.ConfigData)
}

func assertDockerTransferIdentity(t *testing.T, want, root v1.Descriptor, manifest v1.Manifest) {
	t.Helper()
	if root.Digest != want.Digest {
		t.Fatalf("copied manifest digest = %s, want catalog digest %s", root.Digest, want.Digest)
	}
	if root.MediaType != dockerSchema2ManifestMediaType || manifest.MediaType != dockerSchema2ManifestMediaType {
		t.Fatalf("copied manifest media types = root %q document %q, want %q", root.MediaType, manifest.MediaType, dockerSchema2ManifestMediaType)
	}
	if manifest.Config.MediaType != dockerImageConfigMediaType {
		t.Fatalf("copied config media type = %q, want %q", manifest.Config.MediaType, dockerImageConfigMediaType)
	}
}

func assertDockerTransferConfig(t *testing.T, data []byte) {
	t.Helper()
	var document struct {
		Config struct {
			Healthcheck struct {
				Test     []string `json:"Test"`
				Interval int64    `json:"Interval"`
				Retries  int      `json:"Retries"`
			} `json:"Healthcheck"`
			OnBuild []string `json:"OnBuild"`
		} `json:"config"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode copied Docker config: %v", err)
	}
	check := document.Config.Healthcheck
	if len(check.Test) != 2 || check.Test[0] != "CMD" || check.Test[1] != "/bin/check" || check.Interval != int64(30*time.Second) || check.Retries != 2 {
		t.Fatalf("copied Docker healthcheck = %+v", check)
	}
	if len(document.Config.OnBuild) != 1 || document.Config.OnBuild[0] != "ENV FROM_PARENT=yes" {
		t.Fatalf("copied Docker OnBuild = %v", document.Config.OnBuild)
	}
}

func readDockerTransferArchive(t *testing.T, path string) (v1.Descriptor, v1.Manifest, []byte) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	blobs := make(map[string][]byte)
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		blobs[strings.TrimPrefix(header.Name, "./")] = data
	}
	var index v1.Index
	if err := json.Unmarshal(blobs[v1.ImageIndexFile], &index); err != nil {
		t.Fatalf("decode archive index: %v", err)
	}
	if len(index.Manifests) != 1 {
		t.Fatalf("archive roots = %d, want 1", len(index.Manifests))
	}
	root := index.Manifests[0]
	manifestData := blobs[transferBlobPath(root.Digest)]
	if digest.FromBytes(manifestData) != root.Digest {
		t.Fatalf("archive manifest bytes do not match %s", root.Digest)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("decode archive manifest: %v", err)
	}
	config := blobs[transferBlobPath(manifest.Config.Digest)]
	if digest.FromBytes(config) != manifest.Config.Digest {
		t.Fatalf("archive config bytes do not match %s", manifest.Config.Digest)
	}
	return root, manifest, config
}

func transferBlobPath(value digest.Digest) string {
	return fmt.Sprintf("blobs/%s/%s", value.Algorithm(), value.Encoded())
}
