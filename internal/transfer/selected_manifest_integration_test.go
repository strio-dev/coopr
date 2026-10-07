package transfer

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/buildah"
	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"github.com/containerd/platforms"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	cstorage "go.podman.io/storage"
	"go.podman.io/storage/pkg/reexec"
	"go.podman.io/storage/pkg/unshare"
	orasoci "oras.land/oras-go/v2/content/oci"
)

const (
	selectedCopyDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	selectedCopyDockerConfig   = "application/vnd.docker.container.image.v1+json"
	storedSignatureFixtureName = "coopr-test-stored-signature-fixture"
)

func init() {
	for _, name := range []string{storedSignatureFixtureName, storedSignatureFixtureName + "-in-a-user-namespace"} {
		reexec.Register(name, runStoredSignatureFixtureWorker)
	}
}

func TestMain(m *testing.M) {
	if reexec.Init() {
		return
	}
	if os.Getenv("COOPR_TEST_BUILDAH") != "" || os.Getenv("COOPR_TEST_BUILDAH_REGISTRY") != "" || os.Getenv("COOPR_TEST_CONTAINER_STORAGE") != "" {
		unshare.MaybeReexecUsingUserNamespace(false)
	}
	os.Exit(m.Run())
}

func TestExactManifestLookupPreservesNativePlatform(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "images")
	nativeArch, nonNativeArch := runtime.GOARCH, "arm64"
	if nativeArch == nonNativeArch {
		nonNativeArch = "amd64"
	}
	native := v1.Platform{OS: "linux", Architecture: nativeArch}
	nonNative := v1.Platform{OS: "linux", Architecture: nonNativeArch}
	store := nativeTestStore(dir)
	selection := nativeEmptyImageFixture(t, store, nonNative, "foreign")
	manifest := selection.Manifest

	got, selectedPlatform, found, err := lookupStoredImage(ctx, Options{BuildStore: nativeTestStore(dir)}, manifest.Digest.String(), native, false)
	if err != nil || !found || got.Manifest.Digest != manifest.Digest || selectedPlatform.Architecture != nonNativeArch {
		t.Fatalf("exact lookup = %+v on %+v, found=%t, err=%v", got, selectedPlatform, found, err)
	}
	if err := nameNativeTestImage(ctx, store, "copied:latest", got); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := lookupStoredImage(ctx, Options{BuildStore: store}, "copied:latest", nonNative, true); err != nil || !found {
		t.Fatalf("foreign tag found=%v err=%v", found, err)
	}
	if _, _, found, err := lookupStoredImage(ctx, Options{BuildStore: store}, "copied:latest", native, true); err != nil || found {
		t.Fatalf("foreign tag mislabeled native found=%v err=%v", found, err)
	}

}

