package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/localstore"
	"coopr/internal/oci"

	"github.com/google/go-containerregistry/pkg/registry"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/signature/sigstore"
)

func TestParameterSigningDestinationValidation(t *testing.T) {
	options := SigningOptions{SigstoreParameterFile: "parameters.yaml"}
	if !options.Enabled() {
		t.Fatal("parameter signer disabled")
	}
	for _, transport := range []string{"registry", "containers-storage", "dir", "oci"} {
		if err := ValidateSigningDestination(oci.Image, Destination{Transport: transport}, options); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateSigningDestination(oci.Image, Destination{Transport: "local"}, options); err == nil {
		t.Fatal("accepted local parameter signer")
	}
}

func TestSequoiaAndParameterModesUseNativeCombinationRules(t *testing.T) {
	options := SigningOptions{SequoiaFingerprint: "fingerprint", SigstorePrivateKeyFile: "key", SigstoreParameterFile: "params"}
	if err := ValidateSigningDestination(oci.Image, Destination{Transport: "registry"}, options); err != nil {
		t.Fatalf("native multi-signer combination rejected: %v", err)
	}
	options.PassphraseFile = "password"
	if err := ValidateSigningDestination(oci.Image, Destination{Transport: "registry"}, options); err == nil {
		t.Fatal("ambiguous shared passphrase accepted")
	}
}

func TestParameterFileSignsRegistryGraph(t *testing.T) {
	if testing.Short() {
		t.Skip("disposable registry native signature roundtrip")
	}
	ctx := context.Background()
	source, root, manifests := multiPlatformImageFixture(t, ctx)
	store := t.TempDir()
	if err := localstore.Put(ctx, store, source, root, "parameter-signed:latest"); err != nil {
		t.Fatal(err)
	}
	parameters, publicKey := parameterSigningFixture(t)
	server := httptest.NewServer(registry.New())
	defer server.Close()
	name := strings.TrimPrefix(server.URL, "http://") + "/coopr/parameter:test"
	if _, err := CopyRoot(ctx, oci.Image, store, root, Destination{Transport: "registry", Name: name}, Options{TLSVerify: new(false), Signing: SigningOptions{SigstoreParameterFile: parameters}}); err != nil {
		t.Fatal(err)
	}
	verifySignedReference(t, ctx, name, publicKey, imagecopy.CopyAllImages, true)
	assertRemoteManifestDigests(t, ctx, name, root, manifests)
	for _, manifest := range manifests {
		verifySignedReference(t, ctx, strings.TrimSuffix(name, ":test")+"@"+manifest.Digest.String(), publicKey, imagecopy.CopySystemImage, true)
	}
	wrong, err := sigstore.GenerateKeyPair([]byte("wrong"))
	if err != nil {
		t.Fatal(err)
	}
	wrongPublicKey := filepath.Join(t.TempDir(), "wrong.pub")
	if err := os.WriteFile(wrongPublicKey, wrong.PublicKey, 0600); err != nil {
		t.Fatal(err)
	}
	verifySignedReference(t, ctx, name, wrongPublicKey, imagecopy.CopyAllImages, false)
}

func parameterSigningFixture(t *testing.T) (string, string) {
	t.Helper()
	keyDir := t.TempDir()
	passphrase := []byte("parameter signing test")
	keys, err := sigstore.GenerateKeyPair(passphrase)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, publicKey, password := filepath.Join(keyDir, "key"), filepath.Join(keyDir, "pub"), filepath.Join(keyDir, "password")
	for path, data := range map[string][]byte{privateKey: keys.PrivateKey, publicKey: keys.PublicKey, password: passphrase} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	parameters := filepath.Join(keyDir, "parameters.json")
	raw, err := json.Marshal(map[string]string{"privateKeyFile": privateKey, "privateKeyPassphraseFile": password})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parameters, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return parameters, publicKey
}

func TestParameterFileSignsNativeImageAndManifestWorkers(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers")
	}
	ctx := context.Background()
	store := nativeTestStore(t.TempDir())
	_, _, selections := signedIndexFixture(t, store, "parameter")
	selected := selections["linux/"+runtime.GOARCH]
	if err := nameNativeTestImage(ctx, store, "image:latest", selected); err != nil {
		t.Fatal(err)
	}
	parameters, publicKey := parameterSigningFixture(t)
	// The parameter file owns its passphrase, even if the shared fallback is wrong.
	t.Setenv("COSIGN_PASSWORD", "unused fallback password")
	server := httptest.NewServer(registry.New())
	defer server.Close()
	options := Options{BuildStore: store, TLSVerify: new(false), Signing: SigningOptions{SigstoreParameterFile: parameters}}
	for _, flow := range []string{"image", "index", "manifest", "combined"} {
		t.Run(flow, func(t *testing.T) {
			name := strings.TrimPrefix(server.URL, "http://") + "/coopr/parameter-" + flow + ":test"
			destination := Destination{Transport: "registry", Name: name}
			var err error
			switch flow {
			case "combined":
				combined := options
				combined.Signing.SigstorePrivateKeyFile = filepath.Join(filepath.Dir(parameters), "key")
				combined.Signing.PassphraseFile = filepath.Join(filepath.Dir(parameters), "password")
				_, err = Copy(ctx, oci.Image, "image:latest", destination, combined)
			case "image":
				_, err = Copy(ctx, oci.Image, "image:latest", destination, options)
			case "index":
				_, err = Copy(ctx, oci.Image, "index:latest", destination, options)
			case "manifest":
				_, err = PushManifestList(ctx, "index:latest", destination, options, true, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			verifySignedReference(t, ctx, name, publicKey, imagecopy.CopyAllImages, true)
			if flow == "index" || flow == "manifest" {
				for _, member := range selections {
					verifySignedReference(t, ctx, strings.TrimSuffix(name, ":test")+"@"+member.Manifest.Digest.String(), publicKey, imagecopy.CopySystemImage, true)
				}
			}
		})
	}
	// Coopr permits native destinations; upstream decides their signature support
	// and Docker identity requirements instead of a registry-only preflight.
	for _, transport := range []string{"dir", "oci"} {
		_, err := Copy(ctx, oci.Image, "image:latest", Destination{Transport: transport, Name: filepath.Join(t.TempDir(), "output")}, options)
		if err == nil || (!strings.Contains(err.Error(), "Docker reference") && !strings.Contains(err.Error(), "does not support")) {
			t.Fatalf("%s native signature destination error = %v", transport, err)
		}
	}

}

func TestParameterSigningContextAndWorkerCredentials(t *testing.T) {
	parameters, _ := parameterSigningFixture(t)
	paths, err := (SigningOptions{SigstoreParameterFile: parameters}).ContextArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{parameters, filepath.Join(filepath.Dir(parameters), "key"), filepath.Join(filepath.Dir(parameters), "password")} {
		if !slices.Contains(paths, path) {
			t.Errorf("credential missing from context artifacts: %s", path)
		}
	}
	t.Setenv("COSIGN_PASSWORD", "worker-secret-value")
	options, cleanup, err := storedTransferOptions("image", "", Options{BuildStore: nativeTestStore(t.TempDir()), Signing: SigningOptions{SigstoreParameterFile: parameters}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	}()
	data, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "worker-secret-value") {
		t.Fatal("passphrase leaked to worker JSON")
	}
	var decoded buildah.StoredTransferOptions
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SigstoreParameterFile != parameters {
		t.Fatal("parameter path lost in worker JSON")
	}
	file, err := os.Stat(decoded.SigstorePassphraseFile)
	if err != nil {
		t.Fatal(err)
	}
	if file.Mode().Perm() != 0600 {
		t.Fatalf("passphrase mode = %v", file.Mode())
	}
	passphrase, err := os.ReadFile(decoded.SigstorePassphraseFile)
	if err != nil || string(passphrase) != "worker-secret-value" {
		t.Fatalf("passphrase file err=%v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(decoded.SigstorePassphraseFile); !os.IsNotExist(err) {
		t.Fatalf("temporary passphrase survived cleanup: %v", err)
	}
}

func TestNativeSigningDestinationsAndLegacyCombinations(t *testing.T) {
	for _, transport := range []string{"registry", "containers-storage", "dir", "oci", "oci-archive", "docker-archive", "docker"} {
		for _, modes := range []SigningOptions{
			{SignBy: "gpg"},
			{SigstorePrivateKeyFile: "sigstore.key"},
			{SignBy: "gpg", SigstorePrivateKeyFile: "sigstore.key"},
			{SignBy: "gpg", SequoiaFingerprint: "sq"},
			{SequoiaFingerprint: "sq", SigstorePrivateKeyFile: "sigstore.key"},
		} {
			if err := ValidateSigningDestination(oci.Image, Destination{Transport: transport}, modes); err != nil {
				t.Errorf("%s modes=%+v: %v", transport, modes, err)
			}
			modes.PassphraseFile = "password"
			count := 0
			for _, value := range []string{modes.SignBy, modes.SequoiaFingerprint, modes.SigstorePrivateKeyFile} {
				if value != "" {
					count++
				}
			}
			err := ValidateSigningDestination(oci.Image, Destination{Transport: transport}, modes)
			if (err != nil) != (count > 1) {
				t.Errorf("%s shared passphrase modes=%+v: %v", transport, modes, err)
			}
		}
	}
}

func TestNativeCombinedGPGAndPrivateKeySigning(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers and disposable GPG key")
	}
	ctx := context.Background()
	store := nativeTestStore(t.TempDir())
	selected := nativeEmptyImageFixture(t, store, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "combined")
	if err := nameNativeTestImage(ctx, store, "image:latest", selected); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "gnupg")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GNUPGHOME", home)
	t.Cleanup(func() { _ = exec.Command("gpgconf", "--homedir", home, "--kill", "gpg-agent").Run() })
	fingerprint, gpgPublicKey := generateGPGKey(t, ctx, home, "Coopr Combined <combined@example.invalid>", "")
	parameters, publicKey := parameterSigningFixture(t)
	t.Setenv("COSIGN_PASSWORD", "parameter signing test")
	configureTestSignatureLookaside(t, filepath.Join(t.TempDir(), "lookaside"))
	server := httptest.NewServer(registry.New())
	defer server.Close()
	name := strings.TrimPrefix(server.URL, "http://") + "/coopr/combined:test"
	opts := Options{BuildStore: store, TLSVerify: new(false), Signing: SigningOptions{SignBy: fingerprint, SigstorePrivateKeyFile: filepath.Join(filepath.Dir(parameters), "key")}}
	for _, native := range []bool{false, true} {
		if native {
			if _, err := Copy(ctx, oci.Image, "image:latest", Destination{Transport: "registry", Name: name}, opts); err != nil {
				t.Fatal(err)
			}
		} else {
			source, root := imageFixture(t, ctx)
			layoutStore := t.TempDir()
			if err := localstore.Put(ctx, layoutStore, source, root, "combined:latest"); err != nil {
				t.Fatal(err)
			}
			if _, err := CopyRoot(ctx, oci.Image, layoutStore, root, Destination{Transport: "registry", Name: name}, opts); err != nil {
				t.Fatal(err)
			}
		}
		verifySignedReference(t, ctx, name, publicKey, imagecopy.CopyAllImages, true)
		verifyGPGSignedReferenceInSubprocess(t, ctx, name, gpgPublicKey, true)
	}
	for _, transport := range []string{"dir", "oci"} {
		_, err := Copy(ctx, oci.Image, "image:latest", Destination{Transport: transport, Name: filepath.Join(t.TempDir(), "output")}, opts)
		if err == nil || (!strings.Contains(err.Error(), "Docker reference") && !strings.Contains(err.Error(), "does not support")) {
			t.Fatalf("%s combined native destination error = %v", transport, err)
		}
	}

}

func TestNativeNamedContainerStorageSigning(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers")
	}
	ctx := context.Background()
	source := nativeTestStore(t.TempDir())
	selected := nativeEmptyImageFixture(t, source, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "storage-signing")
	if err := nameNativeTestImage(ctx, source, "image:latest", selected); err != nil {
		t.Fatal(err)
	}
	parameters, publicKey := parameterSigningFixture(t)
	for _, mode := range []string{"parameter", "private-key"} {
		t.Run(mode, func(t *testing.T) {
			foreign := nativeTestStore(t.TempDir())
			name := "localhost/signed:latest"
			destination, err := ParsePushDestination("containers-storage:[vfs@"+foreign.GraphRoot+"+"+foreign.RunRoot+"]"+name, oci.Image)
			if err != nil {
				t.Fatal(err)
			}
			signing := SigningOptions{SigstoreParameterFile: parameters}
			if mode == "private-key" {
				signing = SigningOptions{SigstorePrivateKeyFile: filepath.Join(filepath.Dir(parameters), "key"), PassphraseFile: filepath.Join(filepath.Dir(parameters), "password")}
			}
			if _, err := Copy(ctx, oci.Image, "image:latest", destination, Options{BuildStore: source, Signing: signing}); err != nil {
				t.Fatal(err)
			}
			identity, err := signature.NewPRMExactReference(name)
			if err != nil {
				t.Fatal(err)
			}
			// Stored-source preparation uses its immutable image ID. Bind the expected
			// signing identity explicitly while verifying the stored signature.
			requirement, err := signature.NewPRSigstoreSignedKeyPath(publicKey, identity)
			if err != nil {
				t.Fatal(err)
			}
			policy, err := json.Marshal(&signature.Policy{Default: signature.PolicyRequirements{requirement}, Transports: map[string]signature.PolicyTransportScopes{}})
			if err != nil {
				t.Fatal(err)
			}
			policyPath := filepath.Join(t.TempDir(), "policy.json")
			if err := os.WriteFile(policyPath, policy, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Copy(ctx, oci.Image, name, Destination{Transport: "oci", Name: filepath.Join(t.TempDir(), "verified")}, Options{BuildStore: foreign, SignaturePolicyPath: policyPath, Push: PushOptions{RemoveSignatures: true}}); err != nil {
				t.Fatalf("verify stored signature: %v", err)
			}
		})
	}
}

func TestSigningNeverBypassesNativeArchiveOrDockerCopy(t *testing.T) {
	ctx := context.Background()
	for _, graph := range []string{"image", "index", "docker-index"} {
		for _, transport := range []string{"oci-archive", "oci-dir", "dir", "docker"} {
			t.Run(graph+"/"+transport, func(t *testing.T) {
				source, root, members := multiPlatformImageFixture(t, ctx)
				if graph == "image" {
					root = members[0]
				}
				if graph == "docker-index" {
					raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: manifest.DockerV2ListMediaType, Manifests: members})
					if err != nil {
						t.Fatal(err)
					}
					root = oci.Descriptor(manifest.DockerV2ListMediaType, raw)
					if err := source.Push(ctx, root, bytes.NewReader(raw)); err != nil {
						t.Fatal(err)
					}
				}
				layout := t.TempDir()
				if err := localstore.Put(ctx, layout, source, root, "source:latest"); err != nil {
					t.Fatal(err)
				}
				destination := Destination{Transport: transport, Name: filepath.Join(t.TempDir(), "output")}
				if transport == "docker" {
					destination.Name = "coopr/signing:test"
				}
				missing := filepath.Join(t.TempDir(), "missing-parameters.yaml")
				_, err := CopyRoot(ctx, oci.Image, layout, root, destination, Options{Signing: SigningOptions{SigstoreParameterFile: missing}})
				if err == nil || !strings.Contains(err.Error(), "setting up signing using parameter file") {
					t.Fatalf("signing was bypassed: %v", err)
				}
				if transport != "docker" {
					parameters, _ := parameterSigningFixture(t)
					_, err := CopyRoot(ctx, oci.Image, layout, root, destination, Options{Signing: SigningOptions{SigstoreParameterFile: parameters}})
					if err == nil {
						t.Fatal("unnamed native destination unexpectedly succeeded with signing")
					}
				}
			})
		}
	}
}

