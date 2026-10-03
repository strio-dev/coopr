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
	"time"

	"coopr/internal/localstore"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestCopyDockerIndexStreamsNamedOCIArchiveAndVerifiesManifests(t *testing.T) {
	layout, root, manifests := testIndexLayout(t)
	var loadBody []byte
	server := startDockerAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/_ping":
			writeDockerPing(w)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info"):
			writeDockerInfo(w, true)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images/load"):
			if got := r.URL.Query().Get("quiet"); got != "1" {
				t.Errorf("quiet = %q, want 1", got)
			}
			var err error
			loadBody, err = io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read load archive: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"stream":"Loaded image: docker.io/library/example:dev\n"}`+"\n")
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
			if got := r.URL.Query().Get("manifests"); got != "1" {
				t.Errorf("manifests = %q, want 1", got)
			}
			response := map[string]any{
				"Id":         root.Digest.String(),
				"RepoTags":   []string{"example:dev"},
				"Descriptor": root,
				"Manifests": []map[string]any{
					{"ID": manifests[0].Digest.String(), "Descriptor": manifests[0], "Available": true},
					{"ID": manifests[1].Digest.String(), "Descriptor": manifests[1], "Available": true},
				},
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	})
	t.Setenv("DOCKER_HOST", server)

	got, err := Copy(context.Background(), layout, root, "docker", "example:dev")
	if err != nil {
		t.Fatal(err)
	}
	if got != "docker:example:dev" {
		t.Fatalf("Copy() = %q, want docker:example:dev", got)
	}
	archiveRoot := dockerArchiveRoot(t, loadBody)
	if archiveRoot.Digest != root.Digest || archiveRoot.Size != root.Size || archiveRoot.MediaType != root.MediaType {
		t.Fatalf("archive root = %+v, want content descriptor %+v", archiveRoot, root)
	}
	if got := archiveRoot.Annotations[dockerImageNameAnnotation]; got != "docker.io/library/example:dev" {
		t.Fatalf("%s = %q", dockerImageNameAnnotation, got)
	}
	if got := archiveRoot.Annotations[v1.AnnotationRefName]; got != "dev" {
		t.Fatalf("%s = %q, want dev", v1.AnnotationRefName, got)
	}
	if matches, err := filepath.Glob(filepath.Join(layout, "*.tar")); err != nil || len(matches) != 0 {
		t.Fatalf("copy persisted an archive: %v, %v", matches, err)
	}
}

func TestCopyDockerIndexReturnsLoadStreamError(t *testing.T) {
	layout, root, _ := testIndexLayout(t)
	inspectCalled := false
	server := startDockerAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/_ping":
			writeDockerPing(w)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info"):
			writeDockerInfo(w, true)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images/load"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"error":"import failed","errorDetail":{"code":500,"message":"import failed"}}`+"\n")
		case strings.HasSuffix(r.URL.Path, "/json"):
			inspectCalled = true
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	})
	t.Setenv("DOCKER_HOST", server)

	_, err := Copy(context.Background(), layout, root, "docker", "example:dev")
	if err == nil || !strings.Contains(err.Error(), "docker image load: import failed") {
		t.Fatalf("Copy() error = %v", err)
	}
	if inspectCalled {
		t.Fatal("failed load was inspected")
	}
}

func TestCopyDockerIndexRejectsChangedContentDescriptors(t *testing.T) {
	for _, field := range []string{"root digest", "root media type", "root size", "manifest media type", "manifest size", "unavailable manifest"} {
		t.Run(field, func(t *testing.T) {
			layout, root, manifests := testIndexLayout(t)
			reportedRoot := root
			reportedManifest := manifests[0]
			available := true
			switch field {
			case "root digest":
				reportedRoot.Digest = digest.FromString("different index")
			case "root media type":
				reportedRoot.MediaType = v1.MediaTypeImageManifest
			case "root size":
				reportedRoot.Size++
			case "manifest media type":
				reportedManifest.MediaType = v1.MediaTypeImageIndex
			case "manifest size":
				reportedManifest.Size++
			case "unavailable manifest":
				available = false
			}
			server := startDockerAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodHead && r.URL.Path == "/_ping":
					writeDockerPing(w)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info"):
					writeDockerInfo(w, true)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images/load"):
					_, _ = io.Copy(io.Discard, r.Body)
					_, _ = io.WriteString(w, "{}\n")
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
					_ = json.NewEncoder(w).Encode(map[string]any{
						"Descriptor": reportedRoot,
						"Manifests": []map[string]any{
							{"Descriptor": reportedManifest, "Available": available},
							{"Descriptor": manifests[1], "Available": true},
						},
					})
				default:
					t.Errorf("unexpected Docker request %s %s", r.Method, r.URL.String())
					w.WriteHeader(http.StatusNotFound)
				}
			})
			t.Setenv("DOCKER_HOST", server)
			_, err := Copy(context.Background(), layout, root, "docker", "example:dev")
			if err == nil || !strings.Contains(err.Error(), "docker loaded image") {
				t.Fatalf("Copy accepted changed %s: %v", field, err)
			}
		})
	}
}