func TestCopyPinnedManifestExportsExactVariantSharingImageID(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live selected-manifest copy coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "images")
	store := nativeTestStore(storeDir)
	var err error
	olderOCI, olderRef, newerDocker, newerRef := selectedCopySharedConfigFixture(t, ctx)
	build := func(name, reference, format string, platform v1.Platform, selection oci.StoredSelection) buildah.Result {
		t.Helper()
		parsed, err := definition.Parse(strings.NewReader("from \"" + reference + "\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		result, err := buildah.BuildDefinitionSupervised(ctx, parsed, planner.Options{
			Mode: planner.Build, Platform: platform.OS + "/" + platform.Architecture,
		}, buildah.SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output: buildah.Output{Path: filepath.Join(root, name), Format: format}, TLSVerify: new(false), Pull: true,
		})
		if err != nil {
			t.Fatalf("import %s variant: %v", name, err)
		}
		if result.ImageID != selection.ImageID || result.ManifestDigest != selection.Manifest.Digest.String() {
			t.Fatalf("imported %s = image %s manifest %s, want %s/%s", name, result.ImageID, result.ManifestDigest, selection.ImageID, selection.Manifest.Digest)
		}
		return result
	}
	native := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	oldResult := build("older-oci-output", olderRef, "", native, olderOCI)
	newResult := build("newer-docker-output", newerRef, "docker", native, newerDocker)
	if oldResult.ImageID != newResult.ImageID {
		t.Fatalf("fixture image IDs differ: OCI %s, Docker %s", oldResult.ImageID, newResult.ImageID)
	}
	if olderOCI.Manifest.Digest == newerDocker.Manifest.Digest {
		t.Fatal("fixture manifests unexpectedly match")
	}
	wrongMediaType := olderOCI.Manifest
	wrongMediaType.MediaType = selectedCopyDockerManifest
	if err := buildah.VerifyStoredImageSelectedSupervised(ctx, store, olderOCI.ImageID, wrongMediaType); err == nil || !strings.Contains(err.Error(), "selected descriptor") {
		t.Fatalf("verify selected manifest with wrong media type = %v", err)
	}
	wrongSize := olderOCI.Manifest
	wrongSize.Size++
	if err := buildah.VerifyStoredImageSelectedSupervised(ctx, store, olderOCI.ImageID, wrongSize); err == nil || !strings.Contains(err.Error(), "selected descriptor") {
		t.Fatalf("verify selected manifest with wrong size = %v", err)
	}

	archive := filepath.Join(root, "pinned-oci.tar")
	_, err = Copy(ctx, oci.Image, olderOCI.Manifest.Digest.String(), Destination{Transport: "oci-archive", Name: archive}, Options{BuildStore: store})
	if err != nil {
		t.Fatalf("copy pinned OCI manifest: %v", err)
	}
	if got := selectedCopyArchiveRoot(t, archive); got.Digest != olderOCI.Manifest.Digest || got.MediaType != olderOCI.Manifest.MediaType {
		t.Fatalf("copied root = %+v, want exact older OCI manifest %+v", got, olderOCI.Manifest)
	}
	registryServer := httptest.NewServer(registry.New())
	defer registryServer.Close()
	registryName := strings.TrimPrefix(registryServer.URL, "http://") + "/coopr/pinned:test"
	policyPath := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policyPath, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Copy(ctx, oci.Image, olderOCI.Manifest.Digest.String(), Destination{Transport: "registry", Name: registryName}, Options{
		BuildStore: store, TLSVerify: new(false), SignaturePolicyPath: policyPath,
	})
	if err != nil {
		t.Fatalf("copy pinned OCI manifest to registry: %v", err)
	}
	if want := strings.TrimSuffix(registryName, ":test") + "@" + olderOCI.Manifest.Digest.String(); result != want {
		t.Fatalf("registry copy = %q, want %q", result, want)
	}
	assertRemoteManifestDigests(t, ctx, registryName, olderOCI.Manifest, nil)
	if got, err := Copy(ctx, oci.Image, olderOCI.Manifest.Digest.String(), Destination{Transport: "local", Name: "pinned:latest"}, Options{BuildStore: store}); err != nil || got != "pinned:latest" {
		t.Fatalf("tag pinned OCI manifest locally = %q, %v", got, err)
	}
	if tagged, _, found, err := lookupStoredImage(ctx, Options{BuildStore: store}, "pinned:latest", v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, true); err != nil || !found || tagged.Manifest.Digest != olderOCI.Manifest.Digest {
		t.Fatalf("local pinned tag = %+v, found=%t, err=%v", tagged, found, err)
	}

	missing := olderOCI
	missing.Root = v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString("missing selected manifest"), Size: 1}
	missing.Manifest = missing.Root
	_, err = Copy(ctx, oci.Image, missing.Manifest.Digest.String(), Destination{Transport: "local", Name: "missing:latest"}, Options{BuildStore: store})
	if err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("local tag of missing selected manifest = %v", err)
	}

	otherArch := "arm64"
	if runtime.GOARCH == otherArch {
		otherArch = "amd64"
	}
	nonNativePlatform := v1.Platform{OS: "linux", Architecture: otherArch}
	nonNative := nativeEmptyImageFixture(t, store, nonNativePlatform, "foreign-copy")
	if err := nameNativeTestImage(ctx, store, "non-native:latest", nonNative); err != nil {
		t.Fatal(err)
	}
	if got, err := Copy(ctx, oci.Image, "non-native:latest", Destination{Transport: "local", Name: "non-native-default:latest"}, Options{BuildStore: store}); err != nil || got != "non-native-default:latest" {
		t.Fatalf("implicit sole-platform copy of non-native tag = %q, %v", got, err)
	}
	if copied, _, found, err := lookupStoredImage(ctx, Options{BuildStore: store}, "non-native-default:latest", nonNativePlatform, true); err != nil || !found || copied.Manifest.Digest != nonNative.Manifest.Digest {
		t.Fatalf("implicit sole-platform copy selection = %+v, found=%t, err=%v", copied, found, err)
	}
	if _, _, found, err := lookupStoredImage(ctx, Options{BuildStore: store}, "non-native-default:latest", v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, true); err != nil || found {
		t.Fatalf("implicit sole-platform copy gained native selection: found=%t, err=%v", found, err)
	}
	if got, err := Copy(ctx, oci.Image, "non-native:latest", Destination{Transport: "local", Name: "non-native-copy:latest"}, Options{
		BuildStore: store, Platform: nonNativePlatform, PlatformExplicit: true,
	}); err != nil || got != "non-native-copy:latest" {
		t.Fatalf("explicit-platform copy of non-native tag = %q, %v", got, err)
	}
	if got, err := Copy(ctx, oci.Image, nonNative.Root.Digest.String(), Destination{Transport: "local", Name: "non-native-index:latest"}, Options{
		BuildStore: store, Platform: nonNativePlatform, PlatformExplicit: true,
	}); err != nil || got != "non-native-index:latest" {
		t.Fatalf("explicit-platform copy of non-native digest = %q, %v", got, err)
	}
}