func TestQuietParameterSignerPreservesDeviceInstructions(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native signing worker")
	}
	ctx := context.Background()
	store := nativeTestStore(t.TempDir())
	selected := nativeEmptyImageFixture(t, store, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "device")
	if err := nameNativeTestImage(ctx, store, "image:latest", selected); err != nil {
		t.Fatal(err)
	}
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/auth", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "device_authorization_endpoint": issuer + "/device", "code_challenge_methods_supported": []string{"S256"}})
		case "/device":
			_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "device", "user_code": "COOPR-DEVICE", "verification_uri": issuer + "/verify", "expires_in": 60})
		case "/token":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"expired_token"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	issuer = server.URL
	parameters := filepath.Join(t.TempDir(), "device-parameters.json")
	raw, err := json.Marshal(map[string]any{"fulcio": map[string]string{"fulcioURL": issuer + "/fulcio", "oidcMode": "deviceGrant", "oidcIssuerURL": issuer, "oidcClientID": "coopr-test"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parameters, raw, 0600); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(t.TempDir(), "interaction")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := output.Close(); err != nil {
			t.Error(err)
		}
	}()
	previous := os.Stderr
	os.Stderr = output
	defer func() { os.Stderr = previous }()
	for _, redirectedProgress := range []bool{false, true} {
		if err := output.Truncate(0); err != nil {
			t.Fatal(err)
		}
		if _, err := output.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		opts := Options{BuildStore: store, Signing: SigningOptions{SigstoreParameterFile: parameters}}
		var progress bytes.Buffer
		if redirectedProgress {
			opts.ProgressWriter = &progress
		}
		_, err = Copy(ctx, oci.Image, "image:latest", Destination{Transport: "dir", Name: filepath.Join(t.TempDir(), "output")}, opts)
		if err == nil || !strings.Contains(err.Error(), "expired_token") {
			t.Fatalf("device flow did not reach local token endpoint: %v", err)
		}
		data, err := os.ReadFile(output.Name())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "COOPR-DEVICE") || !strings.Contains(string(data), issuer+"/verify") {
			t.Fatalf("redirected-progress=%v signing hid required device instructions: %q", redirectedProgress, data)
		}
		if strings.Contains(progress.String(), "COOPR-DEVICE") {
			t.Fatal("authentication instructions were redirected into copy progress")
		}
	}
}
