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
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/localstore"
	"github.com/strio-dev/coopr/internal/oci"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
	"oras.land/oras-go/v2/content"
)

func TestCopyRootDockerReturnsDestinationManifestDigest(t *testing.T) {
	for _, test := range []struct {
		media  string
		format string
	}{{v1.MediaTypeImageManifest, ""}, {v1.MediaTypeImageManifest, "v2s2"}, {manifest.DockerV2Schema2MediaType, ""}, {v1.MediaTypeImageIndex, ""}} {
		t.Run(test.media+"/format="+test.format, func(t *testing.T) {
			media := test.media
			ctx := context.Background()
			source, root := imageFixture(t, ctx)
			push := func(media string, data []byte) v1.Descriptor {
				desc := oci.Descriptor(media, data)
				if err := source.Push(ctx, desc, bytes.NewReader(data)); err != nil {
					t.Fatal(err)
				}
				return desc
			}
			if media == manifest.DockerV2Schema2MediaType {
				data, err := content.FetchAll(ctx, source, root)
				if err != nil {
					t.Fatal(err)
				}
				var image v1.Manifest
				if err := json.Unmarshal(data, &image); err != nil {
					t.Fatal(err)
				}
				data, err = json.Marshal(manifest.Schema2{SchemaVersion: 2, MediaType: media, ConfigDescriptor: manifest.Schema2Descriptor{MediaType: manifest.DockerV2Schema2ConfigMediaType, Size: image.Config.Size, Digest: image.Config.Digest}, LayersDescriptors: []manifest.Schema2Descriptor{}})
				if err != nil {
					t.Fatal(err)
				}
				root = push(media, data)
			}
			child := root
			if media == v1.MediaTypeImageIndex {
				child.Platform = &v1.Platform{OS: "linux", Architecture: "amd64"}
				data, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: media, Manifests: []v1.Descriptor{child}})
				if err != nil {
					t.Fatal(err)
				}
				root = push(media, data)
			}
			layout := filepath.Join(t.TempDir(), "layout")
			if err := localstore.Put(ctx, layout, source, root); err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			policyDir := filepath.Join(home, ".config", "containers")
			if err := os.MkdirAll(policyDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(policyDir, "policy.json"), []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			var loaded []byte
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodHead && r.URL.Path == "/_ping":
					w.Header().Set("API-Version", "1.48")
					w.Header().Set("OSType", "linux")
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info"):
					_ = json.NewEncoder(w).Encode(map[string]any{"DriverStatus": [][2]string{{"driver-type", "io.containerd.snapshotter.v1"}}})
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images/load"):
					data, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("read Docker load: %v", err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					loaded = data
					_, _ = io.WriteString(w, "{}\n")
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
					_ = json.NewEncoder(w).Encode(map[string]any{"Descriptor": root, "Manifests": []map[string]any{{"Descriptor": child, "Available": true}}})
				default:
					t.Errorf("unexpected Docker request %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			if err := server.Listener.Close(); err != nil {
				t.Fatal(err)
			}
			socketDir, err := os.MkdirTemp("/tmp", "coopr-digest-")
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
			destination := Destination{Transport: "docker", Name: "example:dev"}
			var progress bytes.Buffer
			opts := Options{Push: PushOptions{Format: test.format}, ProgressWriter: &progress}
			rejectPath := filepath.Join(t.TempDir(), "reject.json")
			if err := os.WriteFile(rejectPath, []byte(`{"default":[{"type":"reject"}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			var rejectedBody []byte
			rejectionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodHead && r.URL.Path == "/_ping":
					w.Header().Set("API-Version", "1.48")
					w.Header().Set("OSType", "linux")
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images/load"):
					var err error
					rejectedBody, err = io.ReadAll(r.Body)
					if err != nil {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					_, _ = io.WriteString(w, "{}\n")
				default:
					t.Errorf("unexpected rejection request %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(rejectionServer.Close)
			t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(rejectionServer.URL, "http://"))
			rejected := opts
			rejected.SignaturePolicyPath = rejectPath
			_, rejectErr := CopyRoot(ctx, oci.Image, layout, root, destination, rejected)
			// Close waits for handlers, including an aborted load, before
			// checking all received bytes and restoring the success server.
			rejectionServer.Close()
			if rejectErr == nil || !strings.Contains(rejectErr.Error(), "rejected by policy") {
				t.Fatalf("copy rejection = %v, want signature-policy rejection", rejectErr)
			}
			if len(rejectedBody) != 0 {
				t.Fatalf("rejected image sent %d bytes to Docker", len(rejectedBody))
			}
			t.Setenv("DOCKER_HOST", "unix://"+socket)
			result, err := CopyRoot(ctx, oci.Image, layout, root, destination, opts)
			server.Close()
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded) == 0 {
				t.Fatal("Docker received no archive")
			}
			want := root.Digest
			if media != v1.MediaTypeImageIndex {
				archive := filepath.Join(t.TempDir(), "loaded.tar")
				if err := os.WriteFile(archive, loaded, 0600); err != nil {
					t.Fatal(err)
				}
				ref, err := alltransports.ParseImageName("docker-archive:" + archive + ":docker.io/library/example:dev")
				if err != nil {
					t.Fatal(err)
				}
				saved, err := ref.NewImageSource(ctx, &types.SystemContext{})
				if err != nil {
					t.Fatal(err)
				}
				data, mime, err := saved.GetManifest(ctx, nil)
				closeErr := saved.Close()
				if err != nil {
					t.Fatal(err)
				}
				if closeErr != nil {
					t.Fatal(closeErr)
				}
				if mime != manifest.DockerV2Schema2MediaType {
					t.Fatalf("destination media type = %s", mime)
				}
				want = digest.FromBytes(data)
				if !strings.Contains(progress.String(), "Writing manifest") {
					t.Fatalf("missing copy progress: %s", &progress)
				}
			}
			if got := result[strings.LastIndex(result, "@")+1:]; got != want.String() {
				t.Fatalf("returned digest = %s, destination manifest = %s, source = %s", got, want, root.Digest)
			}
			if matches, err := filepath.Glob(filepath.Join(layout, "*.tar")); err != nil || len(matches) != 0 {
				t.Fatalf("copy persisted an archive: %v, %v", matches, err)
			}
			if media == v1.MediaTypeImageManifest && want == root.Digest {
				t.Fatal("OCI source was not converted")
			}
			if media != v1.MediaTypeImageManifest && want != root.Digest {
				t.Fatal("unchanged manifest/index digest changed")
			}
		})
	}
}
