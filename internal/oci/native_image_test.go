package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
)

func TestNativeImageResolutionUsesRegistriesConfMirror(t *testing.T) {
	server, repo, _, _ := testRepository(t, registryserver.New())
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	manifest, _ := imageManifest(t, repo, platform, "mirror")
	stream, err := repo.Fetch(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.PushReference(context.Background(), manifest, stream, "mirror"); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(t.TempDir(), "registries.conf")
	authority := server.Listener.Addr().String()
	data := fmt.Sprintf(`[[registry]]
prefix = "origin.invalid/coopr/source"
location = "origin.invalid/coopr/source"

[[registry.mirror]]
location = %q
insecure = true
`, authority+"/coopr/test")
	if err := os.WriteFile(conf, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(Options{ImageStoreDir: filepath.Join(t.TempDir(), "images")})
	if err != nil {
		t.Fatal(err)
	}
	resolver.system.SystemRegistriesConfPath = conf
	resolved, err := resolver.Resolve(context.Background(), "origin.invalid/coopr/source:mirror", platform, Image)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Selected.Digest != manifest.Digest || !bytes.Equal(resolved.ConfigData, mustFetchConfig(t, repo, manifest)) {
		t.Fatalf("mirror resolution selected %+v", resolved.Selected)
	}
}

func TestNativeImageResolutionAcceptsTagWithDigest(t *testing.T) {
	server, repo, _, _ := testRepository(t, registryserver.New())
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	manifest, _ := imageManifest(t, repo, platform, "pinned")
	host := strings.TrimPrefix(server.URL, "http://")
	resolver, err := NewResolver(Options{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.ResolveRemoteImage(context.Background(), host+"/coopr/test:unused-tag@"+manifest.Digest.String(), platform)
	if err != nil || resolved.Selected.Digest != manifest.Digest {
		t.Fatalf("tag and digest resolution=%+v err=%v", resolved, err)
	}
}

func TestNativeImageResolutionRejectsBlockedRegistry(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "registries.conf")
	if err := os.WriteFile(conf, []byte(`[[registry]]
location = "blocked.example"
blocked = true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(Options{ImageStoreDir: filepath.Join(t.TempDir(), "images")})
	if err != nil {
		t.Fatal(err)
	}
	resolver.system.SystemRegistriesConfPath = conf
	if _, err := resolver.Resolve(context.Background(), "blocked.example/team/image:latest", v1.Platform{OS: "linux", Architecture: "amd64"}, Image); err == nil {
		t.Fatal("blocked registry unexpectedly resolved")
	}
}

func TestNativeImageResolutionUsesDirectCertificateDirectory(t *testing.T) {
	server := httptest.NewTLSServer(registryserver.New())
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "https://")
	repo, err := remote.NewRepository(host + "/coopr/tls")
	if err != nil {
		t.Fatal(err)
	}
	repo.Client = server.Client()
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	manifest, _ := imageManifest(t, repo, platform, "custom CA")
	stream, err := repo.Fetch(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.PushReference(context.Background(), manifest, stream, "latest"); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	certDir := t.TempDir()
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(certDir, "ca.crt"), certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(Options{CertDir: certDir})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(context.Background(), host+"/coopr/tls:latest", platform, Image)
	if err != nil || resolved.Selected.Digest != manifest.Digest {
		t.Fatalf("direct certificate directory resolution = %+v, %v", resolved, err)
	}
}

func mustFetchConfig(t *testing.T, repo interface {
	Fetch(context.Context, v1.Descriptor) (io.ReadCloser, error)
}, manifest v1.Descriptor) []byte {
	t.Helper()
	manifestStream, err := repo.Fetch(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestData, err := io.ReadAll(manifestStream)
	if err != nil {
		t.Fatal(err)
	}
	_ = manifestStream.Close()
	var parsed v1.Manifest
	if err := json.Unmarshal(manifestData, &parsed); err != nil {
		t.Fatal(err)
	}
	configStream, err := repo.Fetch(context.Background(), parsed.Config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = configStream.Close() }()
	data, err := io.ReadAll(configStream)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
