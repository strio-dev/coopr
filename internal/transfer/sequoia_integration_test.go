package transfer

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/localstore"
	"github.com/strio-dev/coopr/internal/oci"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/image"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/signature"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// These acceptance tests deliberately use the ordinary caller build and automatic
// helper discovery. Only the optional package's VM adds the real helper and sq to
// PATH; ordinary static builds need neither package to run their test suite.
func sequoiaAcceptanceContext(t *testing.T) context.Context {
	t.Helper()
	if testing.Short() {
		t.Skip("real optional Sequoia helper acceptance")
	}
	if buildah.NativeSequoia {
		t.Skip("requires caller without embedded Sequoia")
	}
	for _, name := range []string{buildah.SequoiaHelperName, "sq"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("optional %s is not installed: %v", name, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	if _, err := buildah.FindSequoiaHelper(ctx); err != nil {
		t.Fatal(err)
	}
	configureTestSignatureLookaside(t, t.TempDir())
	sequoiaAcceptanceHome(t)
	return ctx
}

// Keystore IPC uses Unix socket paths. Keep fixture homes short even when the
// enclosing test name and Nix TMPDIR are long.
func sequoiaAcceptanceHome(t *testing.T) {
	t.Helper()
	home, err := os.MkdirTemp("", "sq-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("SEQUOIA_HOME", home)
}

type sequoiaAcceptanceKey struct{ fingerprint, certificate, password string }

func sequoiaKeyFixture(t *testing.T, ctx context.Context, protected bool) sequoiaAcceptanceKey {
	t.Helper()
	dir := t.TempDir()
	key := sequoiaAcceptanceKey{}
	secret := filepath.Join(dir, "secret.pgp")
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, "sq", append([]string{"--home", os.Getenv("SEQUOIA_HOME")}, args...)...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("sq %s: %v\n%s", args[0], err, output)
		}
		return output
	}
	args := []string{"key", "generate", "--own-key", "--email", "coopr-test@example.invalid", "--profile", "rfc4880", "--output", secret, "--rev-cert", filepath.Join(dir, "revoke.pgp")}
	if protected {
		key.password = filepath.Join(dir, "password")
		if err := os.WriteFile(key.password, []byte("sequoia acceptance password"), 0600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "--new-password-file", key.password)
	} else {
		args = append(args, "--without-password")
	}
	run(args...)
	output := run("inspect", secret)
	match := regexp.MustCompile(`Fingerprint:\s+([0-9A-F]{40,64})`).FindSubmatch(output)
	if len(match) != 2 {
		t.Fatalf("sq inspect has no fingerprint: %s", output)
	}
	key.fingerprint = string(match[1])
	run("key", "import", secret)
	key.certificate = filepath.Join(dir, "certificate.pgp")
	run("cert", "export", "--cert", key.fingerprint, "--output", key.certificate)
	return key
}

// Check signedBy policy as a consumer would, then independently check every
// supplied root/member signature against its precise identity and manifest bytes.
func assertSequoiaSignedReference(t *testing.T, ctx context.Context, name string, key sequoiaAcceptanceKey, members []v1.Descriptor) {
	t.Helper()
	match, err := signature.NewPRMExactReference(name)
	if err != nil {
		t.Fatal(err)
	}
	requirement, err := signature.NewPRSignedByKeyPath(signature.SBKeyTypeGPGKeys, key.certificate, match)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{requirement}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := policy.Destroy(); err != nil {
			t.Errorf("cleanup policy: %v", err)
		}
	}()
	remote, err := docker.ParseReference("//" + name)
	if err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{DockerInsecureSkipTLSVerify: types.OptionalBoolTrue, BigFilesTemporaryDir: t.TempDir()}
	source, err := remote.NewImageSource(ctx, system)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := source.Close(); err != nil {
			t.Errorf("cleanup source: %v", err)
		}
	}()
	certificate, err := os.ReadFile(key.certificate)
	if err != nil {
		t.Fatal(err)
	}
	mechanism, identities, err := signature.NewEphemeralGPGSigningMechanism(certificate)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := mechanism.Close(); err != nil {
			t.Errorf("cleanup mechanism: %v", err)
		}
	}()
	for _, member := range append([]v1.Descriptor{{}}, members...) {
		var instance *digest.Digest
		if member.Digest != "" {
			instance = &member.Digest
		}
		allowed, err := policy.IsRunningImageAllowed(ctx, image.UnparsedInstance(source, instance))
		if err != nil || !allowed {
			t.Fatalf("signedBy policy rejected %s %s: allowed=%t err=%v", name, member.Digest, allowed, err)
		}
		raw, _, err := source.GetManifest(ctx, instance)
		if err != nil {
			t.Fatal(err)
		}
		signatures, err := source.GetSignatures(ctx, instance)
		if err != nil || len(signatures) == 0 {
			t.Fatalf("missing signature for %s %s: %v", name, member.Digest, err)
		}
		for _, signed := range signatures {
			_, signer, err := signature.VerifyImageManifestSignatureUsingKeyIdentityList(signed, raw, name, mechanism, identities)
			if err != nil || signer != key.fingerprint {
				t.Fatalf("identity/digest/certificate verification: signer=%s expected=%s err=%v", signer, key.fingerprint, err)
			}
			if _, _, err := signature.VerifyImageManifestSignatureUsingKeyIdentityList(signed, append(append([]byte{}, raw...), ' '), name, mechanism, identities); err == nil {
				t.Fatal("changed manifest digest accepted")
			}
			if _, _, err := signature.VerifyImageManifestSignatureUsingKeyIdentityList(signed, raw, name+"-wrong", mechanism, identities); err == nil {
				t.Fatal("wrong registry identity accepted")
			}
			if _, _, err := signature.VerifyImageManifestSignatureUsingKeyIdentityList(signed, raw, name, mechanism, []string{strings.Repeat("0", 40)}); err == nil {
				t.Fatal("wrong signing key identity accepted")
			}
		}
	}
}

