package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/coopr/internal/oci"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

type publicationReadCounter struct {
	oras.ReadOnlyTarget
	payloads atomic.Int64
}

func (s *publicationReadCounter) Fetch(ctx context.Context, descriptor v1.Descriptor) (io.ReadCloser, error) {
	if descriptor.MediaType == v1.MediaTypeImageLayer {
		s.payloads.Add(1)
	}
	return s.ReadOnlyTarget.Fetch(ctx, descriptor)
}

func TestCachePublicationPlanningDefersPayloadReadsAndVerifiesTransfer(t *testing.T) {
	record, sourceLayout := graphFixture(t)
	cacheRoot := t.TempDir()
	local, err := NewLocalStore(t.Context(), cacheRoot, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	root, err := local.PutGraph(t.Context(), record.Root, record, sourceLayout)
	if err != nil {
		t.Fatal(err)
	}
	store, err := orasoci.New(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	tracked := &publicationReadCounter{ReadOnlyTarget: store}
	server, _ := strictEnvelopeRegistry(t, "")
	host := strings.TrimPrefix(server.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := client.Plan(t.Context(), bale.TransferRequest{
		Roots:       []bale.Root{{Repository: host + "/coopr/envelopes", Descriptor: root, Store: newCachePublicationSource(tracked), Referrers: bale.ReferrerPolicy{Include: "none"}}},
		Destination: host,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tracked.payloads.Load() != 0 {
		t.Fatal("planning read image payloads instead of resolving their CAS descriptors")
	}
	// A fresh destination must still verify streamed bytes after planning.
	data, err := fetchVerified(t.Context(), store, record.Results[0].Image, maxImageRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	var image v1.Manifest
	if err := json.Unmarshal(data, &image); err != nil {
		t.Fatal(err)
	}
	layer := image.Layers[0]
	path := filepath.Join(cacheRoot, "blobs", "sha256", layer.Digest.Encoded())
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(layer.Size)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Execute(t.Context(), plan); err == nil {
		t.Fatal("transfer accepted a same-size payload mutation after planning")
	}
	if tracked.payloads.Load() == 0 {
		t.Fatal("failed transfer did not read the changed source payload")
	}
	response, err := http.Head(server.URL + "/v2/coopr/envelopes/manifests/" + root.Digest.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("failed transfer exposed the cache root: status=%d", response.StatusCode)
	}
}

// A manifest registration does not make its bytes available through the blob
// service. Enforce that distinction over the existing loopback registry, as
// GHCR does when image envelopes occur in a cache artifact's layers.
func strictEnvelopeRegistry(t *testing.T, denied digest.Digest) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	backend := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	var mu sync.Mutex
	blobs := map[digest.Digest]bool{}
	var rejected atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v2/coopr/envelopes/")
		if strings.HasPrefix(path, "blobs/sha256:") {
			mu.Lock()
			present := blobs[digest.Digest(strings.TrimPrefix(path, "blobs/"))]
			mu.Unlock()
			if !present {
				w.WriteHeader(http.StatusNotFound)
				return
			}
		}
		upload := digest.Digest(r.URL.Query().Get("digest"))
		if strings.HasPrefix(path, "blobs/uploads/") && upload != "" && upload == denied {
			rejected.Store(true)
			http.Error(w, "denied envelope blob", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPut && strings.HasPrefix(path, "manifests/") {
			data, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
			var manifest v1.Manifest
			if json.Unmarshal(data, &manifest) == nil && manifest.Config.Digest != "" {
				for _, descriptor := range append([]v1.Descriptor{manifest.Config}, manifest.Layers...) {
					mu.Lock()
					present := blobs[descriptor.Digest]
					mu.Unlock()
					if !present {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						_, _ = fmt.Fprintf(w, `{"errors":[{"code":"MANIFEST_BLOB_UNKNOWN","message":"missing blob role for %s"}]}`, descriptor.Digest)
						return
					}
				}
			}
		}
		// Publish role state before the client sees 201; otherwise a concurrent
		// parent PUT could race this test wrapper after a completed blob upload.
		response := httptest.NewRecorder()
		backend.ServeHTTP(response, r)
		if response.Code == http.StatusCreated && strings.HasPrefix(path, "blobs/uploads/") {
			if upload == "" {
				upload = digest.Digest(r.URL.Query().Get("mount"))
			}
			if upload != "" {
				mu.Lock()
				blobs[upload] = true
				mu.Unlock()
			}
		}
		for key, values := range response.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
	}))
	t.Cleanup(server.Close)
	return server, &rejected
}

func TestCacheRegistryPublishesEnvelopeLayerBlobs(t *testing.T) {
	for _, kind := range []string{"graph", "image"} {
		for _, deny := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deny=%t", kind, deny), func(t *testing.T) {
				record, layout := graphFixture(t)
				images := graphImages(record)
				var denied digest.Digest
				if deny {
					denied = images[0].Digest
				}
				server, rejected := strictEnvelopeRegistry(t, denied)
				resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false), RetrySet: true})
				if err != nil {
					t.Fatal(err)
				}
				repository := strings.TrimPrefix(server.URL, "http://") + "/coopr/envelopes"
				writer, err := NewRegistryStore(resolver, repository, t.TempDir(), nil)
				if err != nil {
					t.Fatal(err)
				}
				source, err := orasoci.New(layout)
				if err != nil {
					t.Fatal(err)
				}
				// Existing child manifests must not suppress their incoming blob role.
				for _, image := range images {
					if err := oras.CopyGraph(t.Context(), source, writer.target, image, oras.DefaultCopyGraphOptions); err != nil {
						t.Fatal(err)
					}
				}
				key := ImageKey{Instruction: digest.FromString("instruction"), Parent: digest.FromString("parent"), Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Executor: "buildah-test", Format: "oci"}
				imageRecord := ImageRecord{Version: ImageRecordVersion, Key: key, Image: images[0], BaseImage: &images[1], RootMetadata: json.RawMessage(`{"mode":493}`)}
				publish := func(store *OCIStore) (v1.Descriptor, error) {
					if kind == "graph" {
						return store.PutGraph(t.Context(), record.Root, record, layout)
					}
					return store.PutImage(t.Context(), key, imageRecord, layout)
				}
				local, err := NewLocalStore(t.Context(), t.TempDir(), t.TempDir(), nil)
				if err != nil {
					t.Fatal(err)
				}
				want, err := publish(local)
				if err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadDir(filepath.Join(layout, "blobs", "sha256"))
				if err != nil {
					t.Fatal(err)
				}
				got, err := publish(writer)
				if deny {
					if err == nil || !rejected.Load() {
						t.Fatalf("missing envelope-blob denial: %v", err)
					}
					tag := graphTag(record.Root)
					if kind == "image" {
						keyDigest, _ := key.Digest()
						tag = instructionImageTag(keyDigest)
					}
					if _, err := writer.target.Resolve(t.Context(), tag); !errors.Is(err, errdef.ErrNotFound) {
						t.Fatalf("failed publication exposed a cache tag: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("cache publication rejected missing blob role: %v", err)
				}
				if !content.Equal(got, want) {
					t.Fatal("registry publication changed the local cache format or digest")
				}
				for _, image := range images {
					response, err := http.Get(server.URL + "/v2/coopr/envelopes/blobs/" + image.Digest.String())
					if err != nil {
						t.Fatal(err)
					}
					data, err := io.ReadAll(response.Body)
					_ = response.Body.Close()
					if err != nil || response.StatusCode != http.StatusOK || digest.FromBytes(data) != image.Digest {
						t.Fatalf("image envelope has no exact blob registration: status=%d err=%v", response.StatusCode, err)
					}
				}
				after, err := os.ReadDir(filepath.Join(layout, "blobs", "sha256"))
				if err != nil || len(after) != len(before) {
					t.Fatal("registry publication modified the caller's source layout", err)
				}
				reader, err := NewRegistryStore(resolver, repository, t.TempDir(), nil)
				if err != nil {
					t.Fatal(err)
				}
				var restored string
				if kind == "graph" {
					_, restored, err = reader.LookupGraph(t.Context(), record.Root)
				} else {
					_, restored, err = reader.LookupImage(t.Context(), key)
				}
				if err != nil {
					t.Fatal("independent reader could not restore published cache", err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(restored) })
			})
		}
	}
}
