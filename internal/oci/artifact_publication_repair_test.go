package oci

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	"oras.land/oras-go/v2/content"
)

func TestArtifactRegistryRepublishRepairsMissingChild(t *testing.T) {
	metadata, paths := baleComponentMetadata(t, "amd64", "repair-child")
	pkg := metadata.Packages[0].Descriptor
	backend := registryserver.New()
	var missing atomic.Bool
	var repaired atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if missing.Load() && strings.HasSuffix(request.URL.Path, "/blobs/"+pkg.Digest.String()) && (request.Method == http.MethodHead || request.Method == http.MethodGet) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"errors":[{"code":"BLOB_UNKNOWN","message":"child removed"}]}`)
			return
		}
		if missing.Load() && request.Method == http.MethodPut && request.URL.Query().Get("digest") == pkg.Digest.String() {
			missing.Store(false)
			repaired.Add(1)
		}
		backend.ServeHTTP(w, request)
	}))
	t.Cleanup(server.Close)
	resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	resolver.system.BlobInfoCacheDir = t.TempDir()
	reference := strings.TrimPrefix(server.URL, "http://") + "/coopr/repair:stable"
	for attempt := 0; attempt < 2; attempt++ {
		if attempt == 1 {
			missing.Store(true)
		}
		if _, err := publishBaleRegistryComponent(t, "layout", resolver, reference, metadata, paths); err != nil {
			t.Fatalf("publication %d: %v", attempt, err)
		}
	}
	if missing.Load() || repaired.Load() != 1 {
		t.Fatalf("existing root skipped missing child: missing=%t repaired=%d", missing.Load(), repaired.Load())
	}
	response, err := server.Client().Get(server.URL + "/v2/coopr/repair/blobs/" + pkg.Digest.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if _, err := content.ReadAll(response.Body, pkg); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("repaired package integrity: status=%d error=%v", response.StatusCode, err)
	}
}
