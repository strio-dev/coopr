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

	"coopr/internal/oci"
	"coopr/internal/transfer"
)

func TestBuildLocalSigningReusesExclusiveStoreLease(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native build integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	definition := filepath.Join(t.TempDir(), "image.coopr")
	if err := os.WriteFile(definition, []byte(`from "scratch" {
    env proof="signing"
}
`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Run(ctx, Options{File: definition, Tag: "localhost/coopr-local-signing:test", Signing: transfer.SigningOptions{SignBy: "missing-test-key"}})
	if err == nil || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || !strings.Contains(err.Error(), "sign stored image") {
		t.Fatalf("local signing should reach the signer without deadlocking nested leases: %v (context %v)", err, ctx.Err())
	}
}

func TestBuildExcludesAmbientGPGKeyringFromCopyDot(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native build integration")
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
	if err := os.WriteFile(definition, []byte(`from "scratch" {
    copy "." "/captured/"
}
`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "marker"), []byte("public payload"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer server.Close()
	archive := filepath.Join(t.TempDir(), "image.tar")
	iid := filepath.Join(t.TempDir(), "iid")
	store := nativeBuildTestStore(filepath.Join(t.TempDir(), "images"))
	_, err := Run(context.Background(), Options{File: definition, TLSVerify: new(false), Push: true, Tag: strings.TrimPrefix(server.URL, "http://") + "/coopr/signing:latest", IIDFileRaw: iid, BuildStore: store, Signing: transfer.SigningOptions{SignBy: "missing-test-key"}})
	if err == nil {
		t.Fatal("unavailable signed destination succeeded")
	}
	imageID, readErr := os.ReadFile(iid)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if _, err := transfer.Copy(context.Background(), oci.Image, strings.TrimSpace(string(imageID)), transfer.Destination{Transport: "oci-archive", Name: archive}, transfer.Options{BuildStore: store}); err != nil {
		t.Fatal(err)
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
