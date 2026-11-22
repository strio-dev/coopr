package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
)

type artifactTrackedBody struct {
	reader        io.Reader
	bytesRead     int64
	reads, closes int
	closeErr      error
}

func (b *artifactTrackedBody) Read(p []byte) (int, error) {
	b.reads++
	n, err := b.reader.Read(p)
	b.bytesRead += int64(n)
	return n, err
}
func (b *artifactTrackedBody) Close() error { b.closes++; return b.closeErr }

func artifactMetadataTarget(t *testing.T, descriptor v1.Descriptor, body *artifactTrackedBody, chunked bool) (*orasArtifactRepository, *int) {
	t.Helper()
	resolver, err := testResolver(t, Options{RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	resolver.system.SystemRegistriesConfPath = writeRegistryConfig(t, "")
	resolver.system.SystemRegistriesConfDirPath = t.TempDir()
	ref, err := ParseReference("registry.example/coopr/metadata:stable")
	if err != nil {
		t.Fatal(err)
	}
	target, err := resolver.repository(ref)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := remote.NewRepository(ref.Registry + "/" + ref.Repository)
	if err != nil {
		t.Fatal(err)
	}
	requests := new(int)
	repository.MaxMetadataBytes = maxMetadataBytes
	repository.Client = &http.Client{Transport: baleCaptureRoundTripper(func(request *http.Request) (*http.Response, error) {
		*requests++
		header := make(http.Header)
		header.Set("Content-Type", descriptor.MediaType)
		header.Set("Docker-Content-Digest", descriptor.Digest.String())
		header.Set("Content-Length", fmt.Sprint(descriptor.Size))
		responseBody := io.ReadCloser(body)
		length := descriptor.Size
		if request.Method == http.MethodHead {
			responseBody = io.NopCloser(strings.NewReader(""))
		} else if chunked {
			length = -1
			header.Del("Content-Length")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: header, ContentLength: length, Body: responseBody, Request: request}, nil
	})}
	target.primary = repository
	return target, requests
}

func TestArtifactRegistryMetadataRejectsAdvertisedSize(t *testing.T) {
	data := append([]byte(`{"schemaVersion":2}`), bytes.Repeat([]byte(" "), maxMetadataBytes)...)
	for _, operation := range []string{"resolve", "fetch-reference", "fetch"} {
		for _, size := range []int64{int64(len(data)), 1 << 40} {
			t.Run(fmt.Sprintf("%s/size=%d", operation, size), func(t *testing.T) {
				descriptor := Descriptor(v1.MediaTypeImageManifest, data)
				descriptor.Size = size
				body := &artifactTrackedBody{reader: bytes.NewReader(data)}
				target, requests := artifactMetadataTarget(t, descriptor, body, false)
				var err error
				switch operation {
				case "resolve":
					_, err = target.Resolve(t.Context(), "stable")
				case "fetch-reference":
					_, _, err = oras.Fetch(t.Context(), target, "stable", oras.DefaultFetchOptions)
				default:
					_, err = target.Fetch(t.Context(), descriptor)
				}
				if err == nil {
					t.Fatal("oversized metadata accepted")
				}
				if body.reads != 0 || body.bytesRead != 0 {
					t.Fatalf("oversized metadata read body: calls=%d bytes=%d", body.reads, body.bytesRead)
				}
				wantRequests, wantCloses := 1, 1
				if operation == "fetch" {
					wantRequests, wantCloses = 0, 0
				}
				if *requests != wantRequests || body.closes != wantCloses {
					t.Fatalf("oversized metadata lifecycle: requests=%d closes=%d; want %d/%d", *requests, body.closes, wantRequests, wantCloses)
				}
			})
		}
	}
}

func TestArtifactRegistryChunkedMetadataRejectsTrailingBytes(t *testing.T) {
	data := []byte(`{"schemaVersion":2}`)
	descriptor := Descriptor(v1.MediaTypeImageManifest, data)
	for _, operation := range []string{"resolve", "fetch-reference", "fetch"} {
		t.Run(operation, func(t *testing.T) {
			body := &artifactTrackedBody{reader: io.MultiReader(bytes.NewReader(data), io.LimitReader(zeroReader{}, 1<<20))}
			target, _ := artifactMetadataTarget(t, descriptor, body, true)
			var err error
			switch operation {
			case "resolve":
				_, err = target.Resolve(t.Context(), "stable")
			case "fetch-reference":
				_, _, err = oras.Fetch(t.Context(), target, "stable", oras.DefaultFetchOptions)
			default:
				_, err = target.Fetch(t.Context(), descriptor)
			}
			if err == nil {
				t.Fatal("trailing metadata accepted")
			}
			if body.bytesRead > descriptor.Size+1 || body.closes != 1 {
				t.Fatalf("metadata read was not bounded/closed: bytes=%d closes=%d", body.bytesRead, body.closes)
			}
		})
	}
}

func TestArtifactRegistryFetchReferenceUsesOneVerifiedRootGET(t *testing.T) {
	child := Descriptor(v1.MediaTypeImageManifest, []byte(`{"schemaVersion":2}`))
	data, err := json.Marshal(VersionedIndex([]v1.Descriptor{child}, "application/vnd.coopr.test.v1"))
	if err != nil {
		t.Fatal(err)
	}
	root := Descriptor(v1.MediaTypeImageIndex, data)
	for _, closeErr := range []error{nil, errors.New("metadata close failed")} {
		body := &artifactTrackedBody{reader: bytes.NewReader(data), closeErr: closeErr}
		target, requests := artifactMetadataTarget(t, root, body, false)
		got, stream, err := oras.Fetch(t.Context(), target, "stable", oras.DefaultFetchOptions)
		if closeErr != nil {
			if !errors.Is(err, closeErr) || stream != nil || target.ResolvedFromPrimary(root) {
				t.Fatalf("failed metadata close admitted content/origin: descriptor=%+v err=%v", got, err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			payload, readErr := content.ReadAll(stream, got)
			if err := errors.Join(readErr, stream.Close()); err != nil || !bytes.Equal(payload, data) || !content.Equal(got, root) {
				t.Fatalf("verified metadata changed: descriptor=%+v err=%v", got, err)
			}
			target.mu.Lock()
			source := target.sources[child.Digest]
			target.mu.Unlock()
			if !target.ResolvedFromPrimary(root) || source.repository != target.primary || len(source.remaining) != 0 {
				t.Fatal("reference fetch lost root or child origin")
			}
		}
		if *requests != 1 || body.closes != 1 || body.bytesRead != int64(len(data)) {
			t.Fatalf("root metadata was fetched/read/closed again: requests=%d bytes=%d closes=%d", *requests, body.bytesRead, body.closes)
		}
	}
}

func TestArtifactRegistryDigestFallbackPreservesTagOrigin(t *testing.T) {
	configData := []byte(`{"record":true}`)
	config := Descriptor("application/vnd.coopr.test.config.v1+json", configData)
	data, err := json.Marshal(VersionedManifest(config, []v1.Descriptor{}, "application/vnd.coopr.test.v1"))
	if err != nil {
		t.Fatal(err)
	}
	root := Descriptor(v1.MediaTypeImageManifest, data)
	serve := func(w http.ResponseWriter, request *http.Request, mirror bool) {
		if request.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		descriptor, payload := root, data
		switch request.URL.Path {
		case "/v2/coopr/metadata/manifests/stable":
			if !mirror {
				http.NotFound(w, request)
				return
			}
		case "/v2/coopr/metadata/manifests/" + root.Digest.String():
			if mirror {
				http.NotFound(w, request)
				return
			}
		case "/v2/coopr/metadata/blobs/" + config.Digest.String():
			if mirror {
				http.NotFound(w, request)
				return
			}
			descriptor, payload = config, configData
		default:
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", descriptor.MediaType)
		w.Header().Set("Docker-Content-Digest", descriptor.Digest.String())
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = w.Write(payload)
	}
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serve(w, r, false) }))
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serve(w, r, true) }))
	t.Cleanup(primary.Close)
	t.Cleanup(mirror.Close)
	host := strings.TrimPrefix(primary.URL, "http://")
	resolver, err := testResolver(t, Options{RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	resolver.system.SystemRegistriesConfPath = writeRegistryConfig(t, fmt.Sprintf("[[registry]]\nlocation = %q\ninsecure = true\n[[registry.mirror]]\nlocation = %q\ninsecure = true\n", host, strings.TrimPrefix(mirror.URL, "http://")))
	resolver.system.SystemRegistriesConfDirPath = t.TempDir()
	ref, err := ParseReference(host + "/coopr/metadata:stable")
	if err != nil {
		t.Fatal(err)
	}
	target, err := resolver.repository(ref)
	if err != nil {
		t.Fatal(err)
	}
	_, stream, err := oras.Fetch(t.Context(), target, "stable", oras.DefaultFetchOptions)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	target.mu.Lock()
	tagOrigin, childOrigin := target.sources[root.Digest], target.sources[config.Digest]
	target.mu.Unlock()
	if tagOrigin.repository == nil || childOrigin.repository != tagOrigin.repository || tagOrigin.repository.Reference.Registry != strings.TrimPrefix(mirror.URL, "http://") {
		t.Fatal("reference fetch did not retain chosen mirror for its child")
	}
	stream, err = target.Fetch(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	target.mu.Lock()
	after, childOrigin := target.sources[root.Digest], target.sources[config.Digest]
	target.mu.Unlock()
	if after.repository != tagOrigin.repository || target.ResolvedFromPrimary(root) || childOrigin.repository.Reference.Registry != host {
		t.Fatal("digest fallback replaced tag origin or lost child fallback endpoint")
	}
	stream, err = target.Fetch(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	payload, readErr := content.ReadAll(stream, config)
	if err := errors.Join(readErr, stream.Close()); err != nil || !bytes.Equal(payload, configData) {
		t.Fatalf("child did not remain available after digest fallback: %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestArtifactRegistryLargeBlobRemainsStreamed(t *testing.T) {
	data := bytes.Repeat([]byte("b"), 16<<20)
	descriptor := Descriptor("application/vnd.coopr.cache.layer.v1+tar", data)
	body := &artifactTrackedBody{reader: bytes.NewReader(data)}
	target, _ := artifactMetadataTarget(t, descriptor, body, false)
	reader, err := target.Fetch(context.Background(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if body.reads != 0 {
		t.Fatal("opening blob eagerly read its body")
	}
	got, err := content.ReadAll(reader, descriptor)
	closeErr := reader.Close()
	if err != nil || closeErr != nil || !bytes.Equal(got, data) {
		t.Fatalf("large blob failed: read=%v close=%v", err, closeErr)
	}
	if body.closes != 1 {
		t.Fatalf("blob close count=%d", body.closes)
	}
}
