package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"coopr/internal/oci"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
)

func TestNativePushUsesDockerHost(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers")
	}
	ctx := context.Background()
	store := nativeTestStore(t.TempDir())
	selected := nativeEmptyImageFixture(t, store, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "docker-host")
	if err := nameNativeTestImage(ctx, store, "source:latest", selected); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	before := storedImageInventory(t, store)
	var loaded []byte
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/_ping":
			w.Header().Set("API-Version", "1.48")
			w.Header().Set("OSType", "linux")
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images/load"):
			var err error
			loaded, err = io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read Docker load: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, "{}\n")
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	if err := server.Listener.Close(); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("/tmp", "coopr-host-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	t.Setenv("DOCKER_HOST", "unix://"+socket)
	var progress bytes.Buffer
	result, err := Copy(ctx, oci.Image, "source:latest", Destination{Transport: "docker", Name: "example:dev"}, Options{BuildStore: store, SignaturePolicyPath: policy, ProgressWriter: &progress})
	server.Close() // Join handlers before reading the captured archive.
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) == 0 || !strings.HasPrefix(result, "docker-daemon:example:dev@sha256:") {
		t.Fatalf("push result = %q, daemon received %d bytes", result, len(loaded))
	}
	if !strings.Contains(progress.String(), "Writing manifest") {
		t.Fatalf("missing copy progress: %s", &progress)
	}
	assertDockerPushSourcesPreserved(t, before, storedImageInventory(t, store))
}

func TestNativeManifestPushUsesDockerHost(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers")
	}
	ctx := context.Background()
	store := nativeTestStore(t.TempDir())
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	selected := nativeEmptyImageFixture(t, store, platform, "manifest-docker-host")
	member := selected.Manifest
	member.Platform = &platform
	data, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{member}})
	if err != nil {
		t.Fatal(err)
	}
	root := oci.Descriptor(v1.MediaTypeImageIndex, data)
	if err := nativeTestIndex(ctx, store, "list:latest", root, data, map[string]oci.StoredSelection{"linux/" + runtime.GOARCH: selected}); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	before := storedImageInventory(t, store)
	// The native docker-daemon transport cannot copy a manifest list. An invalid host must
	// nevertheless fail at destination initialization, proving the native list
	// copier receives DOCKER_HOST before considering multi-image support.
	t.Setenv("DOCKER_HOST", "configured-host-without-scheme")
	for _, all := range []bool{false, true} {
		_, err := PushManifestList(ctx, "list:latest", Destination{Transport: "docker", Name: "example:dev"}, Options{BuildStore: store, SignaturePolicyPath: policy}, all, nil)
		if err == nil || !strings.Contains(err.Error(), "unable to parse docker host `configured-host-without-scheme`") {
			t.Fatalf("all=%v: push error = %v, want configured Docker host initialization error", all, err)
		}
	}
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "absent-docker.sock"))
	for _, all := range []bool{false, true} {
		_, err := PushManifestList(ctx, "list:latest", Destination{Transport: "docker", Name: "example:dev"}, Options{BuildStore: store, SignaturePolicyPath: policy}, all, nil)
		if err == nil || !strings.Contains(err.Error(), `destination transport "docker-daemon" does not support copying multiple images as a group`) {
			t.Fatalf("all=%v: push error = %v, want unsupported manifest-list destination", all, err)
		}
	}
	assertDockerPushSourcesPreserved(t, before, storedImageInventory(t, store))
}

// Native selected-manifest preparation can retain deterministic BigData and a
// temporary alias in name history. Existing records, names and blobs survive.
func assertDockerPushSourcesPreserved(t *testing.T, before, after map[string]storage.Image) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("push changed source record count: %d -> %d", len(before), len(after))
	}
	for id, image := range before {
		current, ok := after[id]
		if !ok || !reflect.DeepEqual(image.Names, current.Names) || image.Digest != current.Digest {
			t.Fatalf("push changed source ID, names or default digest: %+v -> %+v", image, current)
		}
		for key, value := range image.BigDataDigests {
			if current.BigDataDigests[key] != value || current.BigDataSizes[key] != image.BigDataSizes[key] {
				t.Fatalf("push changed existing source blob %s/%s", id, key)
			}
		}
	}
}