func TestLocalSigningPreservesSiblingManifestSignaturesSharingImageID(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live sibling-signature coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "images")
	store := nativeTestStore(storeDir)
	var err error
	store.GraphDriverOptions = nil
	layoutPath, olderOCI, newerDocker := selectedSigningSharedConfigLayoutFixture(t, ctx)
	importVariant := func(name string, selection oci.StoredSelection) {
		t.Helper()
		imageID, err := buildah.ImportLayoutSupervised(ctx, store, layoutPath, selection.Manifest)
		if err != nil {
			t.Fatalf("import %s variant: %v", name, err)
		}
		if imageID != selection.ImageID {
			t.Fatalf("imported %s image = %s, want %s", name, imageID, selection.ImageID)
		}
		if err := nameNativeTestImage(ctx, store, name, selection); err != nil {
			t.Fatalf("native %s variant: %v", name, err)
		}
	}
	importVariant("older-oci", olderOCI)
	importVariant("newer-docker", newerDocker)
	if olderOCI.ImageID != newerDocker.ImageID {
		t.Fatalf("fixture image IDs differ: OCI %s, Docker %s", olderOCI.ImageID, newerDocker.ImageID)
	}
	commitIndex := func(name string, selected []oci.StoredSelection) {
		t.Helper()
		index := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex}
		selections := make(map[string]oci.StoredSelection, len(selected))
		for position, selection := range selected {
			architecture := runtime.GOARCH
			if position == 1 {
				architecture = "arm64"
				if architecture == runtime.GOARCH {
					architecture = "amd64"
				}
			}
			platform := v1.Platform{OS: "linux", Architecture: architecture}
			descriptor := selection.Manifest
			descriptor.Platform = &platform
			index.Manifests = append(index.Manifests, descriptor)
			selection.Manifest = descriptor
			selections[platforms.Format(platform)] = selection
		}
		indexData, err := json.Marshal(index)
		if err != nil {
			t.Fatal(err)
		}
		rootDescriptor := v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: digest.FromBytes(indexData), Size: int64(len(indexData))}
		for key, selection := range selections {
			selection.Root = rootDescriptor
			selections[key] = selection
		}
		if err := nativeTestIndex(ctx, store, name, rootDescriptor, indexData, selections); err != nil {
			t.Fatalf("native signing index %s: %v", name, err)
		}
	}
	commitIndex("oci-only:latest", []oci.StoredSelection{olderOCI})
	commitIndex("docker-only:latest", []oci.StoredSelection{newerDocker})

	gpgHome, err := os.MkdirTemp("", "coopr-gpg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(gpgHome) })
	t.Setenv("GNUPGHOME", gpgHome)
	t.Cleanup(func() { _ = exec.Command("gpgconf", "--homedir", gpgHome, "--kill", "gpg-agent").Run() })
	passphrase := "shared image ID signing passphrase"
	fingerprint, publicKey := generateGPGKey(t, ctx, gpgHome, "Coopr Variant Signing Test <variant-signing@example.invalid>", passphrase)
	passphraseFile := filepath.Join(t.TempDir(), "passphrase")
	if err := os.WriteFile(passphraseFile, []byte(passphrase), 0o600); err != nil {
		t.Fatal(err)
	}
	configureTestSignatureLookaside(t, filepath.Join(t.TempDir(), "lookaside"))
	server := httptest.NewServer(registry.New())
	defer server.Close()
	repository := strings.TrimPrefix(server.URL, "http://") + "/coopr/shared"
	defaultName := repository + ":native-default"
	if _, err := buildah.TransferStoredImageSupervised(ctx, buildah.StoredTransferOptions{
		Store: store, ImageID: olderOCI.ImageID, RegistryDestination: defaultName, TLSVerify: new(false),
	}); err != nil {
		t.Fatalf("publish unsigned native default manifest: %v", err)
	}
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	resolvedDefault, err := resolver.Resolve(ctx, defaultName, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, oci.Image)
	if err != nil {
		t.Fatalf("resolve native default manifest: %v", err)
	}
	defaultSource, siblingSource := "", ""
	var defaultSelection, siblingSelection oci.StoredSelection
	switch resolvedDefault.Selected.Digest {
	case olderOCI.Manifest.Digest:
		defaultSource, siblingSource, defaultSelection, siblingSelection = "oci-only:latest", "docker-only:latest", olderOCI, newerDocker
	case newerDocker.Manifest.Digest:
		defaultSource, siblingSource, defaultSelection, siblingSelection = "docker-only:latest", "oci-only:latest", newerDocker, olderOCI
	default:
		t.Fatalf("native default manifest = %s, want %s or %s", resolvedDefault.Selected.Digest, olderOCI.Manifest.Digest, newerDocker.Manifest.Digest)
	}
	localIdentity := repository + ":signing"
	localSigning := Options{
		BuildStore: store,
		Signing:    SigningOptions{SignBy: fingerprint, PassphraseFile: passphraseFile},
	}
	syntheticSignature := []byte("\x00sigstore-json\n{\"mimeType\":\"application/vnd.dev.cosign.simplesigning.v1+json\",\"payload\":\"cGF5bG9hZA==\",\"annotations\":{\"coopr.test\":\"preserve\"}}")
	unknownMetadata := json.RawMessage(`{"coopr-test-preserve":["unknown",{"value":7}]}`)
	injectStoredSignatureFixture(t, store, siblingSelection.ImageID, siblingSelection.Manifest.Digest, syntheticSignature, unknownMetadata)
	// Sign the native default first and its sibling last. A selected-signature
	// write must not replace the ordinary native source's default signature.
	for _, source := range []string{defaultSource, siblingSource} {
		if _, err := Copy(ctx, oci.Image, source, Destination{Transport: "local", Name: localIdentity}, localSigning); err != nil {
			t.Fatalf("sign shared-ID manifest %s: %v", source, err)
		}
	}
	assertStoredSignatureFixture(t, store, siblingSelection.ImageID, siblingSelection.Manifest.Digest, syntheticSignature, unknownMetadata)
	if _, err := Copy(ctx, oci.Image, siblingSelection.Manifest.Digest.String(), Destination{Transport: "registry", Name: "127.0.0.1:1/coopr/failure:test"}, Options{
		BuildStore: store, TLSVerify: new(false), RetrySet: true,
	}); err == nil {
		t.Fatal("selected publish to unavailable registry unexpectedly succeeded")
	}
	assertStoredTransferState(t, store, siblingSelection.ImageID, siblingSelection.Manifest.Digest)
	publishAndVerify := func(selected oci.StoredSelection, tag string) {
		t.Helper()
		name := repository + ":" + tag
		if _, err := Copy(ctx, oci.Image, selected.Manifest.Digest.String(), Destination{Transport: "registry", Name: name}, Options{
			BuildStore: store, TLSVerify: new(false),
		}); err != nil {
			t.Fatalf("publish selected manifest %s: %v", selected.Manifest.Digest, err)
		}
		assertRemoteManifestDigests(t, ctx, name, selected.Manifest, nil)
		verifyGPGSignedReferenceInSubprocess(t, ctx, name, publicKey, true)
	}
	publishAndVerify(olderOCI, "oci")
	publishAndVerify(newerDocker, "docker")
	assertStoredTransferState(t, store, olderOCI.ImageID, olderOCI.Manifest.Digest, newerDocker.Manifest.Digest)
	assertRemoteSyntheticSigstore(t, ctx, server.Client(), server.URL, "coopr/shared", siblingSelection.Manifest.Digest)
	if _, err := buildah.TransferStoredImageSupervised(ctx, buildah.StoredTransferOptions{
		Store: store, ImageID: olderOCI.ImageID, RegistryDestination: defaultName, TLSVerify: new(false),
	}); err != nil {
		t.Fatalf("publish native default manifest: %v", err)
	}
	assertRemoteManifestDigests(t, ctx, defaultName, defaultSelection.Manifest, nil)
	verifyGPGSignedReferenceInSubprocess(t, ctx, defaultName, publicKey, true)

	if _, err := Copy(ctx, oci.Image, "oci-only:latest", Destination{Transport: "local", Name: localIdentity}, localSigning); err != nil {
		t.Fatalf("re-sign selected manifest %s: %v", olderOCI.Manifest.Digest, err)
	}
	publishAndVerify(olderOCI, "oci-resigned")
	publishAndVerify(newerDocker, "docker-after-sibling-resign")
	assertStoredTransferState(t, store, olderOCI.ImageID, olderOCI.Manifest.Digest, newerDocker.Manifest.Digest)
	if _, err := buildah.TransferStoredImageSupervised(ctx, buildah.StoredTransferOptions{
		Store: store, ImageID: olderOCI.ImageID, RegistryDestination: defaultName, TLSVerify: new(false),
	}); err != nil {
		t.Fatalf("republish native default manifest: %v", err)
	}
	assertRemoteManifestDigests(t, ctx, defaultName, defaultSelection.Manifest, nil)
	verifyGPGSignedReferenceInSubprocess(t, ctx, defaultName, publicKey, true)
}