func TestSequoiaHelperSignsRegistryGraph(t *testing.T) {
	ctx := sequoiaAcceptanceContext(t)
	key := sequoiaKeyFixture(t, ctx, false)
	source, root, members := multiPlatformImageFixture(t, ctx)
	store := t.TempDir()
	if err := localstore.Put(ctx, store, source, root, "sequoia:latest"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	name := strings.TrimPrefix(server.URL, "http://") + "/coopr/sequoia:test"
	options := Options{TLSVerify: new(false), Signing: SigningOptions{SequoiaFingerprint: key.fingerprint, PassphraseFile: key.password}}
	t.Run("wrong-passphrase", func(t *testing.T) {
		sequoiaAcceptanceHome(t)
		protected := sequoiaKeyFixture(t, ctx, true)
		password := filepath.Join(t.TempDir(), "wrong-password")
		if err := os.WriteFile(password, []byte("incorrect"), 0600); err != nil {
			t.Fatal(err)
		}
		bad := options
		bad.Signing.SequoiaFingerprint = protected.fingerprint
		bad.Signing.PassphraseFile = password
		if _, err := CopyRoot(ctx, oci.Image, store, root, Destination{Transport: "registry", Name: name + "-bad-password"}, bad); err == nil {
			t.Fatal("wrong passphrase accepted")
		} else {
			if ctx.Err() != nil {
				t.Fatalf("wrong-passphrase attempt timed out instead of rejecting: %v", err)
			}
			if !strings.Contains(err.Error(), "creating signature") {
				t.Fatalf("wrong-passphrase failed outside signing: %v", err)
			}
			t.Logf("protected key rejected incorrect passphrase: %v", err)
		}
	})
	result, err := CopyRoot(ctx, oci.Image, store, root, Destination{Transport: "registry", Name: name}, options)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSuffix(name, ":test") + "@" + root.Digest.String(); result != want {
		t.Fatalf("result=%s want=%s", result, want)
	}
	assertRemoteManifestDigests(t, ctx, name, root, members)
	assertSequoiaSignedReference(t, ctx, name, key, members)
	wrong := sequoiaKeyFixture(t, ctx, false)
	verifyGPGSignedReference(t, ctx, name, wrong.certificate, false)
	for _, verification := range []struct {
		name, certificate, identity string
		allow                       bool
	}{
		{"helper-source-policy", key.certificate, name, true},
		{"helper-wrong-certificate", wrong.certificate, name, false},
		{"helper-wrong-identity", key.certificate, name + "-wrong", false},
	} {
		t.Run(verification.name, func(t *testing.T) {
			requirement := map[string]any{"type": "signedBy", "keyType": "GPGKeys", "keyPath": verification.certificate, "signedIdentity": map[string]string{"type": "exactReference", "dockerReference": verification.identity}}
			policyData, err := json.Marshal(map[string]any{"default": []any{requirement}})
			if err != nil {
				t.Fatal(err)
			}
			policyPath := filepath.Join(t.TempDir(), "policy.json")
			if err := os.WriteFile(policyPath, policyData, 0600); err != nil {
				t.Fatal(err)
			}
			remote, err := docker.ParseReference("//" + name)
			if err != nil {
				t.Fatal(err)
			}
			verifiedOptions := options
			verifiedOptions.SignaturePolicyPath = policyPath
			// Verify source signatures before replacing them with destination identities.
			verifiedOptions.Push.RemoveSignatures = true
			destinationName := strings.TrimSuffix(name, ":test") + "-" + verification.name + ":test"
			_, err = publishImageReferenceSelection(ctx, remote, root, destinationName, verifiedOptions, imagecopy.CopyAllImages)
			if verification.allow {
				if err != nil {
					t.Fatalf("helper source signedBy policy rejected valid graph: %v", err)
				}
				assertSequoiaSignedReference(t, ctx, destinationName, key, members)
			} else {
				if err == nil {
					t.Fatal("helper source signedBy policy accepted invalid certificate/identity")
				}
				if ctx.Err() != nil || !strings.Contains(err.Error(), "Source image rejected") {
					t.Fatalf("source-policy failure did not reject signature: %v", err)
				}
				t.Logf("helper source policy rejected: %v", err)
			}
		})
	}
	t.Run("protected-single-image", func(t *testing.T) {
		sequoiaAcceptanceHome(t)
		protected := sequoiaKeyFixture(t, ctx, true)
		imageSource, imageRoot := imageFixture(t, ctx)
		imageStore := t.TempDir()
		if err := localstore.Put(ctx, imageStore, imageSource, imageRoot, "protected:latest"); err != nil {
			t.Fatal(err)
		}
		protectedName := name + "-protected"
		protectedOptions := options
		protectedOptions.Signing = SigningOptions{SequoiaFingerprint: protected.fingerprint, PassphraseFile: protected.password}
		if _, err := CopyRoot(ctx, oci.Image, imageStore, imageRoot, Destination{Transport: "registry", Name: protectedName}, protectedOptions); err != nil {
			t.Fatal(err)
		}
		assertSequoiaSignedReference(t, ctx, protectedName, protected, nil)
	})
	t.Run("unknown-key", func(t *testing.T) {
		bad := options
		bad.Signing.SequoiaFingerprint = strings.Repeat("0", 40)
		if _, err := CopyRoot(ctx, oci.Image, store, root, Destination{Transport: "registry", Name: name + "-bad-key"}, bad); err == nil {
			t.Fatal("unknown signing key accepted")
		} else {
			if ctx.Err() != nil {
				t.Fatalf("unknown-key attempt timed out instead of rejecting: %v", err)
			}
			if !strings.Contains(err.Error(), "creating signature") {
				t.Fatalf("unknown-key failed outside signing: %v", err)
			}
			t.Logf("unknown signing key rejected: %v", err)
		}
	})
}

func TestSequoiaHelperSignsNativeRoutes(t *testing.T) {
	ctx := sequoiaAcceptanceContext(t)
	key := sequoiaKeyFixture(t, ctx, false)
	store := nativeTestStore(t.TempDir())
	root, _, selections := signedIndexFixture(t, store, "sequoia")
	selected := selections["linux/"+runtime.GOARCH]
	if err := nameNativeTestImage(ctx, store, "image:latest", selected); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	options := Options{BuildStore: store, TLSVerify: new(false), Signing: SigningOptions{SequoiaFingerprint: key.fingerprint}}
	for _, route := range []string{"image", "index", "manifest-all", "manifest-selected", "direct-native-copy"} {
		t.Run(route, func(t *testing.T) {
			name := strings.TrimPrefix(server.URL, "http://") + "/coopr/sequoia-" + route + ":test"
			destination := Destination{Transport: "registry", Name: name}
			var members []v1.Descriptor
			var err error
			switch route {
			case "image":
				_, err = Copy(ctx, oci.Image, "image:latest", destination, options)
			case "index":
				_, err = Copy(ctx, oci.Image, "index:latest", destination, options)
			case "manifest-all":
				_, err = PushManifestList(ctx, "index:latest", destination, options, true, nil)
			case "manifest-selected":
				// CopySpecificImages with no instances publishes only the list; the
				// destination must already contain its referenced member manifests.
				unsigned := options
				unsigned.Signing = SigningOptions{}
				if _, err := Copy(ctx, oci.Image, "index:latest", destination, unsigned); err != nil {
					t.Fatal(err)
				}
				_, err = PushManifestList(ctx, "index:latest", destination, options, false, nil)
			case "direct-native-copy":
				native := storedTransferOptionsForSequoia(t, selected.ImageID, selected.Manifest.Digest, destination, options)
				_, err = buildah.TransferStoredImageResultSupervised(ctx, native)
			}
			if err != nil {
				t.Fatal(err)
			}
			if route == "index" || route == "manifest-all" {
				for _, selection := range selections {
					members = append(members, selection.Manifest)
				}
				assertRemoteManifestDigests(t, ctx, name, root, members)
			}
			assertSequoiaSignedReference(t, ctx, name, key, members)
			if route == "manifest-selected" {
				remote, err := docker.ParseReference("//" + name)
				if err != nil {
					t.Fatal(err)
				}
				reader, err := remote.NewImageSource(ctx, &types.SystemContext{DockerInsecureSkipTLSVerify: types.OptionalBoolTrue})
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := reader.Close(); err != nil {
						t.Errorf("cleanup reader: %v", err)
					}
				}()
				for _, member := range selections {
					signatures, err := reader.GetSignatures(ctx, &member.Manifest.Digest)
					if err != nil || len(signatures) != 0 {
						t.Fatalf("root-only list push added member signatures: %s count=%d err=%v", member.Manifest.Digest, len(signatures), err)
					}
				}
				assertRemoteManifestDigests(t, ctx, name, root, nil)
			}
			if route == "image" || route == "direct-native-copy" {
				assertRemoteManifestDigests(t, ctx, name, selected.Manifest, nil)
			}
		})
	}
}

