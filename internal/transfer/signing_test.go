package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	buildahbackend "coopr/internal/buildah"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	"github.com/containerd/platforms"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	commonconfig "go.podman.io/common/pkg/config"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/signature/sigstore"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage/pkg/reexec"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestSigningOptionsPassphraseAndDestinationValidation(t *testing.T) {
	if err := ValidateSigningDestination(oci.Image, Destination{Transport: "registry"}, SigningOptions{PassphraseFile: "secret"}); err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("passphrase without key accepted: %v", err)
	}
	options := SigningOptions{SigstorePrivateKeyFile: "key"}
	if err := ValidateSigningDestination(oci.Component, Destination{Transport: "registry"}, options); err == nil || !strings.Contains(err.Error(), "only for images") {
		t.Fatalf("component signing accepted: %v", err)
	}
	if err := ValidateSigningDestination(oci.Image, Destination{Transport: "local"}, options); err == nil || !strings.Contains(err.Error(), "requires a registry") {
		t.Fatalf("local signing accepted: %v", err)
	}
	if err := ValidateSigningDestination(oci.Image, Destination{Transport: "local"}, SigningOptions{SignBy: "fingerprint"}); err != nil {
		t.Fatalf("local GPG signing rejected: %v", err)
	}
	missingStore := filepath.Join(t.TempDir(), "must-not-be-created")
	if _, err := Copy(context.Background(), oci.Image, "source:latest", Destination{Transport: "local", Name: "dest:latest"}, Options{BuildStore: nativeTestStore(missingStore), Signing: options}); err == nil || !strings.Contains(err.Error(), "requires a registry") {
		t.Fatalf("Copy accepted nonregistry signing: %v", err)
	}
	if _, err := CopyRoot(context.Background(), oci.Image, missingStore, v1.Descriptor{}, Destination{Transport: "local", Name: "example:latest"}, Options{Signing: options}); err == nil || !strings.Contains(err.Error(), "requires a registry") {
		t.Fatalf("copy did not validate signing before opening its store: %v", err)
	}
	if _, err := os.Stat(missingStore); !os.IsNotExist(err) {
		t.Fatalf("invalid signed copy created its store: %v", err)
	}
	if err := ValidateSigningDestination(oci.Image, Destination{Transport: "registry"}, SigningOptions{SignBy: "fingerprint", SigstorePrivateKeyFile: "key"}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("multiple signing modes accepted: %v", err)
	}

	t.Setenv("COSIGN_PASSWORD", "from-environment")
	sigstorePassphrase, gpgPassphrase, err := options.passphrases()
	if err != nil || string(sigstorePassphrase) != "from-environment" || gpgPassphrase != "" {
		t.Fatalf("environment passphrases = %q/%q, %v", sigstorePassphrase, gpgPassphrase, err)
	}
	passphraseFile := filepath.Join(t.TempDir(), "passphrase")
	if err := os.WriteFile(passphraseFile, []byte("from-file\r\nignored"), 0600); err != nil {
		t.Fatal(err)
	}
	sigstorePassphrase, gpgPassphrase, err = (SigningOptions{SigstorePrivateKeyFile: "key", PassphraseFile: passphraseFile}).passphrases()
	if err != nil || string(sigstorePassphrase) != "from-file" || gpgPassphrase != "" {
		t.Fatalf("sigstore file passphrases = %q/%q, %v", sigstorePassphrase, gpgPassphrase, err)
	}
	sigstorePassphrase, gpgPassphrase, err = (SigningOptions{SignBy: "fingerprint", PassphraseFile: passphraseFile}).passphrases()
	if err != nil || sigstorePassphrase != nil || gpgPassphrase != "from-file" {
		t.Fatalf("GPG file passphrases = %q/%q, %v", sigstorePassphrase, gpgPassphrase, err)
	}
}

