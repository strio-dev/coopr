package transfer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/oci"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/signature/sigstore"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func TestStoredIndexRegistryRetainsRootSignature(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers and registry signing")
	}
	ctx := context.Background()
	st := nativeTestStore(t.TempDir())
	root, _, _ := signedIndexFixture(t, st, "reaudit")
	err := buildah.WithStore(st, func(backend storage.Store) error {
		image, err := backend.Image("localhost/index:latest")
		if err != nil {
			return err
		}
		previous, err := oci.CaptureStoredSignatures(backend, image.ID)
		if err != nil {
			return err
		}
		sig := []byte("\x00sigstore-json\n{\"mimeType\":\"application/vnd.dev.cosign.simplesigning.v1+json\",\"payload\":\"cGF5bG9hZA==\",\"annotations\":{\"coopr.test\":\"root-proof\"}}")
		return oci.WriteStoredManifestSignatures(backend, image.ID, root.Digest, true, [][]byte{sig}, previous)
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	opts := Options{BuildStore: st, TLSVerify: new(false)}
	_, err = Copy(ctx, oci.Image, "index:latest", Destination{Transport: "registry", Name: host + "/copy:latest"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := http.Get(server.URL + "/v2/copy/manifests/sha256-" + root.Digest.Encoded() + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := copied.Body.Close(); err != nil {
			t.Errorf("close attachment response: %v", err)
		}
	}()
	stage := t.TempDir() + "/signed-dir"
	_, err = Copy(ctx, oci.Image, "index:latest", Destination{Transport: "dir", Name: stage}, opts)
	if err != nil {
		t.Fatal(err)
	}
	src, err := alltransports.ParseImageName("dir:" + stage)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := alltransports.ParseImageName("docker://" + host + "/native:latest")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := policy.Destroy(); err != nil {
			t.Errorf("destroy native policy: %v", err)
		}
	}()
	registriesDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(registriesDir, "default.yaml"), []byte("default-docker:\n  use-sigstore-attachments: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = imagecopy.Image(ctx, policy, dst, src, &imagecopy.Options{ImageListSelection: imagecopy.CopyAllImages, DestinationCtx: &types.SystemContext{RegistriesDirPath: registriesDir, DockerInsecureSkipTLSVerify: types.OptionalBoolTrue}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := http.Get(server.URL + "/v2/native/manifests/sha256-" + root.Digest.Encoded() + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := native.Body.Close(); err != nil {
			t.Errorf("close attachment response: %v", err)
		}
	}()
	if native.StatusCode != http.StatusOK {
		t.Fatalf("native control missing root attachment: HTTP %d", native.StatusCode)
	}
	t.Logf("copy root signature HTTP=%d; native imagecopy.Image root signature HTTP=%d", copied.StatusCode, native.StatusCode)
	if copied.StatusCode != http.StatusOK {
		t.Fatalf("Copy silently lost existing index-root signature: HTTP %d (native control %d)", copied.StatusCode, native.StatusCode)
	}
}

func TestStoredIndexRegistryRetainsVerifiableSignatures(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers and registry signing")
	}
	ctx := context.Background()
	store := nativeTestStore(t.TempDir())
	root, _, selections := signedIndexFixture(t, store, "real-key")
	server := httptest.NewServer(registry.New())
	defer server.Close()
	name := strings.TrimPrefix(server.URL, "http://") + "/retained:latest"
	passphrase := []byte("private test key")
	keys, err := sigstore.GenerateKeyPair(passphrase)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, publicKey := filepath.Join(t.TempDir(), "key"), filepath.Join(t.TempDir(), "key.pub")
	for path, data := range map[string][]byte{privateKey: keys.PrivateKey, publicKey: keys.PublicKey} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	unsigned := filepath.Join(t.TempDir(), "unsigned")
	if _, err := Copy(ctx, oci.Image, "index:latest", Destination{Transport: "dir", Name: unsigned}, Options{BuildStore: store}); err != nil {
		t.Fatal(err)
	}
	signed := filepath.Join(t.TempDir(), "signed")
	src, err := alltransports.ParseImageName("dir:" + unsigned)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := alltransports.ParseImageName("dir:" + signed)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := reference.ParseNormalizedNamed(name)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := policy.Destroy(); err != nil {
			t.Errorf("destroy native policy: %v", err)
		}
	}()
	if _, err := imagecopy.Image(ctx, policy, dst, src, &imagecopy.Options{ImageListSelection: imagecopy.CopyAllImages, SignIdentity: identity, SignBySigstorePrivateKeyFile: privateKey, SignSigstorePrivateKeyPassphrase: passphrase}); err != nil {
		t.Fatal(err)
	}
	// Seed signatures generated by the public copier into the real native
	// records. The subsequent registry copy must retain them without a signer.
	if err := buildah.WithStore(store, func(backend storage.Store) error {
		image, err := backend.Image("localhost/index:latest")
		if err != nil {
			return err
		}
		ids := map[digest.Digest]string{root.Digest: image.ID}
		for _, selection := range selections {
			ids[selection.Manifest.Digest] = selection.ImageID
		}
		for selected, id := range ids {
			prefix := selected.Encoded() + "."
			if selected == root.Digest {
				prefix = ""
			}
			blob, err := os.ReadFile(filepath.Join(signed, prefix+"signature-1"))
			if err != nil {
				return err
			}
			previous, err := oci.CaptureStoredSignatures(backend, id)
			if err != nil {
				return err
			}
			if err := oci.WriteStoredManifestSignatures(backend, id, selected, true, [][]byte{blob}, previous); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(ctx, oci.Image, "index:latest", Destination{Transport: "registry", Name: name}, Options{BuildStore: store, TLSVerify: new(false)}); err != nil {
		t.Fatal(err)
	}
	verifySignedReference(t, ctx, name, publicKey, imagecopy.CopyAllImages, true)
	for _, selection := range selections {
		verifySignedReference(t, ctx, strings.TrimSuffix(name, ":latest")+"@"+selection.Manifest.Digest.String(), publicKey, imagecopy.CopySystemImage, true)
	}
	assertRemoteManifestDigests(t, ctx, name, root, nil)
	// A transformation of an already signed root requires explicit removal.
	options := Options{BuildStore: store, TLSVerify: new(false), Push: PushOptions{Format: "docker"}}
	if _, err := Copy(ctx, oci.Image, "index:latest", Destination{Transport: "registry", Name: name}, options); err == nil || !strings.Contains(strings.ToLower(err.Error()), "sign") {
		t.Fatalf("signed-root rewrite did not fail for signatures: %v", err)
	}
	options.Push.RemoveSignatures = true
	removed := strings.TrimSuffix(name, ":latest") + ":removed"
	if _, err := Copy(ctx, oci.Image, "index:latest", Destination{Transport: "registry", Name: removed}, options); err != nil {
		t.Fatal(err)
	}
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := resolver.RemoteImageManifest(ctx, removed)
	if err != nil {
		t.Fatal(err)
	}
	if oci.Descriptor(v1.MediaTypeImageIndex, raw).Digest == root.Digest {
		t.Fatal("explicit removal did not transform root")
	}
}