func storedTransferOptionsForSequoia(t *testing.T, imageID string, selected digest.Digest, destination Destination, options Options) buildah.StoredTransferOptions {
	t.Helper()
	native, cleanup, err := storedTransferOptions(imageID, selected, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	})
	native.Destination = destination.String()
	return native
}

func TestSequoiaHelperSignsTransformedManifest(t *testing.T) {
	ctx := sequoiaAcceptanceContext(t)
	key := sequoiaKeyFixture(t, ctx, false)
	source, _ := imageFixture(t, ctx)
	var layer bytes.Buffer
	writer := tar.NewWriter(&layer)
	payload := []byte("Sequoia signed compressed content\n")
	if err := writer.WriteHeader(&tar.Header{Name: "payload", Mode: 0644, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	layerDescriptor := oci.Descriptor(v1.MediaTypeImageLayer, layer.Bytes())
	configData, err := json.Marshal(v1.Image{Platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{layerDescriptor.Digest}}})
	if err != nil {
		t.Fatal(err)
	}
	config := oci.Descriptor(v1.MediaTypeImageConfig, configData)
	manifestData, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{layerDescriptor}})
	if err != nil {
		t.Fatal(err)
	}
	root := oci.Descriptor(v1.MediaTypeImageManifest, manifestData)
	for _, blob := range []struct {
		descriptor v1.Descriptor
		data       []byte
	}{{layerDescriptor, layer.Bytes()}, {config, configData}, {root, manifestData}} {
		if err := source.Push(ctx, blob.descriptor, bytes.NewReader(blob.data)); err != nil {
			t.Fatal(err)
		}
	}
	store := t.TempDir()
	if err := localstore.Put(ctx, store, source, root, "transform:latest"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	name := strings.TrimPrefix(server.URL, "http://") + "/coopr/sequoia-transform:test"
	result, err := CopyRoot(ctx, oci.Image, store, root, Destination{Transport: "registry", Name: name}, Options{TLSVerify: new(false), Push: PushOptions{Format: "docker", CompressionFormat: "gzip", ForceCompression: true}, Signing: SigningOptions{SequoiaFingerprint: key.fingerprint}})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := docker.ParseReference("//" + name)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := remote.NewImageSource(ctx, &types.SystemContext{DockerInsecureSkipTLSVerify: types.OptionalBoolTrue})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Errorf("cleanup reader: %v", err)
		}
	}()
	raw, mime, err := reader.GetManifest(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mime != manifest.DockerV2Schema2MediaType {
		t.Fatalf("transformed media type=%s", mime)
	}
	published := digest.FromBytes(raw)
	if published == root.Digest || !strings.HasSuffix(result, "@"+published.String()) {
		t.Fatalf("transformed result=%s source=%s published=%s", result, root.Digest, published)
	}
	var document manifest.Schema2
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.LayersDescriptors) != 1 || document.LayersDescriptors[0].MediaType != manifest.DockerV2Schema2LayerMediaType || document.LayersDescriptors[0].Digest == layerDescriptor.Digest {
		t.Fatalf("layer was not gzip transformed: %+v", document.LayersDescriptors)
	}
	assertSequoiaSignedReference(t, ctx, name, key, nil)
	t.Logf("signed transformed manifest %s (%s)", published, document.MediaType)
}