func TestStoredTransferOptionsCaptureCosignPasswordForSanitizedWorker(t *testing.T) {
	t.Setenv("COSIGN_PASSWORD", "worker-only-passphrase")
	store, err := buildahbackend.DefaultStoreOptions()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store.RunRoot = filepath.Join(root, "runroot")
	store.GraphRoot = filepath.Join(root, "graphroot")
	options, cleanup, err := storedTransferOptions("image-id", digest.FromString("manifest"), Options{
		BuildStore: store,
		Signing:    SigningOptions{SigstorePrivateKeyFile: "cosign.key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	passphraseFile := options.SigningPassphraseFile
	defer cleanup()
	if passphraseFile == "" {
		t.Fatal("COSIGN_PASSWORD was not captured for the sanitized worker")
	}
	data, err := os.ReadFile(passphraseFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "worker-only-passphrase" {
		t.Fatalf("captured passphrase = %q", data)
	}
	cleanup()
	if _, err := os.Stat(passphraseFile); !os.IsNotExist(err) {
		t.Fatalf("captured passphrase file remains after cleanup: %v", err)
	}
}

func TestSignedTransferSystemContextUsesDirectCredentialsAndCertLeaf(t *testing.T) {
	certDir := t.TempDir()
	system, err := signedTransferSystemContext("auth.json", certDir, "", Options{
		Credentials: "user:pass", CertDir: certDir, TLSVerify: new(false), SignaturePolicyPath: "policy.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if system.DockerCertPath != certDir || system.DockerPerHostCertDirPath != "" || system.DockerAuthConfig == nil || system.DockerAuthConfig.Username != "user" || system.DockerAuthConfig.Password != "pass" || system.SignaturePolicyPath != "policy.json" || system.DockerInsecureSkipTLSVerify != types.OptionalBoolTrue {
		t.Fatalf("system context = %#v", system)
	}
	if _, err := signedTransferSystemContext("", "", "", Options{Credentials: ":pass"}); err == nil {
		t.Fatal("empty credential username accepted")
	}
}

func TestSignedTransferTLSVerifyPolicy(t *testing.T) {
	for _, test := range []struct {
		verify *bool
		want   types.OptionalBool
	}{
		{nil, types.OptionalBoolUndefined},
		{new(true), types.OptionalBoolFalse},
		{new(false), types.OptionalBoolTrue},
	} {
		system, err := signedTransferSystemContext("", "", "", Options{TLSVerify: test.verify})
		if err != nil {
			t.Fatal(err)
		}
		if system.DockerInsecureSkipTLSVerify != test.want {
			t.Fatalf("native TLS policy = %v, want %v", system.DockerInsecureSkipTLSVerify, test.want)
		}
	}
}

func TestNativeRetryOptionsDistinguishExplicitZeroFromNativeDefault(t *testing.T) {
	explicit, err := nativeRetryOptions(Options{RetrySet: true, Retry: 0, RetryDelay: 25 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.MaxRetry != 0 || explicit.Delay != 25*time.Millisecond {
		t.Fatalf("explicit retry options = %#v", explicit)
	}
	native, err := nativeRetryOptions(Options{})
	if err != nil {
		t.Fatal(err)
	}
	config, err := commonconfig.Default()
	if err != nil {
		t.Fatal(err)
	}
	if native.MaxRetry != int(config.Engine.Retry) {
		t.Fatalf("native retry options = %#v", native)
	}
}

func TestCopyRootSignsRegistryImageWithGPGKey(t *testing.T) {
	ctx := context.Background()
	storeDir := filepath.Join(t.TempDir(), "images")
	source, root, manifests := multiPlatformImageFixture(t, ctx)
	if err := localstore.Put(ctx, storeDir, source, root, "gpg-signed:latest"); err != nil {
		t.Fatal(err)
	}

	gpgHome := filepath.Join(t.TempDir(), "gnupg")
	if err := os.Mkdir(gpgHome, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GNUPGHOME", gpgHome)
	t.Cleanup(func() {
		_ = exec.Command("gpgconf", "--homedir", gpgHome, "--kill", "gpg-agent").Run()
	})
	passphrase := "correct horse battery staple"
	fingerprint, publicKey := generateGPGKey(t, ctx, gpgHome, "Coopr Signing Test <signing@example.invalid>", passphrase)
	_, wrongPublicKey := generateGPGKey(t, ctx, gpgHome, "Coopr Wrong Key <wrong@example.invalid>", "wrong password")
	passphraseFile := filepath.Join(t.TempDir(), "passphrase")
	if err := os.WriteFile(passphraseFile, []byte(passphrase+"\nignored"), 0600); err != nil {
		t.Fatal(err)
	}

	lookaside := filepath.Join(t.TempDir(), "lookaside")
	configureTestSignatureLookaside(t, lookaside)

	server := httptest.NewServer(registry.New())
	defer server.Close()
	registryName := strings.TrimPrefix(server.URL, "http://") + "/coopr/gpg-signed:test"
	result, err := CopyRoot(ctx, oci.Image, storeDir, root, Destination{Transport: "registry", Name: registryName}, Options{
		TLSVerify: new(false),
		Signing:   SigningOptions{SignBy: fingerprint, PassphraseFile: passphraseFile},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSuffix(registryName, ":test") + "@" + root.Digest.String(); result != want {
		t.Fatalf("GPG-signed result = %q, want %q", result, want)
	}
	verifyGPGSignedReferenceInSubprocess(t, ctx, registryName, publicKey, true)
	verifyGPGSignedReferenceInSubprocess(t, ctx, registryName, wrongPublicKey, false)
	for _, manifest := range manifests {
		digestReference := strings.TrimSuffix(registryName, ":test") + "@" + manifest.Digest.String()
		verifyGPGSignedReferenceInSubprocess(t, ctx, digestReference, publicKey, true)
	}
	assertRemoteManifestDigests(t, ctx, registryName, root, manifests)
}

func TestSignStoredImageWithGPGKey(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live native-store signing coverage")
	}
	ctx := context.Background()
	layoutPath := filepath.Join(t.TempDir(), "layout")
	layoutStore, root, manifests := multiPlatformImageFixtureAt(t, ctx, layoutPath)
	storeDir := filepath.Join(t.TempDir(), "images")
	storeOptions := nativeTestStore(storeDir)
	selections := make(map[string]oci.StoredSelection, len(manifests))
	for _, selected := range manifests {
		imageID, err := buildahbackend.ImportLayoutSupervised(ctx, storeOptions, layoutPath, selected)
		if err != nil {
			t.Fatal(err)
		}
		manifestReader, err := layoutStore.Fetch(ctx, selected)
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
		configReader, err := layoutStore.Fetch(ctx, manifest.Config)
		if err != nil {
			t.Fatal(err)
		}
		configData, readErr := io.ReadAll(configReader)
		if err := errors.Join(readErr, configReader.Close()); err != nil {
			t.Fatal(err)
		}
		platform := platforms.Normalize(*selected.Platform)
		selections[platforms.Format(platform)] = oci.StoredSelection{Root: root, Manifest: selected, ImageID: imageID, ConfigData: configData}
	}
	indexReader, err := layoutStore.Fetch(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	indexData, readErr := io.ReadAll(indexReader)
	if err := errors.Join(readErr, indexReader.Close()); err != nil {
		t.Fatal(err)
	}
	if err := nativeTestIndex(ctx, storeOptions, "source:latest", root, indexData, selections); err != nil {
		t.Fatal(err)
	}

	gpgHome := filepath.Join(t.TempDir(), "gnupg")
	if err := os.Mkdir(gpgHome, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GNUPGHOME", gpgHome)
	t.Cleanup(func() { _ = exec.Command("gpgconf", "--homedir", gpgHome, "--kill", "gpg-agent").Run() })
	passphrase := "local signing passphrase"
	fingerprint, publicKey := generateGPGKey(t, ctx, gpgHome, "Coopr Local Signing Test <local-signing@example.invalid>", passphrase)
	passphraseFile := filepath.Join(t.TempDir(), "passphrase")
	if err := os.WriteFile(passphraseFile, []byte(passphrase+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	lookaside := filepath.Join(t.TempDir(), "lookaside")
	configureTestSignatureLookaside(t, lookaside)
	server := httptest.NewServer(registry.New())
	defer server.Close()
	name := strings.TrimPrefix(server.URL, "http://") + "/coopr/signed:latest"
	if _, err := Copy(ctx, oci.Image, "source:latest", Destination{Transport: "local", Name: name}, Options{

		BuildStore: storeOptions,
		Signing:    SigningOptions{SignBy: fingerprint, PassphraseFile: passphraseFile},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(ctx, oci.Image, name, Destination{Transport: "registry", Name: name}, Options{

		BuildStore: storeOptions,
		TLSVerify:  new(false),
	}); err != nil {
		t.Fatal(err)
	}
	assertRemoteManifestDigests(t, ctx, name, root, manifests)
	for _, manifest := range manifests {
		digestReference := strings.TrimSuffix(name, ":latest") + "@" + manifest.Digest.String()
		verifyGPGSignedReferenceInSubprocess(t, ctx, digestReference, publicKey, true)
	}

	sigstorePassphrase := []byte("native store sigstore passphrase")
	sigstoreKeys, err := sigstore.GenerateKeyPair(sigstorePassphrase)
	if err != nil {
		t.Fatal(err)
	}
	sigstorePrivateKey := filepath.Join(t.TempDir(), "cosign.key")
	sigstorePublicKey := filepath.Join(t.TempDir(), "cosign.pub")
	for path, data := range map[string][]byte{sigstorePrivateKey: sigstoreKeys.PrivateKey, sigstorePublicKey: sigstoreKeys.PublicKey} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("COSIGN_PASSWORD", string(sigstorePassphrase))
	if _, err := Copy(ctx, oci.Image, "source:latest", Destination{Transport: "registry", Name: name}, Options{

		BuildStore: storeOptions,
		TLSVerify:  new(false),
		Signing:    SigningOptions{SigstorePrivateKeyFile: sigstorePrivateKey},
	}); err != nil {
		t.Fatal(err)
	}
	verifySignedReference(t, ctx, name, sigstorePublicKey, imagecopy.CopyAllImages, true)
	for _, manifest := range manifests {
		digestReference := strings.TrimSuffix(name, ":latest") + "@" + manifest.Digest.String()
		verifySignedReference(t, ctx, digestReference, sigstorePublicKey, imagecopy.CopySystemImage, true)
		verifyGPGSignedReferenceInSubprocess(t, ctx, digestReference, publicKey, true)
	}
}

func configureTestSignatureLookaside(t *testing.T, lookaside string) {
	t.Helper()
	configHome := t.TempDir()
	registriesDir := filepath.Join(configHome, "containers", "registries.d")
	if err := os.MkdirAll(registriesDir, 0700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("default-docker:\n  lookaside: %q\n", "file://"+lookaside)
	if err := os.WriteFile(filepath.Join(registriesDir, "coopr-test.yaml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	// These private registries start with unsigned fixtures. Keep the test
	// policy inside their isolated config instead of depending on the host.
	if err := os.WriteFile(filepath.Join(configHome, "containers", "policy.json"), []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)
}

func TestGPGVerificationHelper(t *testing.T) {
	if os.Getenv("COOPR_TEST_GPG_VERIFY_HELPER") == "" {
		t.Skip("verification subprocess helper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	verifyGPGSignedReference(t, ctx, os.Getenv("COOPR_TEST_GPG_REFERENCE"), os.Getenv("COOPR_TEST_GPG_PUBLIC_KEY"), os.Getenv("COOPR_TEST_GPG_WANT_ALLOWED") == "1")
}

func TestCopyRootSignsRegistryImageWithSigstoreKey(t *testing.T) {
	ctx := context.Background()
	storeDir := filepath.Join(t.TempDir(), "images")
	source, root := imageFixture(t, ctx)
	if err := localstore.Put(ctx, storeDir, source, root, "signed:latest"); err != nil {
		t.Fatal(err)
	}
	passphrase := []byte("correct horse battery staple")
	keys, err := sigstore.GenerateKeyPair(passphrase)
	if err != nil {
		t.Fatal(err)
	}
	keyDir := t.TempDir()
	privateKey := filepath.Join(keyDir, "cosign.key")
	publicKey := filepath.Join(keyDir, "cosign.pub")
	passphraseFile := filepath.Join(keyDir, "password")
	for path, data := range map[string][]byte{privateKey: keys.PrivateKey, publicKey: keys.PublicKey, passphraseFile: append(passphrase, '\n')} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	registryName := strings.TrimPrefix(server.URL, "http://") + "/coopr/signed:test"
	result, err := CopyRoot(ctx, oci.Image, storeDir, root, Destination{Transport: "registry", Name: registryName}, Options{
		TLSVerify: new(false),
		Signing: SigningOptions{
			SigstorePrivateKeyFile: privateKey,
			PassphraseFile:         passphraseFile,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSuffix(registryName, ":test") + "@" + root.Digest.String(); result != want {
		t.Fatalf("signed result = %q, want %q", result, want)
	}
	if os.Getenv("COOPR_TEST_COSIGN") != "" {
		command := exec.CommandContext(ctx, "cosign", "verify", "--key", publicKey, "--insecure-ignore-tlog", "--allow-http-registry", result)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("cosign verify %s: %v\n%s", result, err, output)
		}
	}
	verifySignedReference(t, ctx, registryName, publicKey, imagecopy.CopyAllImages, true)
	assertRemoteManifestDigests(t, ctx, registryName, root, nil)

	wrongKeys, err := sigstore.GenerateKeyPair([]byte("wrong password"))
	if err != nil {
		t.Fatal(err)
	}
	wrongPublicKey := filepath.Join(t.TempDir(), "wrong.pub")
	if err := os.WriteFile(wrongPublicKey, wrongKeys.PublicKey, 0600); err != nil {
		t.Fatal(err)
	}
	verifySignedReference(t, ctx, registryName, wrongPublicKey, imagecopy.CopyAllImages, false)
}

func TestCopyRootSignsMultiPlatformIndexAndEveryInstance(t *testing.T) {
	ctx := context.Background()
	storeDir := filepath.Join(t.TempDir(), "images")
	source, root, manifests := multiPlatformImageFixture(t, ctx)
	if err := localstore.Put(ctx, storeDir, source, root, "signed-index:latest"); err != nil {
		t.Fatal(err)
	}
	passphrase := []byte("multi-platform password")
	keys, err := sigstore.GenerateKeyPair(passphrase)
	if err != nil {
		t.Fatal(err)
	}
	keyDir := t.TempDir()
	privateKey := filepath.Join(keyDir, "cosign.key")
	publicKey := filepath.Join(keyDir, "cosign.pub")
	for path, data := range map[string][]byte{privateKey: keys.PrivateKey, publicKey: keys.PublicKey} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("COSIGN_PASSWORD", string(passphrase))
	server := httptest.NewServer(registry.New())
	defer server.Close()
	registryAuthority := strings.TrimPrefix(server.URL, "http://")
	registryName := registryAuthority + "/coopr/signed-index:test"
	result, err := CopyRoot(ctx, oci.Image, storeDir, root, Destination{Transport: "registry", Name: registryName}, Options{
		TLSVerify: new(false), Signing: SigningOptions{SigstorePrivateKeyFile: privateKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSuffix(registryName, ":test") + "@" + root.Digest.String(); result != want {
		t.Fatalf("signed index result = %q, want %q", result, want)
	}

	verifySignedReference(t, ctx, registryName, publicKey, imagecopy.CopyAllImages, true)
	assertRemoteManifestDigests(t, ctx, registryName, root, manifests)
	for _, manifest := range manifests {
		digestReference := strings.TrimSuffix(registryName, ":test") + "@" + manifest.Digest.String()
		verifySignedReference(t, ctx, digestReference, publicKey, imagecopy.CopySystemImage, true)
	}
}

func verifySignedReference(t *testing.T, ctx context.Context, registryName, publicKey string, selection imagecopy.ImageListSelection, wantAllowed bool) {
	t.Helper()
	registriesDir, err := sigstoreAttachmentsConfig()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(registriesDir) }()
	system := &types.SystemContext{
		RegistriesDirPath: registriesDir, DockerInsecureSkipTLSVerify: types.OptionalBoolTrue,
		BigFilesTemporaryDir: t.TempDir(),
	}
	requirement, err := signature.NewPRSigstoreSignedKeyPath(publicKey, signature.NewPRMMatchRepository())
	if err != nil {
		t.Fatal(err)
	}
	policyContext, err := signature.NewPolicyContext(&signature.Policy{
		Default: signature.PolicyRequirements{requirement}, Transports: map[string]signature.PolicyTransportScopes{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = policyContext.Destroy() }()
	remote, err := docker.ParseReference("//" + registryName)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := layout.NewReference(filepath.Join(t.TempDir(), "verified"), "verified")
	if err != nil {
		t.Fatal(err)
	}
	_, err = imagecopy.Image(ctx, policyContext, verified, remote, &imagecopy.Options{
		SourceCtx: system, DestinationCtx: system, PreserveDigests: true,
		ImageListSelection: selection, RemoveSignatures: true,
	})
	if wantAllowed && err != nil {
		t.Fatalf("verify signed image %s: %v", registryName, err)
	}
	if !wantAllowed && err == nil {
		t.Fatalf("wrong public key accepted signed image %s", registryName)
	}
}

func verifyGPGSignedReference(t *testing.T, ctx context.Context, registryName, publicKey string, wantAllowed bool) {
	t.Helper()
	requirement, err := signature.NewPRSignedByKeyPath(signature.SBKeyTypeGPGKeys, publicKey, signature.NewPRMMatchRepository())
	if err != nil {
		t.Fatal(err)
	}
	policyContext, err := signature.NewPolicyContext(&signature.Policy{
		Default: signature.PolicyRequirements{requirement}, Transports: map[string]signature.PolicyTransportScopes{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = policyContext.Destroy() }()
	remote, err := docker.ParseReference("//" + registryName)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := layout.NewReference(filepath.Join(t.TempDir(), "verified"), "verified")
	if err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{DockerInsecureSkipTLSVerify: types.OptionalBoolTrue, BigFilesTemporaryDir: t.TempDir()}
	_, err = imagecopy.Image(ctx, policyContext, verified, remote, &imagecopy.Options{
		SourceCtx: system, DestinationCtx: system, PreserveDigests: true, RemoveSignatures: true,
	})
	if wantAllowed && err != nil {
		t.Fatalf("verify GPG-signed image %s: %v", registryName, err)
	}
	if !wantAllowed && err == nil {
		t.Fatalf("wrong GPG public key accepted signed image %s", registryName)
	}
}

func verifyGPGSignedReferenceInSubprocess(t *testing.T, ctx context.Context, registryName, publicKey string, wantAllowed bool) {
	t.Helper()
	command := exec.CommandContext(ctx, reexec.Self(), "-test.run=^TestGPGVerificationHelper$")
	wantAllowedValue := "0"
	if wantAllowed {
		wantAllowedValue = "1"
	}
	command.Env = append(os.Environ(),
		"COOPR_TEST_GPG_VERIFY_HELPER=1",
		"COOPR_TEST_GPG_REFERENCE="+registryName,
		"COOPR_TEST_GPG_PUBLIC_KEY="+publicKey,
		"COOPR_TEST_GPG_WANT_ALLOWED="+wantAllowedValue,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("verify GPG signature in isolated process: %v\n%s", err, output)
	}
}

func generateGPGKey(t *testing.T, ctx context.Context, home, identity, passphrase string) (string, string) {
	t.Helper()
	command := exec.CommandContext(ctx, "gpg", "--homedir", home, "--batch", "--pinentry-mode", "loopback", "--passphrase-fd", "0",
		"--quick-generate-key", identity, "rsa2048", "sign", "0")
	command.Stdin = strings.NewReader(passphrase)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate GPG key: %v\n%s", err, output)
	}
	command = exec.CommandContext(ctx, "gpg", "--homedir", home, "--batch", "--with-colons", "--list-secret-keys", identity)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("list generated GPG key: %v", err)
	}
	fingerprint := ""
	for line := range strings.SplitSeq(string(output), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) > 9 && fields[0] == "fpr" {
			fingerprint = fields[9]
			break
		}
	}
	if fingerprint == "" {
		t.Fatalf("generated GPG key has no fingerprint:\n%s", output)
	}
	publicKey := filepath.Join(t.TempDir(), "public-key.gpg")
	command = exec.CommandContext(ctx, "gpg", "--homedir", home, "--batch", "--output", publicKey, "--export", fingerprint)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("export GPG key %s: %v\n%s", fingerprint, err, output)
	}
	return fingerprint, publicKey
}

func assertRemoteManifestDigests(t *testing.T, ctx context.Context, registryName string, root v1.Descriptor, manifests []v1.Descriptor) {
	t.Helper()
	remote, err := docker.ParseReference("//" + registryName)
	if err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{DockerInsecureSkipTLSVerify: types.OptionalBoolTrue, BigFilesTemporaryDir: t.TempDir()}
	source, err := remote.NewImageSource(ctx, system)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	data, mediaType, err := source.GetManifest(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := digest.FromBytes(data); got != root.Digest || int64(len(data)) != root.Size || mediaType != root.MediaType {
		t.Fatalf("remote root changed: digest=%s size=%d type=%s, want %s/%d/%s", got, len(data), mediaType, root.Digest, root.Size, root.MediaType)
	}
	for _, manifest := range manifests {
		manifestDigest := manifest.Digest
		data, mediaType, err := source.GetManifest(ctx, &manifestDigest)
		if err != nil {
			t.Fatal(err)
		}
		if got := digest.FromBytes(data); got != manifest.Digest || int64(len(data)) != manifest.Size || mediaType != manifest.MediaType {
			t.Fatalf("remote manifest changed: digest=%s size=%d type=%s, want %s/%d/%s", got, len(data), mediaType, manifest.Digest, manifest.Size, manifest.MediaType)
		}
	}
}

func multiPlatformImageFixture(t *testing.T, ctx context.Context) (*orasoci.Store, v1.Descriptor, []v1.Descriptor) {
	return multiPlatformImageFixtureAt(t, ctx, t.TempDir())
}

func multiPlatformImageFixtureAt(t *testing.T, ctx context.Context, path string) (*orasoci.Store, v1.Descriptor, []v1.Descriptor) {
	t.Helper()
	store, err := orasoci.NewWithContext(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	manifests := make([]v1.Descriptor, 0, 2)
	for _, architecture := range []string{"amd64", "arm64"} {
		configData := []byte(fmt.Sprintf(`{"architecture":%q,"os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`, architecture))
		config := v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: digest.FromBytes(configData), Size: int64(len(configData))}
		if err := store.Push(ctx, config, bytes.NewReader(configData)); err != nil {
			t.Fatal(err)
		}
		manifestData, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config})
		if err != nil {
			t.Fatal(err)
		}
		platform := v1.Platform{OS: "linux", Architecture: architecture}
		manifest := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromBytes(manifestData), Size: int64(len(manifestData)), Platform: &platform}
		if err := store.Push(ctx, manifest, bytes.NewReader(manifestData)); err != nil {
			t.Fatal(err)
		}
		manifests = append(manifests, manifest)
	}
	indexData, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: manifests})
	if err != nil {
		t.Fatal(err)
	}
	root := v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: digest.FromBytes(indexData), Size: int64(len(indexData))}
	if err := store.Push(ctx, root, bytes.NewReader(indexData)); err != nil {
		t.Fatal(err)
	}
	return store, root, manifests
}
