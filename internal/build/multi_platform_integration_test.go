package build

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"coopr/internal/buildah"
	"coopr/internal/oci"
	"coopr/internal/transfer"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestRunBuildsAndCopiesMultiPlatformIndex(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH_REGISTRY") == "" {
		t.Skip("set COOPR_TEST_BUILDAH_REGISTRY for live Buildah registry tests")
	}
	ctx := context.Background()
	work := t.TempDir()
	storeDir := filepath.Join(work, "images")
	file := filepath.Join(work, "base.coopr")
	if err := os.WriteFile(file, []byte("from \"scratch\"\nlabel multi=\"yes\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const tag = "multi:latest"
	if got, err := Run(ctx, Options{File: file, BuildStore: nativeBuildTestStore(storeDir), Tag: tag, Platforms: []string{"linux/arm64", "linux/amd64"}}); err != nil || got != tag {
		t.Fatalf("multi-platform build = %q, %v", got, err)
	}
	root, indexData, selections, found, err := testStoredImageIndex(ctx, nativeBuildTestStore(storeDir), tag)
	if err != nil || !found || len(selections) != 2 {
		t.Fatalf("cataloged index = %s, %t, %d platforms, %v", root.Digest, found, len(selections), err)
	}
	var index v1.Index
	if err := json.Unmarshal(indexData, &index); err != nil || index.MediaType != v1.MediaTypeImageIndex || len(index.Manifests) != 2 {
		t.Fatalf("built index = %+v, %v", index, err)
	}
	if index.Manifests[0].Platform.Architecture != "arm64" || index.Manifests[1].Platform.Architecture != "amd64" {
		t.Fatalf("index platform order = %+v", index.Manifests)
	}
	archive := filepath.Join(work, "multi.oci.tar")
	if _, err := transfer.Copy(ctx, oci.Image, tag, transfer.Destination{Transport: "oci-archive", Name: archive}, transfer.Options{BuildStore: nativeBuildTestStore(storeDir)}); err != nil {
		t.Fatalf("copy complete index to archive: %v", err)
	}
	archived, err := orasoci.NewFromTar(ctx, archive)
	if err != nil {
		t.Fatal(err)
	}
	archivedRoot, err := archived.Resolve(ctx, root.Digest.String())
	if err != nil || archivedRoot.Digest != root.Digest {
		t.Fatalf("archive root = %s, %v; want %s", archivedRoot.Digest, err, root.Digest)
	}
	archivedData, err := content.FetchAll(ctx, archived, root)
	if err != nil || string(archivedData) != string(indexData) {
		t.Fatalf("archived index differs: %v", err)
	}
	for _, manifest := range index.Manifests {
		if _, err := content.FetchAll(ctx, archived, manifest); err != nil {
			t.Fatalf("archive lacks %s manifest: %v", manifest.Platform.Architecture, err)
		}
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	remote := strings.TrimPrefix(server.URL, "http://") + "/coopr/multi:latest"
	if _, err := transfer.Copy(ctx, oci.Image, tag, transfer.Destination{Transport: "registry", Name: remote}, transfer.Options{BuildStore: nativeBuildTestStore(storeDir), TLSVerify: new(false)}); err != nil {
		t.Fatalf("copy complete index to registry: %v", err)
	}
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false), NativeStore: buildah.NativeStoreOptions(nativeBuildTestStore(filepath.Join(work, "remote-images")))})
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"arm64", "amd64"} {
		resolved, err := resolver.Resolve(ctx, remote, v1.Platform{OS: "linux", Architecture: arch}, oci.Image)
		if err != nil || resolved.Root.Digest != root.Digest || resolved.Selected.Digest != selections["linux/"+arch].Manifest.Digest {
			t.Fatalf("resolve registry %s = root %s selected %s, %v", arch, resolved.Root.Digest, resolved.Selected.Digest, err)
		}
	}
	directRemote := strings.TrimPrefix(server.URL, "http://") + "/coopr/direct:latest"
	immutable, err := Run(ctx, Options{File: file, BuildStore: nativeBuildTestStore(storeDir), Tag: directRemote, Push: true, TLSVerify: new(false), Platforms: []string{"linux/arm64", "linux/amd64"}})
	if err != nil || !strings.HasPrefix(immutable, strings.TrimSuffix(directRemote, ":latest")+"@sha256:") {
		t.Fatalf("direct multi-platform push = %q, %v", immutable, err)
	}
	for _, arch := range []string{"arm64", "amd64"} {
		resolved, err := resolver.Resolve(ctx, immutable, v1.Platform{OS: "linux", Architecture: arch}, oci.Image)
		if err != nil || immutable != resolved.Repository+"@"+resolved.Root.Digest.String() {
			t.Fatalf("resolve direct push %s = %s, %v; want %s", arch, resolved.Root.Digest, err, immutable)
		}
	}
	alias := "localhost/multi-alias:latest"
	if _, err := transfer.Copy(ctx, oci.Image, tag, transfer.Destination{Transport: "local", Name: alias}, transfer.Options{BuildStore: nativeBuildTestStore(storeDir)}); err != nil {
		t.Fatalf("retag complete index: %v", err)
	}
	aliasedRoot, _, _, found, err := testStoredImageIndex(ctx, nativeBuildTestStore(storeDir), alias)
	if err != nil || !found || aliasedRoot.Digest != root.Digest {
		t.Fatalf("aliased index = %s, %t, %v", aliasedRoot.Digest, found, err)
	}
	selectedAlias := "multi-amd64:latest"
	if _, err := transfer.Copy(ctx, oci.Image, tag, transfer.Destination{Transport: "local", Name: selectedAlias}, transfer.Options{
		BuildStore: nativeBuildTestStore(storeDir), Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, PlatformExplicit: true,
	}); err != nil {
		t.Fatalf("retag selected platform: %v", err)
	}
	if unexpectedRoot, _, _, complete, err := testStoredImageIndex(ctx, nativeBuildTestStore(storeDir), selectedAlias); err != nil || complete {
		t.Fatalf("selected alias resolves to complete index %s, complete=%t, err=%v", unexpectedRoot.Digest, complete, err)
	}
	selected, found, err := testStoredImageSelection(ctx, nativeBuildTestStore(storeDir), selectedAlias, v1.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil || !found || selected.Root.Digest != selections["linux/amd64"].Manifest.Digest {
		t.Fatalf("selected alias root = %s, found=%t, err=%v", selected.Root.Digest, found, err)
	}
	child := filepath.Join(work, "child.coopr")
	if err := os.WriteFile(child, []byte("from \"localhost/multi-alias:latest\"\nlabel child=\"yes\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"arm64", "amd64"} {
		output := filepath.Join(work, "child-"+arch+".oci.tar")
		if _, err := Run(ctx, Options{File: child, BuildStore: nativeBuildTestStore(storeDir), Tag: "oci-archive:" + output, Platform: "linux/" + arch}); err != nil {
			t.Fatalf("build child for %s from local index: %v", arch, err)
		}
		_, image := readExampleImage(t, ctx, output)
		if image.Architecture != arch || image.Config.Labels["multi"] != "yes" || image.Config.Labels["child"] != "yes" {
			t.Fatalf("child %s config = %+v", arch, image)
		}
	}
}

func TestCopyMultiPlatformIndexToPodman(t *testing.T) {
	loadTestBackend(t)
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skipf("Podman is unavailable: %v", err)
	}
	ctx := context.Background()
	work := t.TempDir()
	storeOptions, err := buildah.DefaultStoreOptions()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(work, "podman.coopr")
	if err := os.WriteFile(file, []byte("from \"scratch\"\nlabel copied=\"yes\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTag := fmt.Sprintf("podman-index-%d-%d:test", os.Getpid(), time.Now().UnixNano())
	if _, err := Run(ctx, Options{File: file, BuildStore: storeOptions, Tag: sourceTag, Platforms: []string{"linux/amd64", "linux/arm64"}}); err != nil {
		t.Fatal(err)
	}
	sourceRoot, sourceData, _, found, err := testStoredImageIndex(ctx, storeOptions, sourceTag)
	if err != nil || !found {
		t.Fatalf("lookup source index: %t, %v", found, err)
	}
	var sourceIndex v1.Index
	if err := json.Unmarshal(sourceData, &sourceIndex); err != nil {
		t.Fatalf("parse source index: %v", err)
	}
	tag := fmt.Sprintf("localhost/coopr-index-test-%d-%d:test", os.Getpid(), time.Now().UnixNano())
	cli := storageTestCLI(t, ctx)
	if output, err := exec.CommandContext(ctx, cli, "copy", sourceTag, "podman:"+tag).CombinedOutput(); err != nil {
		t.Fatalf("copy complete index to Podman: %v: %s", err, output)
	}
	t.Cleanup(func() {
		if output, err := exec.Command("podman", "manifest", "rm", tag).CombinedOutput(); err != nil {
			t.Errorf("remove test Podman manifest %s: %v: %s", tag, err, output)
		}
	})
	output, err := exec.CommandContext(ctx, "podman", "manifest", "inspect", tag).Output()
	if err != nil {
		t.Fatalf("inspect Podman index: %v", err)
	}
	var index v1.Index
	if err := json.Unmarshal(output, &index); err != nil || !reflect.DeepEqual(index, sourceIndex) {
		t.Fatalf("Podman index = %+v, %v", index, err)
	}
	assertPodmanIndexDigest(t, ctx, tag, sourceRoot.Digest)
	if err := os.WriteFile(file, []byte("from \"scratch\"\nlabel copied=\"updated\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, Options{File: file, BuildStore: storeOptions, Tag: sourceTag, Platforms: []string{"linux/amd64", "linux/arm64"}}); err != nil {
		t.Fatal(err)
	}
	updatedRoot, updatedData, _, found, err := testStoredImageIndex(ctx, storeOptions, sourceTag)
	if err != nil || !found || updatedRoot.Digest == sourceRoot.Digest {
		t.Fatalf("lookup updated source index: %s, %t, %v", updatedRoot.Digest, found, err)
	}
	var updatedSourceIndex v1.Index
	if err := json.Unmarshal(updatedData, &updatedSourceIndex); err != nil {
		t.Fatalf("parse updated source index: %v", err)
	}
	if output, err := exec.CommandContext(ctx, cli, "copy", sourceTag, "podman:"+tag).CombinedOutput(); err != nil {
		t.Fatalf("refresh Podman index tag: %v: %s", err, output)
	}
	updated, err := exec.CommandContext(ctx, "podman", "manifest", "inspect", tag).Output()
	if err != nil || string(updated) == string(output) {
		t.Fatalf("Podman index tag did not refresh: %v", err)
	}
	var updatedIndex v1.Index
	if err := json.Unmarshal(updated, &updatedIndex); err != nil || !reflect.DeepEqual(updatedIndex, updatedSourceIndex) {
		t.Fatalf("updated Podman index = %+v, %v", updatedIndex, err)
	}
	assertPodmanIndexDigest(t, ctx, tag, updatedRoot.Digest)
}

func assertPodmanIndexDigest(t *testing.T, ctx context.Context, tag string, want digest.Digest) {
	t.Helper()
	work := t.TempDir()
	digestFile := filepath.Join(work, "digest")
	destination := "oci:" + filepath.Join(work, "layout")
	output, err := exec.CommandContext(ctx, "podman", "manifest", "push", "--quiet", "--digestfile", digestFile, tag, destination).CombinedOutput()
	if err != nil {
		t.Fatalf("export Podman index digest: %v: %s", err, output)
	}
	digestData, err := os.ReadFile(digestFile)
	if err != nil {
		t.Fatalf("read Podman index digest: %v", err)
	}
	got, err := digest.Parse(strings.TrimSpace(string(digestData)))
	if err != nil {
		t.Fatalf("parse Podman index digest %q: %v", digestData, err)
	}
	if got != want {
		t.Fatalf("Podman index digest = %s, want %s", got, want)
	}
}

func TestRunBuildsDockerManifestList(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for live Buildah tests")
	}
	ctx := context.Background()
	work := t.TempDir()
	storeDir := filepath.Join(work, "images")
	file := filepath.Join(work, "docker.coopr")
	if err := os.WriteFile(file, []byte("from \"scratch\"\nlabel format=\"docker\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const tag = "docker-multi:latest"
	if _, err := Run(ctx, Options{File: file, BuildStore: nativeBuildTestStore(storeDir), Tag: tag, Platforms: []string{"linux/amd64", "linux/arm64"}, Format: "docker"}); err != nil {
		t.Fatal(err)
	}
	root, data, selections, found, err := testStoredImageIndex(ctx, nativeBuildTestStore(storeDir), tag)
	if err != nil || !found || len(selections) != 2 {
		t.Fatalf("Docker list catalog = %s, %t, %d, %v", root.Digest, found, len(selections), err)
	}
	const dockerListType = "application/vnd.docker.distribution.manifest.list.v2+json"
	const dockerManifestType = "application/vnd.docker.distribution.manifest.v2+json"
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil || root.MediaType != dockerListType || index.MediaType != dockerListType || len(index.Manifests) != 2 {
		t.Fatalf("Docker list = %+v, root=%s, %v", index, root.MediaType, err)
	}
	for _, manifest := range index.Manifests {
		if manifest.MediaType != dockerManifestType {
			t.Fatalf("Docker list child media type = %s", manifest.MediaType)
		}
	}
	archive := filepath.Join(work, "docker-multi.oci.tar")
	if _, err := transfer.Copy(ctx, oci.Image, tag, transfer.Destination{Transport: "oci-archive", Name: archive}, transfer.Options{BuildStore: nativeBuildTestStore(storeDir)}); err != nil {
		t.Fatalf("copy Docker list to archive: %v", err)
	}
	source, err := orasoci.NewFromTar(ctx, archive)
	if err != nil {
		t.Fatal(err)
	}
	if copied, err := content.FetchAll(ctx, source, root); err != nil || string(copied) != string(data) {
		t.Fatalf("Docker list archive changed index bytes: %v", err)
	}
}