func assertRemoteSyntheticSigstore(t *testing.T, ctx context.Context, client *http.Client, registryURL, repository string, manifestDigest digest.Digest) {
	t.Helper()
	attachmentURL := registryURL + "/v2/" + repository + "/manifests/sha256-" + manifestDigest.Encoded() + ".sig"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, attachmentURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", v1.MediaTypeImageManifest)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		t.Fatalf("read remote sigstore attachment: %s", response.Status)
	}
	attachmentData, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		t.Fatal(err)
	}
	var attachment v1.Manifest
	if err := json.Unmarshal(attachmentData, &attachment); err != nil {
		t.Fatal(err)
	}
	for _, layer := range attachment.Layers {
		if layer.MediaType != "application/vnd.dev.cosign.simplesigning.v1+json" || layer.Annotations["coopr.test"] != "preserve" {
			continue
		}
		blobRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, registryURL+"/v2/"+repository+"/blobs/"+layer.Digest.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		blobResponse, err := client.Do(blobRequest)
		if err != nil {
			t.Fatal(err)
		}
		payload, readErr := io.ReadAll(blobResponse.Body)
		closeErr := blobResponse.Body.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			t.Fatal(err)
		}
		if blobResponse.StatusCode != http.StatusOK || !bytes.Equal(payload, []byte("payload")) {
			t.Fatalf("remote synthetic sigstore payload = %q (%s), want payload", payload, blobResponse.Status)
		}
		return
	}
	t.Fatal("remote sigstore attachment did not preserve the synthetic signature layer and annotations")
}

