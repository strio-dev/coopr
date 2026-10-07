package transfer

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/localstore"
	"coopr/internal/oci"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestDestinationTransports(t *testing.T) {
	for _, test := range []struct {
		value, transport, name string
	}{
		{"app:dev", "local", "app:dev"},
		{"local:app", "local", "app:latest"},
		{"podman:app:dev", "podman", "localhost/app:dev"},
		{"docker:app:dev", "docker", "app:dev"},
		{"registry:example.com/team/app:dev", "registry", "example.com/team/app:dev"},
		{"oci-archive:app.tar", "oci-archive", "app.tar"},
	} {
		got, err := ParseDestination(test.value, oci.Image)
		if err != nil || got != (Destination{Transport: test.transport, Name: test.name}) {
			t.Fatalf("destination %q = %+v, %v", test.value, got, err)
		}
	}
	for _, value := range []string{"podman:app:dev", "docker:app:dev"} {
		if _, err := ParseDestination(value, oci.Component); err == nil || !strings.Contains(err.Error(), "cannot store Coopr component") {
			t.Fatalf("component destination %q accepted: %v", value, err)
		}
	}
	for _, value := range []string{"podman:app:dev", "docker:app:dev", "local:app:dev", "oci-archive:app.tar"} {
		if _, err := ParsePushDestination(value, oci.Image); err == nil || !strings.Contains(err.Error(), "--push cannot use") {
			t.Fatalf("push target %q accepted: %v", value, err)
		}
	}
	if destination, err := ParsePushDestination("localhost:5000/team/app:dev", oci.Image); err != nil || destination.Transport != "registry" {
		t.Fatalf("registry with port rejected: %+v, %v", destination, err)
	}
}

func TestCopyRootArchiveAndRegistryWithoutRebuild(t *testing.T) {
	ctx := context.Background()
	options := nativeTestStore(t.TempDir())
	source, root := imageFixture(t, ctx)
	layout := filepath.Join(t.TempDir(), "layout")
	if err := localstore.Put(ctx, layout, source, root); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "image.oci.tar")
	result, err := CopyRoot(ctx, oci.Image, layout, root, Destination{Transport: "oci-archive", Name: archive}, Options{BuildStore: options})
	if err != nil || result != archive {
		t.Fatalf("copy archive = %q, %v", result, err)
	}
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	var index v1.Index
	for {
		header, err := reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != "index.json" {
			continue
		}
		if err := json.NewDecoder(reader).Decode(&index); err != nil {
			t.Fatal(err)
		}
		break
	}
	if len(index.Manifests) != 1 || index.Manifests[0].Digest != root.Digest {
		t.Fatalf("archive root = %+v, want %s", index.Manifests, root.Digest)
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	registryRef := strings.TrimPrefix(server.URL, "http://") + "/coopr/copy:dev"
	result, err = CopyRoot(ctx, oci.Image, layout, root, Destination{Transport: "registry", Name: registryRef}, Options{BuildStore: options, PlainHTTP: true})
	if err != nil || result != strings.TrimSuffix(registryRef, ":dev")+"@"+root.Digest.String() {
		t.Fatalf("copy registry = %q, %v", result, err)
	}
	resolver, err := oci.NewResolver(oci.Options{PlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(ctx, registryRef, v1.Platform{OS: "linux", Architecture: "amd64"}, oci.Image)
	if err != nil || resolved.Root.Digest != root.Digest {
		t.Fatalf("registry graph differs: %+v, %v", resolved, err)
	}
}

func TestCopyRootRegistryUsesCredentialsAndDirectCertLeaf(t *testing.T) {
	ctx := context.Background()
	storeDir := filepath.Join(t.TempDir(), "images")
	source, root := imageFixture(t, ctx)
	if err := localstore.Put(ctx, storeDir, source, root); err != nil {
		t.Fatal(err)
	}
	registryHandler := registry.New()
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "coopr" || password != "secret" {
			writer.Header().Set("WWW-Authenticate", `Basic realm="coopr"`)
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		registryHandler.ServeHTTP(writer, request)
	}))
	defer server.Close()
	certDir := t.TempDir()
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(certDir, "ca.crt"), certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	registryRef := strings.TrimPrefix(server.URL, "https://") + "/coopr/private:test"
	result, err := CopyRoot(ctx, oci.Image, storeDir, root, Destination{Transport: "registry", Name: registryRef}, Options{
		Credentials: "coopr:secret", CertDir: certDir, Retry: 0, RetrySet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSuffix(registryRef, ":test") + "@" + root.Digest.String(); result != want {
		t.Fatalf("private registry result = %q, want %q", result, want)
	}
}

func TestArchiveDestinationCannotOverwriteStore(t *testing.T) {
	ctx := context.Background()
	storeDir := filepath.Join(t.TempDir(), "images")
	source, root := imageFixture(t, ctx)
	if err := localstore.Put(ctx, storeDir, source, root); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(storeDir, "index.json")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "store-alias")
	if err := os.Symlink(storeDir, alias); err != nil {
		t.Fatal(err)
	}
	_, err = CopyRoot(ctx, oci.Image, storeDir, root, Destination{Transport: "oci-archive", Name: filepath.Join(alias, "index.json")}, Options{})
	if err == nil || !strings.Contains(err.Error(), "would overwrite the Coopr store") {
		t.Fatalf("store archive overwrite = %v", err)
	}
	after, err := os.ReadFile(index)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("store index changed after rejected archive: %v", err)
	}
}

func imageFixture(t *testing.T, ctx context.Context) (*orasoci.Store, v1.Descriptor) {
	t.Helper()
	store, err := orasoci.NewWithContext(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configBytes := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	config := v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: digest.FromBytes(configBytes), Size: int64(len(configBytes))}
	if err := store.Push(ctx, config, bytes.NewReader(configBytes)); err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config})
	if err != nil {
		t.Fatal(err)
	}
	root := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromBytes(manifestBytes), Size: int64(len(manifestBytes))}
	if err := store.Push(ctx, root, bytes.NewReader(manifestBytes)); err != nil {
		t.Fatal(err)
	}
	return store, root
}
