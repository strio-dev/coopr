package oci

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
)

type artifactTrackedBody struct {
	reader        io.Reader
	bytesRead     int64
	reads, closes int
}

func (b *artifactTrackedBody) Read(p []byte) (int, error) {
	b.reads++
	n, err := b.reader.Read(p)
	b.bytesRead += int64(n)
	return n, err
}
func (b *artifactTrackedBody) Close() error { b.closes++; return nil }

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
	for _, operation := range []string{"resolve", "fetch"} {
		for _, size := range []int64{int64(len(data)), 1 << 40} {
			t.Run(fmt.Sprintf("%s/size=%d", operation, size), func(t *testing.T) {
				descriptor := Descriptor(v1.MediaTypeImageManifest, data)
				descriptor.Size = size
				body := &artifactTrackedBody{reader: bytes.NewReader(data)}
				target, requests := artifactMetadataTarget(t, descriptor, body, false)
				var err error
				if operation == "resolve" {
					_, err = target.Resolve(t.Context(), "stable")
				} else {
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
	for _, operation := range []string{"resolve", "fetch"} {
		t.Run(operation, func(t *testing.T) {
			body := &artifactTrackedBody{reader: io.MultiReader(bytes.NewReader(data), io.LimitReader(zeroReader{}, 1<<20))}
			target, _ := artifactMetadataTarget(t, descriptor, body, true)
			var err error
			if operation == "resolve" {
				_, err = target.Resolve(t.Context(), "stable")
			} else {
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
