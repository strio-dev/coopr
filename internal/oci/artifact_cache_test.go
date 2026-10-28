package oci

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/pkg/blobinfocache"
	"go.podman.io/image/v5/types"
	"oras.land/oras-go/v2/content"
)

func TestArtifactRegistryInvalidCacheLocationsFallBack(t *testing.T) {
	var foreignRequests, staleRequests, uploads, writes atomic.Int64
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignRequests.Add(1)
		http.Error(w, "must not contact cached foreign authority", http.StatusInternalServerError)
	}))
	t.Cleanup(foreign.Close)
	backend := registryserver.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut || r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
			writes.Add(1)
		}
		if strings.HasPrefix(r.URL.Path, "/v2/coopr/absent/blobs/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"code":"BLOB_UNKNOWN","message":"blob not linked to repository"}]}`))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v2/coopr/stale/blobs/") {
			staleRequests.Add(1)
		}
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/uploads/") {
			uploads.Add(1)
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), Credentials: "private:credential", RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("verified artifact payload after stale native location hints")
	descriptor := Descriptor(ComponentPackageType, data)
	cache := blobinfocache.DefaultCache(resolver.SystemContext())
	scope := types.BICTransportScope{Opaque: host}
	for _, location := range []string{
		"@invalid",
		strings.TrimPrefix(foreign.URL, "http://") + "/coopr/stolen",
		host + "/coopr/stale",
	} {
		cache.RecordKnownLocation(docker.Transport, scope, descriptor.Digest, types.BICLocationReference{Opaque: location})
	}
	target, err := resolver.CacheRepository(host + "/coopr/target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Push(t.Context(), descriptor, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if foreignRequests.Load() != 0 || staleRequests.Load() == 0 || uploads.Load() != 1 {
		t.Fatalf("candidate handling: foreign=%d stale=%d payloadUploads=%d", foreignRequests.Load(), staleRequests.Load(), uploads.Load())
	}
	reader, err := target.Fetch(t.Context(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := content.ReadAll(reader, descriptor)
	closeErr := reader.Close()
	if err != nil || closeErr != nil || !bytes.Equal(verified, data) {
		t.Fatalf("fallback integrity: read=%v close=%v", err, closeErr)
	}
	var records int
	for _, candidate := range cache.CandidateLocations(docker.Transport, scope, descriptor.Digest, false) {
		if candidate.Location.Opaque == host+"/coopr/target" {
			records++
		}
	}
	if records != 1 {
		t.Fatalf("verified upload location was not recorded: %d", records)
	}
	// No-payload writes on Exists: a missing repository remains a read-only miss.
	before := writes.Load()
	absent, err := resolver.CacheRepository(host + "/coopr/absent")
	if err != nil {
		t.Fatal(err)
	}
	if exists, err := absent.Exists(t.Context(), descriptor); err != nil || exists {
		t.Fatalf("read-only absence: exists=%t error=%v", exists, err)
	}
	if uploads.Load() != 1 || writes.Load() != before {
		t.Fatal("Exists caused a registry write")
	}
}

func TestArtifactRegistryCacheProbeCancellation(t *testing.T) {
	backend := registryserver.New()
	entered := make(chan struct{})
	var writes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPut || request.Method == http.MethodPost || request.Method == http.MethodPatch {
			writes.Add(1)
		}
		if request.Method == http.MethodHead && strings.HasPrefix(request.URL.Path, "/v2/coopr/candidate/blobs/") {
			close(entered)
			<-request.Context().Done()
			return
		}
		backend.ServeHTTP(w, request)
	}))
	t.Cleanup(server.Close)
	resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true, Retry: 1})
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(server.URL, "http://")
	data := []byte("cancel candidate lookup before payload transfer")
	descriptor := Descriptor(ComponentPackageType, data)
	cache := blobinfocache.DefaultCache(resolver.SystemContext())
	cache.RecordKnownLocation(docker.Transport, types.BICTransportScope{Opaque: host}, descriptor.Digest, types.BICLocationReference{Opaque: host + "/coopr/candidate"})
	target, err := resolver.CacheRepository(host + "/coopr/destination")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- target.Push(ctx, descriptor, bytes.NewReader(data)) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("cached candidate was not probed")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("candidate cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("candidate cancellation did not finish")
	}
	if writes.Load() != 0 {
		t.Fatalf("canceled candidate caused registry writes: %d", writes.Load())
	}
}