func TestCopyDockerIndexRejectsClassicImageStoreBeforeLoad(t *testing.T) {
	layout, root, _ := testIndexLayout(t)
	loadCalled := false
	server := startDockerAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/_ping":
			writeDockerPing(w)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info"):
			writeDockerInfo(w, false)
		case strings.HasSuffix(r.URL.Path, "/images/load"):
			loadCalled = true
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	})
	t.Setenv("DOCKER_HOST", server)

	_, err := Copy(context.Background(), layout, root, "docker", "example:dev")
	if err == nil || !strings.Contains(err.Error(), "classic image store") || !strings.Contains(err.Error(), "containerd image store") {
		t.Fatalf("Copy() error = %v", err)
	}
	if loadCalled {
		t.Fatal("classic image store received an image load")
	}
}

func TestCopyDockerIndexPropagatesArchiveStreamFailure(t *testing.T) {
	layout, root, manifests := testIndexLayout(t)
	missing := filepath.Join(layout, "blobs", manifests[0].Digest.Algorithm().String(), manifests[0].Digest.Encoded())
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	server := startDockerAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/_ping":
			writeDockerPing(w)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info"):
			writeDockerInfo(w, true)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images/load"):
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, "{}\n")
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	})
	t.Setenv("DOCKER_HOST", server)

	_, err := Copy(context.Background(), layout, root, "docker", "example:dev")
	if err == nil || !strings.Contains(err.Error(), "stream OCI archive to Docker") {
		t.Fatalf("Copy() error = %v", err)
	}
}

func TestCopyDockerIndexCancelsPendingLoad(t *testing.T) {
	layout, root, _ := testIndexLayout(t)
	loaded := make(chan struct{})
	server := startDockerAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/_ping":
			writeDockerPing(w)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info"):
			writeDockerInfo(w, true)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images/load"):
			_, _ = io.Copy(io.Discard, r.Body)
			close(loaded)
			<-r.Context().Done()
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	})
	t.Setenv("DOCKER_HOST", server)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Copy(ctx, layout, root, "docker", "example:dev")
		done <- err
	}()
	select {
	case <-loaded:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("Docker load did not receive archive")
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("Copy() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Copy did not stop after cancellation")
	}
}

func testIndexLayout(t *testing.T) (string, v1.Descriptor, []v1.Descriptor) {
	t.Helper()
	ctx := context.Background()
	source, err := orasoci.NewWithContext(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manifests := make([]v1.Descriptor, 0, 2)
	for _, architecture := range []string{"amd64", "arm64"} {
		configData, err := json.Marshal(v1.Image{Platform: v1.Platform{Architecture: architecture, OS: "linux"}, RootFS: v1.RootFS{Type: "layers"}})
		if err != nil {
			t.Fatal(err)
		}
		config := v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: digest.FromBytes(configData), Size: int64(len(configData))}
		if err := source.Push(ctx, config, bytes.NewReader(configData)); err != nil {
			t.Fatal(err)
		}
		manifestData, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config})
		if err != nil {
			t.Fatal(err)
		}
		manifest := v1.Descriptor{
			MediaType: v1.MediaTypeImageManifest, Digest: digest.FromBytes(manifestData), Size: int64(len(manifestData)),
			Platform: &v1.Platform{Architecture: architecture, OS: "linux"},
		}
		if err := source.Push(ctx, manifest, bytes.NewReader(manifestData)); err != nil {
			t.Fatal(err)
		}
		manifests = append(manifests, manifest)
	}
	indexData, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: manifests})
	if err != nil {
		t.Fatal(err)
	}
	root := v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: digest.FromBytes(indexData), Size: int64(len(indexData))}
	if err := source.Push(ctx, root, bytes.NewReader(indexData)); err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(t.TempDir(), "images")
	if err := localstore.Put(ctx, layout, source, root, "example:index"); err != nil {
		t.Fatal(err)
	}
	return layout, root, manifests
}

func startDockerAPIServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	runtimeDir, err := os.MkdirTemp("/tmp", "coopr-docker-index-")
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
	return "unix://" + socket
}

func writeDockerPing(w http.ResponseWriter) {
	w.Header().Set("API-Version", "1.52")
	w.Header().Set("OSType", "linux")
	w.WriteHeader(http.StatusOK)
}

func writeDockerInfo(w http.ResponseWriter, containerdStore bool) {
	driverStatus := [][2]string{{"Backing Filesystem", "extfs"}}
	if containerdStore {
		driverStatus = append(driverStatus, [2]string{"driver-type", "io.containerd.snapshotter.v1"})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"Driver": "overlay2", "DriverStatus": driverStatus})
}

func dockerArchiveRoot(t *testing.T, data []byte) v1.Descriptor {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := reader.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		if header.Name != v1.ImageIndexFile {
			continue
		}
		var index v1.Index
		if err := json.NewDecoder(reader).Decode(&index); err != nil {
			t.Fatal(err)
		}
		if len(index.Manifests) != 1 {
			t.Fatalf("archive index has %d roots, want 1", len(index.Manifests))
		}
		return index.Manifests[0]
	}
	t.Fatal("archive has no index.json")
	return v1.Descriptor{}
}
