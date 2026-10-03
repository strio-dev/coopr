package build

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coopr/internal/transfer"
)

func TestBuildLocalSigningReusesExclusiveStoreLease(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for native local signing build")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	definition := filepath.Join(t.TempDir(), "image.coopr")
	if err := os.WriteFile(definition, []byte("from \"scratch\"\nenv proof=\"signing\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Run(ctx, Options{File: definition, Tag: "localhost/coopr-local-signing:test", Signing: transfer.SigningOptions{SignBy: "missing-test-key"}})
	if err == nil || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || !strings.Contains(err.Error(), "sign stored image") {
		t.Fatalf("local signing should reach the signer without deadlocking nested leases: %v (context %v)", err, ctx.Err())
	}
}

func TestBuildExcludesAmbientGPGKeyringFromCopyDot(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for native signing context test")
	}
	contextDir := t.TempDir()
	home := filepath.Join(contextDir, ".gnupg")
	privateDir := filepath.Join(home, "private-keys-v1.d")
	if err := os.MkdirAll(privateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateDir, "fake.key"), []byte("private signing key"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GNUPGHOME", home)
	definition := filepath.Join(contextDir, "image.coopr")
	if err := os.WriteFile(definition, []byte("from \"scratch\"\ncopy \".\" \"/captured/\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "marker"), []byte("public payload"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer server.Close()
	archive := filepath.Join(t.TempDir(), "image.tar")
	_, err := Run(context.Background(), Options{File: definition, PlainHTTP: true, Tags: []string{"oci-archive:" + archive, "registry:" + strings.TrimPrefix(server.URL, "http://") + "/coopr/signing:latest"}, Signing: transfer.SigningOptions{SignBy: "missing-test-key"}})
	if err == nil {
		t.Fatal("unavailable signed destination succeeded")
	}
	names := archiveLayerNames(t, archive)
	if !containsName(names, "captured/marker") {
		t.Fatalf("public payload missing: %v", names)
	}
	for _, name := range names {
		if strings.Contains(name, ".gnupg") || strings.Contains(name, "fake.key") {
			t.Fatalf("private keyring leaked into %q", name)
		}
	}
}