func injectStoredSignatureFixture(t *testing.T, options buildah.StoreOptions, imageID string, manifestDigest digest.Digest, signatureBlob []byte, unknownMetadata json.RawMessage) {
	t.Helper()
	runStoredSignatureFixture(t, storedSignatureFixtureRequest{Store: options, ImageID: imageID, ManifestDigest: manifestDigest, SignatureBlob: signatureBlob, UnknownMetadata: unknownMetadata})
}

func assertStoredSignatureFixture(t *testing.T, options buildah.StoreOptions, imageID string, manifestDigest digest.Digest, signatureBlob []byte, unknownMetadata json.RawMessage) {
	t.Helper()
	runStoredSignatureFixture(t, storedSignatureFixtureRequest{Store: options, ImageID: imageID, ManifestDigest: manifestDigest, SignatureBlob: signatureBlob, UnknownMetadata: unknownMetadata, Assert: true})
}

func assertStoredTransferState(t *testing.T, options buildah.StoreOptions, imageID string, expected ...digest.Digest) {
	t.Helper()
	runStoredSignatureFixture(t, storedSignatureFixtureRequest{Store: options, ImageID: imageID, AssertTransferState: true, ExpectedIndexes: expected})
}

type storedSignatureFixtureRequest struct {
	Store               buildah.StoreOptions `json:"store"`
	ImageID             string               `json:"image_id"`
	ManifestDigest      digest.Digest        `json:"manifest_digest"`
	SignatureBlob       []byte               `json:"signature_blob"`
	UnknownMetadata     json.RawMessage      `json:"unknown_metadata"`
	Assert              bool                 `json:"assert"`
	AssertTransferState bool                 `json:"assert_transfer_state"`
	ExpectedIndexes     []digest.Digest      `json:"expected_indexes"`
}

