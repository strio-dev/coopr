package transfer

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/localstore"
	"coopr/internal/oci"
	registryserver "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	imagecopy "go.podman.io/image/v5/copy"
	imagecli "go.podman.io/image/v5/pkg/cli"
	"go.podman.io/image/v5/signature/sigstore"
)

func TestSigningPreservesNativePasswordBytes(t *testing.T) {
	for _, test := range []struct {
		name, password string
		file           bool
	}{
		{"environment multiline", "first\nsecond", false},
		{"environment carriage return", "first\r", false},
		{"explicit first line", "first\r", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			keys, err := sigstore.GenerateKeyPair([]byte(test.password))
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			key := filepath.Join(dir, "cosign.key")
			public := filepath.Join(dir, "cosign.pub")
			for path, data := range map[string][]byte{key: keys.PrivateKey, public: keys.PublicKey} {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			signing := SigningOptions{SigstorePrivateKeyFile: key}
			t.Setenv("COSIGN_PASSWORD", test.password)
			if test.file {
				signing.PassphraseFile = filepath.Join(dir, "password")
				if err := os.WriteFile(signing.PassphraseFile, []byte(test.password+"\nignored"), 0600); err != nil {
					t.Fatal(err)
				}
				native, err := imagecli.ReadPassphraseFile(signing.PassphraseFile)
				if err != nil || native != test.password {
					t.Fatalf("native password control: %v", err)
				}
				t.Setenv("COSIGN_PASSWORD", "ignored environment")
			}
			native, err := sigstore.NewSigner(sigstore.WithPrivateKeyFile(key, []byte(test.password)))
			if err != nil {
				t.Fatalf("native encrypted-key control: %v", err)
			}
			t.Cleanup(func() {
				if err := native.Close(); err != nil {
					t.Error(err)
				}
			})
			server := httptest.NewServer(registryserver.New())
			defer server.Close()
			source, root := imageFixture(t, ctx)
			layout := t.TempDir()
			if err := localstore.CopyGraphLayout(ctx, layout, source, root); err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{"layout", "stored"} {
				t.Run(mode, func(t *testing.T) {
					if mode == "stored" && testing.Short() {
						t.Skip("requires native storage workers")
					}
					name := strings.TrimPrefix(server.URL, "http://") + "/password/" + mode + ":test"
					opts := Options{TLSVerify: new(false), RetrySet: true, Signing: signing}
					var err error
					if mode == "layout" {
						_, err = CopyRoot(ctx, oci.Image, layout, root, Destination{Transport: "registry", Name: name}, opts)
					} else {
						opts.BuildStore = nativeTestStore(t.TempDir())
						selected := nativeEmptyImageFixture(t, opts.BuildStore, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "password")
						_, err = Copy(ctx, oci.Image, selected.ImageID, Destination{Transport: "registry", Name: name}, opts)
					}
					if err != nil {
						t.Fatalf("sign with native password bytes: %v", err)
					}
					verifySignedReference(t, ctx, name, public, imagecopy.CopyAllImages, true)
				})
			}
		})
	}
}
