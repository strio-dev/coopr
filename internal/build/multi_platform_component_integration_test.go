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
	"strings"
	"testing"

	"coopr/internal/componentstore"
	"coopr/internal/oci"
	"coopr/internal/transfer"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
)

func TestBuildMultiPlatformComponentAndConsumeBothPlatforms(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH_REGISTRY") == "" {
		t.Skip("set COOPR_TEST_BUILDAH_REGISTRY for live Buildah registry tests")
	}
	ctx := context.Background()
	work := t.TempDir()
	file := filepath.Join(work, "component.coopr")
	definition := "package as=\"pkg\"\narg \"TARGETARCH\"\ncopy \"payload-$TARGETARCH\" \"/artifact\"\nextend as=\"base\"\ncopy \"/artifact\" \"/installed\" from=\"pkg\"\n"
	if err := os.WriteFile(file, []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if err := os.WriteFile(filepath.Join(work, "payload-"+arch), []byte(arch+" package\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const tag = "multi-component"
	got, err := BuildComponent(ctx, ComponentOptions{
		File: file, Tag: tag, Platforms: []string{"linux/arm64", "linux/amd64"},
	})
	if err != nil || got != tag {
		t.Fatalf("multi-platform component build = %q, %v", got, err)
	}
	storeDir, err := componentstore.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	store, err := componentstore.Open(ctx, storeDir)
	if err != nil {
		t.Fatal(err)
	}
	root, err := store.Resolve(ctx, tag)
	if err != nil || root.MediaType != v1.MediaTypeImageIndex {
		t.Fatalf("stored component root = %+v, %v", root, err)
	}
	indexData, err := content.FetchAll(ctx, store, root)
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(indexData, &index); err != nil || len(index.Manifests) != 2 || index.Manifests[0].Platform.Architecture != "arm64" || index.Manifests[1].Platform.Architecture != "amd64" {
		t.Fatalf("component index = %+v, %v", index, err)
	}

	server := httptest.NewServer(registry.New())
	defer server.Close()
	remote := strings.TrimPrefix(server.URL, "http://") + "/coopr/multi-component:latest"
	destination, err := transfer.ParseDestination("registry:"+remote, oci.Component)
	if err != nil {
		t.Fatal(err)
	}
	immutable, err := transfer.Copy(ctx, oci.Component, "local:"+tag, destination, transfer.Options{ComponentStoreDir: storeDir, TLSVerify: new(false)})
	if err != nil || !strings.Contains(immutable, "@"+root.Digest.String()) {
		t.Fatalf("copy component index to registry = %q, %v", immutable, err)
	}
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false), ComponentStoreDir: storeDir})
	if err != nil {
		t.Fatal(err)
	}
	for _, reference := range []string{"local:" + tag, immutable} {
		for _, arch := range []string{"amd64", "arm64"} {
			t.Run(fmt.Sprintf("%s/%s", reference, arch), func(t *testing.T) {
				platform := v1.Platform{OS: "linux", Architecture: arch}
				resolved, err := resolver.Resolve(ctx, reference, platform, oci.Component)
				if err != nil || resolved.Root.Digest != root.Digest || resolved.Component == nil || resolved.Component.Platform.Architecture != arch || len(resolved.Component.Packages) != 1 {
					t.Fatalf("resolve component = %+v, %v", resolved, err)
				}
				pkg := filepath.Join(t.TempDir(), "package.tar")
				if err := resolver.Download(ctx, resolved, resolved.Component.Packages[0].Descriptor, pkg); err != nil {
					t.Fatal(err)
				}
				if got := packageArtifact(t, pkg); got != arch+" package\n" {
					t.Fatalf("package artifact = %q, want %s", got, arch)
				}
				consumer := t.TempDir()
				app := filepath.Join(consumer, "app.coopr")
				if err := os.WriteFile(app, []byte("from \"scratch\"\ncomponent \""+reference+"\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				archive := filepath.Join(consumer, "app.oci.tar")
				if _, err := Run(ctx, Options{File: app, Tag: "oci-archive:" + archive, Platform: "linux/" + arch, TLSVerify: new(false)}); err != nil {
					t.Fatal(err)
				}
				if !containsName(archiveLayerNames(t, archive), "installed") {
					t.Fatal("consumer image lacks installed component artifact")
				}
			})
		}
	}
}

func packageArtifact(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	archive := tar.NewReader(file)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			t.Fatal("package lacks artifact")
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimPrefix(header.Name, "./") != "artifact" {
			continue
		}
		data, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
}
