package build

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/transfer"

	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/signature/sigstore"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func TestBuildPushWithParameterSignerExcludesCredentials(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native build and signing workers")
	}
	ctx := context.Background()
	root := t.TempDir()
	keys, err := sigstore.GenerateKeyPair([]byte("build signing password"))
	if err != nil {
		t.Fatal(err)
	}
	privateKey := filepath.Join(root, "signing.key")
	publicKey := filepath.Join(root, "signing.pub")
	password := filepath.Join(root, "signing.password")
	parameters := filepath.Join(root, "signing.parameters")
	raw, err := json.Marshal(map[string]string{"privateKeyFile": privateKey, "privateKeyPassphraseFile": password})
	if err != nil {
		t.Fatal(err)
	}
	definition := filepath.Join(root, "image.coopr")
	for path, data := range map[string][]byte{
		privateKey: keys.PrivateKey, publicKey: keys.PublicKey, password: []byte("build signing password\n"), parameters: raw,
		filepath.Join(root, "marker"): []byte("public payload"),
		definition:                    []byte("from \"scratch\" {\n copy \".\" \"/captured/\"\n}\n"),
	} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	name := strings.TrimPrefix(server.URL, "http://") + "/coopr/parameter-build:test"
	store := nativeBuildTestStore(t.TempDir())
	result, err := Run(ctx, Options{File: definition, Context: root, Push: true, Tag: name, TLSVerify: new(false), BuildStore: store, Signing: transfer.SigningOptions{SigstoreParameterFile: parameters}})
	if err != nil {
		t.Fatal(err)
	}
	registries := t.TempDir()
	if err := os.WriteFile(filepath.Join(registries, "signatures.yaml"), []byte(`{"default-docker":{"use-sigstore-attachments":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	requirement, err := signature.NewPRSigstoreSignedKeyPath(publicKey, signature.NewPRMMatchRepository())
	if err != nil {
		t.Fatal(err)
	}
	policy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{requirement}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = policy.Destroy() }()
	source, err := docker.ParseReference("//" + name)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := layout.NewReference(filepath.Join(t.TempDir(), "verified"), "verified")
	if err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{RegistriesDirPath: registries, DockerInsecureSkipTLSVerify: types.OptionalBoolTrue}
	if _, err := imagecopy.Image(ctx, policy, verified, source, &imagecopy.Options{SourceCtx: system, DestinationCtx: system, RemoveSignatures: true, ImageListSelection: imagecopy.CopyAllImages}); err != nil {
		t.Fatalf("native signature policy rejected built image: %v", err)
	}
	archive := filepath.Join(t.TempDir(), "image.tar")
	if _, err := transfer.Copy(ctx, oci.Image, result.ImageID, transfer.Destination{Transport: "oci-archive", Name: archive}, transfer.Options{BuildStore: store}); err != nil {
		t.Fatal(err)
	}
	names := archiveLayerNames(t, archive)
	if !containsName(names, "captured/marker") {
		t.Fatalf("public payload missing: %v", names)
	}
	for _, name := range names {
		for _, secret := range []string{"signing.key", "signing.password", "signing.parameters"} {
			if strings.Contains(name, secret) {
				t.Fatalf("signing credential leaked into image: %s", name)
			}
		}
	}
	// The build output dispatcher must pass signers to native destinations; an
	// unnamed directory must reach upstream signing identity validation.
	var descriptor v1.Descriptor
	if err := buildah.WithStore(store, func(backend storage.Store) error {
		raw, err := backend.ImageBigData(result.ImageID, storage.ImageDigestBigDataKey)
		if err != nil {
			return err
		}
		descriptor = oci.Descriptor(v1.MediaTypeImageManifest, raw)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err = applyOutputDestinations(ctx, oci.Image, "", descriptor, []transfer.Destination{{Transport: "dir", Name: filepath.Join(t.TempDir(), "native-output")}}, transfer.Options{BuildStore: store, Signing: transfer.SigningOptions{SigstoreParameterFile: parameters}})
	if err == nil || !strings.Contains(err.Error(), "Docker reference") {
		t.Fatalf("build dispatcher dropped native signer: %v", err)
	}

}
