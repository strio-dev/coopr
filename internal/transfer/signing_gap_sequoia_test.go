package transfer

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/imagestore"
	"github.com/strio-dev/coopr/internal/localstore"
	"github.com/strio-dev/coopr/internal/oci"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/signature/signer"
	"go.podman.io/image/v5/signature/simplesequoia"
	"go.podman.io/image/v5/types"
)

// Run both with the ordinary optional helper and with containers_image_sequoia.
// The latter compares Coopr with the public native copier in the same process.
func TestSigningGapProtectedSequoiaGraph(t *testing.T) {
	if testing.Short() {
		t.Skip("real optional signing backend")
	}
	if _, err := exec.LookPath("sq"); err != nil {
		t.Skip("optional sq is not installed")
	}
	if !buildah.NativeSequoia {
		if _, err := exec.LookPath(buildah.SequoiaHelperName); err != nil {
			t.Skip("optional signing helper is not installed")
		}
		if _, err := buildah.FindSequoiaHelper(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	configureTestSignatureLookaside(t, t.TempDir())
	for _, protected := range []bool{false, true} {
		label := "unprotected"
		if protected {
			label = "protected"
		}
		for _, graph := range []string{"single", "index"} {
			backends := []string{"coopr"}
			if buildah.NativeSequoia {
				backends = append(backends, "native")
			}
			for _, backend := range backends {
				t.Run(label+"/"+graph+"/"+backend, func(t *testing.T) {
					sequoiaAcceptanceHome(t)
					key := sequoiaKeyFixture(t, ctx, protected)
					source, root := imageFixture(t, ctx)
					var members []v1.Descriptor
					if graph == "index" {
						source, root, members = multiPlatformImageFixture(t, ctx)
					}
					store := t.TempDir()
					if err := localstore.Put(ctx, store, source, root, "signing-gap:latest"); err != nil {
						t.Fatal(err)
					}
					server := httptest.NewServer(registry.New())
					defer server.Close()
					name := strings.TrimPrefix(server.URL, "http://") + "/coopr/protected:test"
					var signingErr error
					if backend == "coopr" {
						_, signingErr = CopyRoot(ctx, oci.Image, store, root, Destination{Transport: "registry", Name: name}, Options{TLSVerify: new(false), Signing: SigningOptions{SequoiaFingerprint: key.fingerprint, PassphraseFile: key.password}})
					} else {
						src, err := imagestore.LayoutReference(store, root)
						if err != nil {
							t.Fatal(err)
						}
						dst, err := docker.ParseReference("//" + name)
						if err != nil {
							t.Fatal(err)
						}
						var password []byte
						if key.password != "" {
							password, err = os.ReadFile(key.password)
							if err != nil {
								t.Fatal(err)
							}
						}
						signing, err := simplesequoia.NewSigner(simplesequoia.WithKeyFingerprint(key.fingerprint), simplesequoia.WithPassphrase(string(password)))
						if err != nil {
							t.Fatal(err)
						}
						defer func() {
							if err := signing.Close(); err != nil {
								t.Error(err)
							}
						}()
						policy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}})
						if err != nil {
							t.Fatal(err)
						}
						defer func() {
							if err := policy.Destroy(); err != nil {
								t.Error(err)
							}
						}()
						_, signingErr = imagecopy.Image(ctx, policy, dst, src, &imagecopy.Options{ImageListSelection: imagecopy.CopyAllImages, Signers: []*signer.Signer{signing}, DestinationCtx: &types.SystemContext{DockerInsecureSkipTLSVerify: types.OptionalBoolTrue}})
					}
					if protected && graph == "index" {
						// podman-sequoia 0.4.0 attempts to unlock an already-unlocked device
						// for the second graph member. Keep this native limitation explicit.
						if signingErr == nil || !strings.Contains(signingErr.Error(), "copying image 2/2") || !strings.Contains(signingErr.Error(), "Device is already unlocked") {
							t.Fatalf("protected graph native limitation changed: %v", signingErr)
						}
						t.Logf("confirmed native protected graph limitation: %v", signingErr)
						return
					}
					if signingErr != nil {
						t.Fatal(signingErr)
					}
					assertSequoiaSignedReference(t, ctx, name, key, members)
				})
			}
		}
	}
}