func runStoredSignatureFixture(t *testing.T, request storedSignatureFixtureRequest) {
	t.Helper()
	requestPath := filepath.Join(t.TempDir(), "request.json")
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	command := reexec.Command(storedSignatureFixtureName, requestPath)
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("stored signature fixture worker: %v\n%s", err, output)
	}
}

func runStoredSignatureFixtureWorker() {
	unshare.MaybeReexecUsingUserNamespace(false)
	if len(os.Args) != 2 {
		_, _ = fmt.Fprintln(os.Stderr, "invalid stored signature fixture arguments")
		os.Exit(2)
	}
	if err := executeStoredSignatureFixture(os.Args[1]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func executeStoredSignatureFixture(requestPath string) (retErr error) {
	data, err := os.ReadFile(requestPath)
	if err != nil {
		return err
	}
	var request storedSignatureFixtureRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return err
	}
	store, err := cstorage.GetStore(cstorage.StoreOptions{
		RunRoot: request.Store.RunRoot, GraphRoot: request.Store.GraphRoot, ImageStore: request.Store.ImageStore,
		GraphDriverName: request.Store.GraphDriverName, GraphDriverOptions: append([]string(nil), request.Store.GraphDriverOptions...), TransientStore: request.Store.TransientStore,
	})
	if err != nil {
		return err
	}
	defer func() {
		_, closeErr := store.Shutdown(false)
		retErr = errors.Join(retErr, closeErr)
	}()
	metadataText, err := store.Metadata(request.ImageID)
	if err != nil {
		return err
	}
	metadata := map[string]json.RawMessage{}
	if metadataText != "" {
		if err := json.Unmarshal([]byte(metadataText), &metadata); err != nil {
			return err
		}
	}
	var perDigest map[digest.Digest][]int
	if raw := metadata["signatures-sizes"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &perDigest); err != nil {
			return err
		}
	}
	if request.Assert {
		if !bytes.Equal(metadata["coopr-test-unknown"], request.UnknownMetadata) {
			return fmt.Errorf("unknown storage metadata = %s, want %s", metadata["coopr-test-unknown"], request.UnknownMetadata)
		}
		sizes := perDigest[request.ManifestDigest]
		if len(sizes) != 2 || sizes[0] != len(request.SignatureBlob) || sizes[1] <= 0 {
			return fmt.Errorf("preserved signature sizes = %v, want synthetic + GPG", sizes)
		}
		stored, err := store.ImageBigData(request.ImageID, "signature-"+request.ManifestDigest.Encoded())
		if err != nil {
			return err
		}
		if len(stored) != sizes[0]+sizes[1] || !bytes.Equal(stored[:sizes[0]], request.SignatureBlob) {
			return errors.New("stored non-simple signature was not preserved byte-for-byte before appended GPG signature")
		}
		return nil
	}
	if request.AssertTransferState {
		image, err := store.Image(request.ImageID)
		if err != nil {
			return err
		}
		for _, name := range image.Names {
			if strings.Contains(name, "/coopr-transfer/") {
				return fmt.Errorf("temporary selected-image name leaked: %s", name)
			}
		}
		found := map[digest.Digest]int{}
		for _, key := range image.BigDataNames {
			if !strings.HasPrefix(key, cstorage.ImageDigestManifestBigDataNamePrefix+"-") {
				continue
			}
			data, err := store.ImageBigData(request.ImageID, key)
			if err != nil {
				return err
			}
			var index v1.Index
			if json.Unmarshal(data, &index) != nil || index.MediaType != v1.MediaTypeImageIndex || len(index.Manifests) != 1 {
				continue
			}
			found[index.Manifests[0].Digest]++
		}
		if len(found) != len(request.ExpectedIndexes) {
			return fmt.Errorf("stored selected indexes = %v, want exactly %v", found, request.ExpectedIndexes)
		}
		for _, expected := range request.ExpectedIndexes {
			if found[expected] != 1 {
				return fmt.Errorf("stored selected index count for %s = %d, want 1", expected, found[expected])
			}
		}
		return nil
	}
	if perDigest == nil {
		perDigest = map[digest.Digest][]int{}
	}
	perDigest[request.ManifestDigest] = []int{len(request.SignatureBlob)}
	metadata["signatures-sizes"], err = json.Marshal(perDigest)
	if err != nil {
		return err
	}
	metadata["coopr-test-unknown"] = bytes.Clone(request.UnknownMetadata)
	if err := store.SetImageBigData(request.ImageID, "signature-"+request.ManifestDigest.Encoded(), request.SignatureBlob, nil); err != nil {
		return err
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	return store.SetMetadata(request.ImageID, string(encoded))
}

func selectedCopySharedConfigFixture(t *testing.T, ctx context.Context) (oci.StoredSelection, string, oci.StoredSelection, string) {
	t.Helper()
	layout, olderOCI, newerDocker := selectedCopySharedConfigLayoutFixture(t, ctx)
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	olderRef := host + "/coopr/shared:oci"
	newerRef := host + "/coopr/shared:docker"
	if _, err := resolver.PublishLayout(ctx, olderRef, layout, olderOCI.Manifest); err != nil {
		t.Fatalf("publish OCI fixture: %v", err)
	}
	if _, err := resolver.PublishLayout(ctx, newerRef, layout, newerDocker.Manifest); err != nil {
		t.Fatalf("publish Docker fixture: %v", err)
	}
	return olderOCI, olderRef, newerDocker, newerRef
}

func selectedCopySharedConfigLayoutFixture(t *testing.T, ctx context.Context) (string, oci.StoredSelection, oci.StoredSelection) {
	t.Helper()
	layout := t.TempDir()
	source, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	configData, err := json.Marshal(v1.Image{
		Platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH},
		RootFS:   v1.RootFS{Type: "layers"},
		Config:   v1.ImageConfig{Env: []string{"VARIANT=shared"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: digest.FromBytes(configData), Size: int64(len(configData))}
	if err := source.Push(ctx, config, bytes.NewReader(configData)); err != nil {
		t.Fatal(err)
	}
	manifest := func(manifestType, configType string) v1.Descriptor {
		t.Helper()
		configDescriptor := config
		configDescriptor.MediaType = configType
		data, err := json.Marshal(v1.Manifest{
			Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: manifestType, Config: configDescriptor,
		})
		if err != nil {
			t.Fatal(err)
		}
		descriptor := v1.Descriptor{MediaType: manifestType, Digest: digest.FromBytes(data), Size: int64(len(data))}
		if err := source.Push(ctx, descriptor, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		return descriptor
	}
	ociManifest := manifest(v1.MediaTypeImageManifest, v1.MediaTypeImageConfig)
	dockerManifest := manifest(selectedCopyDockerManifest, selectedCopyDockerConfig)
	selection := func(descriptor v1.Descriptor) oci.StoredSelection {
		return oci.StoredSelection{
			Root: descriptor, Manifest: descriptor, ImageID: config.Digest.Encoded(), ConfigData: configData,
		}
	}
	olderOCI, newerDocker := selection(ociManifest), selection(dockerManifest)
	return layout, olderOCI, newerDocker
}

func selectedSigningSharedConfigLayoutFixture(t *testing.T, ctx context.Context) (string, oci.StoredSelection, oci.StoredSelection) {
	t.Helper()
	layoutPath := t.TempDir()
	store, _, manifests := multiPlatformImageFixtureAt(t, ctx, layoutPath)
	ociManifest := manifests[0]
	manifestReader, err := store.Fetch(ctx, ociManifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestData, readErr := io.ReadAll(manifestReader)
	if err := errors.Join(readErr, manifestReader.Close()); err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	configReader, err := store.Fetch(ctx, manifest.Config)
	if err != nil {
		t.Fatal(err)
	}
	configData, readErr := io.ReadAll(configReader)
	if err := errors.Join(readErr, configReader.Close()); err != nil {
		t.Fatal(err)
	}
	manifest.MediaType = selectedCopyDockerManifest
	manifest.Config.MediaType = selectedCopyDockerConfig
	for index := range manifest.Layers {
		if manifest.Layers[index].MediaType == v1.MediaTypeImageLayerGzip {
			manifest.Layers[index].MediaType = "application/vnd.docker.image.rootfs.diff.tar.gzip"
		}
	}
	dockerData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	dockerManifest := v1.Descriptor{MediaType: selectedCopyDockerManifest, Digest: digest.FromBytes(dockerData), Size: int64(len(dockerData))}
	if err := store.Push(ctx, dockerManifest, bytes.NewReader(dockerData)); err != nil {
		t.Fatal(err)
	}
	selection := func(descriptor v1.Descriptor) oci.StoredSelection {
		return oci.StoredSelection{Root: descriptor, Manifest: descriptor, ImageID: manifest.Config.Digest.Encoded(), ConfigData: configData}
	}
	return layoutPath, selection(ociManifest), selection(dockerManifest)
}

func selectedCopyArchiveRoot(t *testing.T, archive string) v1.Descriptor {
	t.Helper()
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			t.Fatal("archive has no index.json")
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != "index.json" {
			continue
		}
		var index v1.Index
		if err := json.NewDecoder(reader).Decode(&index); err != nil {
			t.Fatal(err)
		}
		if len(index.Manifests) != 1 {
			t.Fatalf("archive manifests = %+v, want one", index.Manifests)
		}
		return index.Manifests[0]
	}
}