func TestSequoiaHelperNativeDestinationBoundaries(t *testing.T) {
	ctx := sequoiaAcceptanceContext(t)
	key := sequoiaKeyFixture(t, ctx, false)
	// Use the same real OpenPGP key through the pinned public GPG copier as
	// the destination oracle, independently of the optional helper protocol.
	gpgHome, err := os.MkdirTemp("", "sq-gpg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := exec.Command("gpgconf", "--homedir", gpgHome, "--kill", "gpg-agent").Run(); err != nil {
			t.Errorf("stop GPG agent: %v", err)
		}
		if err := os.RemoveAll(gpgHome); err != nil {
			t.Errorf("remove GPG home: %v", err)
		}
	})
	t.Setenv("GNUPGHOME", gpgHome)
	secret := filepath.Join(filepath.Dir(key.certificate), "secret.pgp")
	if output, err := exec.CommandContext(ctx, "gpg", "--batch", "--import", secret).CombinedOutput(); err != nil {
		t.Fatalf("import native control key: %v\n%s", err, output)
	}
	st := nativeTestStore(t.TempDir())
	selected := nativeEmptyImageFixture(t, st, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "sequoia-destinations")
	if err := nameNativeTestImage(ctx, st, "source:latest", selected); err != nil {
		t.Fatal(err)
	}
	options := Options{BuildStore: st, Signing: SigningOptions{SequoiaFingerprint: key.fingerprint}}
	nativeCopy := func(t *testing.T, destination Destination, identity string) error {
		t.Helper()
		return buildah.WithStore(st, func(backend storage.Store) error {
			src, err := imagestorage.Transport.NewStoreReference(backend, nil, selected.ImageID)
			if err != nil {
				return err
			}
			dst, err := alltransports.ParseImageName(destination.String())
			if err != nil {
				return err
			}
			policy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}})
			if err != nil {
				return err
			}
			defer func() {
				if err := policy.Destroy(); err != nil {
					t.Errorf("destroy native control policy: %v", err)
				}
			}()
			copyOptions := &imagecopy.Options{SignBy: key.fingerprint, DestinationCtx: &types.SystemContext{DockerDaemonHost: os.Getenv("DOCKER_HOST")}}
			if identity != "" {
				copyOptions.SignIdentity, err = reference.ParseNormalizedNamed(identity)
				if err != nil {
					return err
				}
			}
			_, err = imagecopy.Image(ctx, policy, dst, src, copyOptions)
			return err
		})
	}
	verify := func(t *testing.T, target, identity, certificate string, want bool) {
		t.Helper()
		ref, err := alltransports.ParseImageName(target)
		if err != nil {
			t.Fatal(err)
		}
		source, err := ref.NewImageSource(ctx, &types.SystemContext{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := source.Close(); err != nil {
				t.Errorf("close signed output: %v", err)
			}
		}()
		raw, _, err := source.GetManifest(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if digest.FromBytes(raw) != selected.Manifest.Digest {
			t.Fatal("signing changed source manifest")
		}
		blobs, err := source.GetSignatures(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(blobs) == 0 {
			t.Fatal("destination has no OpenPGP signature")
		}
		match, err := signature.NewPRMExactReference(identity)
		if err != nil {
			t.Fatal(err)
		}
		requirement, err := signature.NewPRSignedByKeyPath(signature.SBKeyTypeGPGKeys, certificate, match)
		if err != nil {
			t.Fatal(err)
		}
		policy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{requirement}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := policy.Destroy(); err != nil {
				t.Errorf("destroy verification policy: %v", err)
			}
		}()
		allowed, err := policy.IsRunningImageAllowed(ctx, image.UnparsedInstance(source, nil))
		if want && (err != nil || !allowed) {
			t.Fatalf("verify signed native destination: allowed=%t error=%v", allowed, err)
		}
		if !want && allowed {
			t.Fatal("wrong certificate accepted helper signature")
		}
	}
	foreign, nativeForeign := nativeTestStore(t.TempDir()), nativeTestStore(t.TempDir())
	identity := "localhost/sequoia-foreign:latest"
	destination := Destination{Transport: "containers-storage", Name: "[vfs@" + foreign.GraphRoot + "+" + foreign.RunRoot + "]" + identity}
	t.Run("named-foreign-storage", func(t *testing.T) {
		native := Destination{Transport: "containers-storage", Name: "[vfs@" + nativeForeign.GraphRoot + "+" + nativeForeign.RunRoot + "]" + identity}
		if err := nativeCopy(t, native, ""); err != nil {
			t.Fatalf("native named destination signing: %v", err)
		}
		verify(t, native.String(), identity, key.certificate, true)
		if _, err := Copy(ctx, oci.Image, "source:latest", destination, options); err != nil {
			t.Fatalf("real helper named destination signing: %v", err)
		}
		verify(t, destination.String(), identity, key.certificate, true)
		wrong := sequoiaKeyFixture(t, ctx, false)
		verify(t, destination.String(), identity, wrong.certificate, false)
	})
	t.Run("directory-capability-with-explicit-native-identity", func(t *testing.T) {
		target := Destination{Transport: "dir", Name: filepath.Join(t.TempDir(), "native-signed")}
		if err := nativeCopy(t, target, identity); err != nil {
			t.Fatalf("native directory signature capability: %v", err)
		}
		verify(t, target.String(), identity, key.certificate, true)
	})
	t.Run("directory-preserves-real-helper-signature", func(t *testing.T) {
		target := Destination{Transport: "dir", Name: filepath.Join(t.TempDir(), "helper-signed")}
		if _, err := Copy(ctx, oci.Image, identity, target, Options{BuildStore: foreign}); err != nil {
			t.Fatal(err)
		}
		verify(t, target.String(), identity, key.certificate, true)
	})
	// The daemon is a disposable local native destination. A real signing
	// request must fail for native signature capability, not daemon absence.
	daemon := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/_ping":
			w.Header().Set("API-Version", "1.48")
			w.Header().Set("OSType", "linux")
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images/load"):
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				// Native signature rejection aborts the uncommitted tar upload.
				t.Logf("aborted native Docker load: %v", err)
				return
			}
			if _, err := io.WriteString(w, "{}\n"); err != nil {
				t.Logf("cancelled native Docker response: %v", err)
			}
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	if err := daemon.Listener.Close(); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("/tmp", "sq-docker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(socketDir); err != nil {
			t.Errorf("remove Docker socket directory: %v", err)
		}
	})
	socket := filepath.Join(socketDir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	daemon.Listener = listener
	daemon.Start()
	t.Cleanup(daemon.Close)
	t.Setenv("DOCKER_HOST", "unix://"+socket)
	for _, transport := range []string{"dir", "oci", "oci-dir", "oci-archive", "docker-archive", "docker"} {
		t.Run("native-request-contract/"+transport, func(t *testing.T) {
			co := Destination{Transport: transport, Name: filepath.Join(t.TempDir(), "helper")}
			native := Destination{Transport: transport, Name: filepath.Join(t.TempDir(), "native")}
			if transport == "docker" {
				co.Name = "localhost/sequoia:latest"
				native.Name = co.Name
			}
			if transport == "oci-dir" {
				native.Transport = "oci"
			}
			nativeErr := nativeCopy(t, native, "")
			_, helperErr := Copy(ctx, oci.Image, "source:latest", co, options)
			if nativeErr == nil || helperErr == nil {
				t.Fatalf("native/helper unexpectedly accepted this frontend request: native=%v helper=%v", nativeErr, helperErr)
			}
			if ctx.Err() != nil {
				t.Fatalf("destination attempt timed out: native=%v helper=%v", nativeErr, helperErr)
			}
			matched := false
			for _, marker := range []string{"Cannot determine canonical Docker reference", "Pushing signatures for OCI images is not supported", "Storing signatures for docker tar files is not supported", "Storing signatures for docker daemon is not supported"} {
				if strings.Contains(nativeErr.Error(), marker) && strings.Contains(helperErr.Error(), marker) {
					matched = true
					break
				}
			}
			if !matched {
				t.Fatalf("failure did not reach matching native identity/signature destination boundary: native=%v helper=%v", nativeErr, helperErr)
			}
			t.Logf("real key native destination rejection: native=%v helper=%v", nativeErr, helperErr)
		})
	}
}
