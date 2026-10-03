package build

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/oci"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestRunPushPublishesAndReturnsImmutableReference(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH_REGISTRY") == "" {
		t.Skip("set COOPR_TEST_BUILDAH_REGISTRY for live Buildah registry tests")
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	target := strings.TrimPrefix(server.URL, "http://") + "/coopr/app:stable"
	dir := t.TempDir()
	file := filepath.Join(dir, "app.coopr")
	if err := os.WriteFile(file, []byte("from \"scratch\"\nlabel coopr_test=\"push\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	immutable, err := Run(context.Background(), Options{
		File: file, Platform: "linux/amd64",
		Tag: target, Push: true, PlainHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(immutable, strings.TrimSuffix(target, ":stable")+"@sha256:") {
		t.Fatalf("push result %q is not an immutable reference for %q", immutable, target)
	}

	resolver, err := oci.NewResolver(oci.Options{
		PlainHTTP: true, ImageStoreDir: filepath.Join(t.TempDir(), "empty-image-store"),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(context.Background(), immutable, v1.Platform{OS: "linux", Architecture: "amd64"}, oci.Image)
	if err != nil {
		t.Fatal(err)
	}
	if immutable != resolved.Repository+"@"+resolved.Root.Digest.String() {
		t.Fatalf("push result %q differs from published root %s", immutable, resolved.Root.Digest)
	}
}
