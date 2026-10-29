package build

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/transfer"
	"go.podman.io/common/libimage"
	"go.podman.io/common/libimage/manifests"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/signature"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/storage"
)

const (
	dockerSchema2ManifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"
	dockerImageConfigMediaType     = "application/vnd.docker.container.image.v1+json"
)

func TestDockerFormatSurvivesCatalogCopyToArchiveAndRegistry(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native build integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	root := t.TempDir()
	definition := filepath.Join(root, "app.coopr")
	if err := os.WriteFile(definition, []byte(`from "scratch" {
    healthcheck interval="30s" retries=2 { exec "/bin/check" }
    onbuild { env FROM_PARENT="yes" }
}
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
	} else if len(got.References) != 1 || got.References[0] != tag {
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
	nativeRoot, nativeManifest, nativeConfig := readDockerTransferArchive(t, nativeDockerTransferArchive(t, ctx, nativeBuildTestStore(storeDir), tag))
	if archiveRoot.Digest != nativeRoot.Digest || archiveRoot.MediaType != v1.MediaTypeImageManifest || archiveManifest.Config.MediaType != v1.MediaTypeImageConfig || !reflect.DeepEqual(archiveManifest, nativeManifest) || !bytes.Equal(archiveConfig, nativeConfig) {
		t.Fatalf("OCI archive differs from native Docker-to-OCI conversion: root=%+v native=%+v", archiveRoot, nativeRoot)
	}

	server := httptest.NewServer(registry.New())
	defer server.Close()
	registryRef := strings.TrimPrefix(server.URL, "http://") + "/coopr/docker-transfer:latest"
	registryDestination, err := transfer.ParseDestination("docker://"+registryRef, oci.Image)
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
	root, blobs := readDockerTransferArchiveBlobs(t, path)
	manifestData := blobs[transferBlobPath(root.Digest)]
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	config := blobs[transferBlobPath(manifest.Config.Digest)]
	if digest.FromBytes(config) != manifest.Config.Digest {
		t.Fatalf("archive config bytes do not match %s", manifest.Config.Digest)
	}
	return root, manifest, config
}

func readDockerTransferArchiveBlobs(t *testing.T, path string) (v1.Descriptor, map[string][]byte) {
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
	return root, blobs
}

func transferBlobPath(value digest.Digest) string {
	return fmt.Sprintf("blobs/%s/%s", value.Algorithm(), value.Encoded())
}

// Use the public native copy path as the format-conversion control, independently
// of Coopr's transfer dispatcher and prepared-source handling.
func nativeDockerTransferArchive(t *testing.T, ctx context.Context, options buildah.StoreOptions, source string) string {
	t.Helper()
	archive := filepath.Join(t.TempDir(), "native.oci.tar")
	err := buildah.WithStore(options, func(backend storage.Store) error {
		previous := imagestorage.Transport.GetStoreIfSet()
		imagestorage.Transport.SetStore(backend)
		defer imagestorage.Transport.SetStore(previous)
		native, err := libimage.RuntimeFromStore(backend, nil)
		if err != nil {
			return err
		}
		image, _, err := native.LookupImage(source, &libimage.LookupImageOptions{ManifestList: true})
		if err != nil {
			return err
		}
		ref, err := image.StorageReference()
		if err != nil {
			return err
		}
		isList, err := image.IsManifestList(ctx)
		if err != nil {
			return err
		}
		if isList {
			_, list, err := manifests.LoadFromImage(backend, image.ID())
			if err != nil {
				return err
			}
			ref, err = list.Reference(backend, imagecopy.CopyAllImages, nil)
			if err != nil {
				return err
			}
		}
		destination, err := alltransports.ParseImageName("oci-archive:" + archive)
		if err != nil {
			return err
		}
		policy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}})
		if err != nil {
			return err
		}
		_, copyErr := imagecopy.Image(ctx, policy, destination, ref, &imagecopy.Options{ImageListSelection: imagecopy.CopyAllImages})
		return errors.Join(copyErr, policy.Destroy())
	})
	if err != nil {
		t.Fatalf("native OCI archive control: %v", err)
	}
	return archive
}
