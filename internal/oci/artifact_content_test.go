package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
)

func TestArtifactContentNamespaces(t *testing.T) {
	ctx := context.Background()
	registry := registryserver.New()
	var writes atomic.Int64
	var corrupt atomic.Bool
	var envelope v1.Descriptor
	_, _, resolver, name := testRepository(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writes.Add(1)
		}
		// The in-process registry shares blobs globally. Keep this repository
		// empty so a cached source location cannot turn a lookup into a mount.
		if strings.HasPrefix(r.URL.Path, "/v2/coopr/test-other/blobs/") && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			http.NotFound(w, r)
			return
		}
		if corrupt.Load() && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/blobs/"+envelope.Digest.String()) {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(bytes.Repeat([]byte("x"), int(envelope.Size)))
			return
		}
		registry.ServeHTTP(w, r)
	}))
	meta, paths := publicationMetadata(t, nil, false)
	subject, err := resolver.PublishComponent(ctx, name+":subject", meta, paths)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := ParseReference(name + ":not-published")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := resolver.orasRepository(ctx, ref.Registry+"/"+ref.Repository, nil, resolver.SystemContext(), 0)
	if err != nil {
		t.Fatal(err)
	}
	blobs := repository.Blobs()
	if _, err := repository.Resolve(ctx, subject.Digest.String()); err != nil {
		t.Fatal(err)
	}

	// A manifest's digest alone does not make its bytes a blob in that repository.
	before := writes.Load()
	if _, err := blobs.Resolve(ctx, subject.Digest.String()); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("manifest-only blob resolution: %v", err)
	}
	if exists, err := blobs.Exists(ctx, subject); err != nil || exists {
		t.Fatalf("manifest-only blob exists: %v, %v", exists, err)
	}
	if writes.Load() != before {
		t.Fatal("blob lookup wrote registry content")
	}

	configData := []byte(`{}`)
	config := Descriptor(v1.MediaTypeEmptyJSON, configData)
	if err := blobs.Push(ctx, config, bytes.NewReader(configData)); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(VersionedManifest(config, nil, "application/vnd.example.artifact.v1"))
	if err != nil {
		t.Fatal(err)
	}
	envelope = Descriptor(v1.MediaTypeImageManifest, data)
	if err := blobs.Push(ctx, envelope, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	before = writes.Load()
	resolved, err := blobs.Resolve(ctx, envelope.Digest.String())
	if err != nil || resolved.Digest != envelope.Digest || resolved.Size != envelope.Size {
		t.Fatalf("blob resolve: %+v, %v", resolved, err)
	}
	if exists, err := blobs.Exists(ctx, envelope); err != nil || !exists {
		t.Fatalf("envelope blob exists: %v, %v", exists, err)
	}
	if writes.Load() != before {
		t.Fatal("blob lookup wrote registry content")
	}
	if got, err := content.FetchAll(ctx, blobs, envelope); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("envelope blob fetch: %q, %v", got, err)
	}
	otherRef, err := ParseReference(name + "-other:unused")
	if err != nil {
		t.Fatal(err)
	}
	other, err := resolver.orasRepository(ctx, otherRef.Registry+"/"+otherRef.Repository, nil, resolver.SystemContext(), 0)
	if err != nil {
		t.Fatal(err)
	}
	otherBlobs := other.Blobs()
	before = writes.Load()
	if _, err := otherBlobs.Resolve(ctx, envelope.Digest.String()); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("blob absent from second repository: %v", err)
	}
	if exists, err := otherBlobs.Exists(ctx, envelope); err != nil || exists {
		t.Fatalf("blob was reused from another repository: %v, %v", exists, err)
	}
	if writes.Load() != before {
		t.Fatal("blob lookup mounted content from another repository")
	}
	if _, err := repository.Resolve(ctx, envelope.Digest.String()); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("blob-only manifest resolution: %v", err)
	}
	if _, err := repository.Fetch(ctx, envelope); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("blob-only manifest fetch: %v", err)
	}

	if err := repository.Push(ctx, envelope, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := repository.Tag(ctx, envelope, "attachment"); err != nil {
		t.Fatal(err)
	}
	if got, err := repository.Resolve(ctx, "attachment"); err != nil || got.Digest != envelope.Digest {
		t.Fatalf("manifest resolve: %+v, %v", got, err)
	}
	for kind, store := range map[string]content.ReadOnlyStorage{"manifest": repository, "blob": blobs} {
		if got, err := content.FetchAll(ctx, store, envelope); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%s fetch after both publications: %q, %v", kind, got, err)
		}
	}

	missing := Descriptor(v1.MediaTypeImageManifest, []byte("missing blob"))
	if exists, err := blobs.Exists(ctx, missing); err != nil || exists {
		t.Fatalf("missing blob exists: %v, %v", exists, err)
	}
	if _, err := blobs.Resolve(ctx, missing.Digest.String()); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("missing blob resolve: %v", err)
	}
	if _, err := blobs.Fetch(ctx, missing); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("missing blob fetch: %v", err)
	}
	corrupt.Store(true)
	if _, err := content.FetchAll(ctx, blobs, envelope); !errors.Is(err, content.ErrMismatchedDigest) {
		t.Fatalf("corrupt blob was not rejected: %v", err)
	}
}
