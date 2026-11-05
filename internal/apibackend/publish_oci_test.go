package apibackend

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/api"
)

func publishOCIFixture(t *testing.T) (api.Request, v1.Descriptor, []byte) {
	t.Helper()
	r := request(t)
	r.Kind, r.Program, r.Options.Output = "publish", nil, api.Output{}
	dir := filepath.Join(filepath.Dir(r.Options.ContextDir), "original-layout")
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0700); err != nil {
		t.Fatal(err)
	}
	put := func(media string, data []byte) v1.Descriptor {
		d := v1.Descriptor{MediaType: media, Digest: digest.FromBytes(data), Size: int64(len(data))}
		if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", d.Digest.Encoded()), data, 0600); err != nil {
			t.Fatal(err)
		}
		return d
	}
	var plain, compressed bytes.Buffer
	tarWriter := tar.NewWriter(&plain)
	payload := []byte("exact original layer\n")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "original", Mode: 0644, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := gzipWriter.Write(plain.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	layer := put(v1.MediaTypeImageLayerGzip, compressed.Bytes())
	configData, err := json.Marshal(map[string]any{"architecture": "amd64", "os": "linux", "rootfs": map[string]any{"type": "layers", "diff_ids": []digest.Digest{digest.FromBytes(plain.Bytes())}}, "config": map[string]any{"Env": []string{"EXACT=original"}}})
	if err != nil {
		t.Fatal(err)
	}
	config := put(v1.MediaTypeImageConfig, configData)
	manifest, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{layer}})
	if err != nil {
		t.Fatal(err)
	}
	root := put(v1.MediaTypeImageManifest, manifest)
	index, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []v1.Descriptor{root}})
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{v1.ImageLayoutFile: []byte(`{"imageLayoutVersion":"1.0.0"}`), v1.ImageIndexFile: index} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	r.Publish = &api.Publish{Artifact: "image", Source: "oci:" + dir, SourceDigest: root.Digest.String(), Destinations: []string{"registry.example/team/image:tested"}}
	return r, root, manifest
}

func TestPublishOCILayoutValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*testing.T, *api.Request)
	}{
		{"missing digest", func(_ *testing.T, r *api.Request) { r.Publish.SourceDigest = "" }},
		{"wrong digest", func(_ *testing.T, r *api.Request) { r.Publish.SourceDigest = digest.FromString("other").String() }},
		{"relative path", func(_ *testing.T, r *api.Request) { r.Publish.Source = "oci:relative" }},
		{"component", func(_ *testing.T, r *api.Request) { r.Publish.Artifact = "component" }},
		{"digest with native source", func(_ *testing.T, r *api.Request) { r.Publish.Source = "native-image" }},
		{"source overlaps store", func(_ *testing.T, r *api.Request) {
			r.Options.Store.GraphRoot = strings.TrimPrefix(r.Publish.Source, "oci:")
		}},
		{"destination overlaps source", func(_ *testing.T, r *api.Request) { r.Publish.Destinations = []string{r.Publish.Source + "/delivery"} }},
		{"ambiguous roots", func(t *testing.T, r *api.Request) {
			file := filepath.Join(strings.TrimPrefix(r.Publish.Source, "oci:"), v1.ImageIndexFile)
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var index v1.Index
			if err := json.Unmarshal(data, &index); err != nil {
				t.Fatal(err)
			}
			index.Manifests = append(index.Manifests, index.Manifests[0])
			data, err = json.Marshal(index)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"corrupt manifest", func(t *testing.T, r *api.Request) {
			file := filepath.Join(strings.TrimPrefix(r.Publish.Source, "oci:"), "blobs", "sha256", strings.TrimPrefix(r.Publish.SourceDigest, "sha256:"))
			if err := os.WriteFile(file, []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink metadata", func(t *testing.T, r *api.Request) {
			dir := strings.TrimPrefix(r.Publish.Source, "oci:")
			file := filepath.Join(dir, v1.ImageIndexFile)
			if err := os.Rename(file, file+".original"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(file+".original", file); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversized metadata", func(t *testing.T, r *api.Request) {
			file := filepath.Join(strings.TrimPrefix(r.Publish.Source, "oci:"), v1.ImageIndexFile)
			if err := os.Truncate(file, api.MaxEventBytes+1); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _ := publishOCIFixture(t)
			tc.edit(t, &r)
			_, err := Execute(t.Context(), r, io.Discard, io.Discard)
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != "invalid_request" {
				t.Fatalf("error=%v", err)
			}
			if _, err := os.Stat(r.Options.Store.RunRoot); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid source created native runroot: %v", err)
			}
		})
	}
	r, root, _ := publishOCIFixture(t)
	if err := validate(r); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := publishOCISource(ctx, r.Publish); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled selection: %v", err)
	}
	if _, got, err := publishOCISource(t.Context(), r.Publish); err != nil || got.Digest != root.Digest {
		t.Fatalf("selected=%+v err=%v", got, err)
	}
}

func TestPublishOCILayoutPreservesRegistryManifest(t *testing.T) {
	r, root, manifest := publishOCIFixture(t)
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	r.Publish.Destinations = []string{strings.TrimPrefix(server.URL, "http://") + "/exact:tested"}
	tls := false
	r.Options.Registry.TLSVerify = &tls
	policy := filepath.Join(filepath.Dir(r.Options.ContextDir), "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	r.Options.Registry.SignaturePolicyPath = policy
	result, err := Execute(t.Context(), r, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Destinations) != 1 || result.Destinations[0].Status != "passed" || !strings.HasSuffix(result.Destinations[0].Reference, "@"+root.Digest.String()) {
		t.Fatalf("publication=%+v", result)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/v2/exact/manifests/tested", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", v1.MediaTypeImageManifest)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	got, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || !bytes.Equal(got, manifest) {
		t.Fatalf("manifest changed: status=%d err=%v body=%s", response.StatusCode, err, got)
	}
	for _, path := range []string{r.Options.Store.GraphRoot, r.Options.Store.RunRoot} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("publication used native store %s: %v", path, err)
		}
	}
}
