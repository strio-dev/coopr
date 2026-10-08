package enginecopy

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/localstore"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestCopyDockerStreamsConvertedImageToImageLoad(t *testing.T) {
	layout, root := testImageLayout(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	policyDir := filepath.Join(home, ".config", "containers")
	if err := os.MkdirAll(policyDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(policyDir, "policy.json"), []byte(`{"default":[{"type":"reject"}],"transports":{"oci":{"": [{"type":"insecureAcceptAnything"}]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var (
		loadCalled bool
		loadBody   []byte
	)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/_ping":
			w.Header().Set("API-Version", "1.48")
			w.Header().Set("OSType", "linux")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images/load"):
			if got := r.URL.Query().Get("quiet"); got != "1" {
				t.Errorf("quiet = %q, want 1", got)
			}
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read ImageLoad body: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			loadCalled = true
			loadBody = data
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{}\n")
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	runtimeDir, err := os.MkdirTemp("/tmp", "coopr-docker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	socket := filepath.Join(runtimeDir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	t.Setenv("DOCKER_HOST", "unix://"+socket)

	got, err := Copy(context.Background(), layout, root, "docker", "example:dev")
	if err != nil {
		t.Fatal(err)
	}
	if got != "docker:example:dev" {
		t.Fatalf("Copy() = %q, want docker:example:dev", got)
	}
	if !loadCalled {
		t.Fatal("Docker ImageLoad was not called")
	}
	assertDockerLoadArchive(t, loadBody, "docker.io/library/example:dev")
	if matches, err := filepath.Glob(filepath.Join(layout, "*.tar")); err != nil || len(matches) != 0 {
		t.Fatalf("copy persisted an archive: %v, %v", matches, err)
	}
}

func TestCopyRejectsUnsupportedEngine(t *testing.T) {
	for _, engine := range []string{"podman", "buildah"} {
		if _, err := Copy(context.Background(), "ignored", v1.Descriptor{}, engine, "app:dev"); err == nil || !strings.Contains(err.Error(), "unsupported image engine") {
			t.Fatalf("Copy accepted engine %q: %v", engine, err)
		}
		if _, err := Read(context.Background(), engine, "app:dev", "ignored", v1.Platform{}); err == nil || !strings.Contains(err.Error(), "unsupported image engine") {
			t.Fatalf("Read accepted engine %q: %v", engine, err)
		}
	}
}

func testImageLayout(t *testing.T) (string, v1.Descriptor) {
	t.Helper()
	ctx := context.Background()
	source, err := orasoci.NewWithContext(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configData := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	config := v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: digest.FromBytes(configData), Size: int64(len(configData))}
	if err := source.Push(ctx, config, bytes.NewReader(configData)); err != nil {
		t.Fatal(err)
	}
	manifestData, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    config,
	})
	if err != nil {
		t.Fatal(err)
	}
	root := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromBytes(manifestData), Size: int64(len(manifestData))}
	if err := source.Push(ctx, root, bytes.NewReader(manifestData)); err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(t.TempDir(), "images")
	if err := localstore.Put(ctx, layout, source, root, "example:source"); err != nil {
		t.Fatal(err)
	}
	return layout, root
}

func assertDockerLoadArchive(t *testing.T, data []byte, tag string) {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(data))
	foundManifest := false
	for {
		header, err := reader.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		if header.Name == "oci-layout" || header.Name == "index.json" {
			t.Fatalf("Docker ImageLoad received OCI-layout entry %q", header.Name)
		}
		if header.Name != "manifest.json" {
			continue
		}
		foundManifest = true
		manifestData, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		var manifests []struct {
			Config   string   `json:"Config"`
			RepoTags []string `json:"RepoTags"`
			Layers   []string `json:"Layers"`
		}
		if err := json.Unmarshal(manifestData, &manifests); err != nil {
			t.Fatal(err)
		}
		if len(manifests) != 1 || manifests[0].Config == "" || len(manifests[0].RepoTags) != 1 || manifests[0].RepoTags[0] != tag {
			t.Fatalf("Docker manifest = %+v, want one image tagged %q", manifests, tag)
		}
	}
	if !foundManifest {
		t.Fatal("Docker load stream does not contain manifest.json")
	}
}
