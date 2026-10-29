package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/imagestore"
	"github.com/strio-dev/coopr/internal/oci"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage"
	"go.podman.io/common/libimage/manifests"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/signature"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/transports"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func TestCopyDecryptedIndexPreservesArtifactsAndMetadata(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers")
	}
	for _, fixture := range []struct {
		name, artifactType string
		files              bool
	}{{"typed", "application/vnd.coopr.test.artifact", true}, {"untyped", "", true}, {"untyped-empty", "", false}} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			store := nativeTestStore(t.TempDir())
			platform := v1.Platform{OS: "linux", Architecture: "amd64"}
			plaintext := nativeEmptyImageFixture(t, store, platform, "mixed-decrypted")
			sibling := nativeEmptyImageFixture(t, store, v1.Platform{OS: "linux", Architecture: "arm64"}, "mixed-sibling")
			policy := filepath.Join(t.TempDir(), "policy.json")
			if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			payload := []byte("mixed native artifact payload")
			var files []string
			if fixture.files {
				file := filepath.Join(t.TempDir(), "payload.txt")
				if err := os.WriteFile(file, payload, 0600); err != nil {
					t.Fatal(err)
				}
				files = []string{file}
			}
			var root v1.Descriptor
			var original []byte
			var expected v1.Index
			var artifact v1.Descriptor
			var artifactRaw, plaintextRaw []byte
			var nativeListID string
			err := buildah.WithStore(store, func(backend storage.Store) error {
				previous := imagestorage.Transport.GetStoreIfSet()
				imagestorage.Transport.SetStore(backend)
				defer imagestorage.Transport.SetStore(previous)
				native, err := libimage.RuntimeFromStore(backend, &libimage.RuntimeOptions{SystemContext: &types.SystemContext{SignaturePolicyPath: policy}})
				if err != nil {
					return err
				}
				list, err := native.CreateManifestList("mixed-decrypted")
				if err != nil {
					return err
				}
				nativeListID = list.ID()
				artifactDigest, err := list.AddArtifact(ctx, &libimage.ManifestListAddArtifactOptions{Type: &fixture.artifactType, ConfigType: "application/vnd.coopr.test.config", Config: `{"native":"config"}`, Annotations: map[string]string{"artifact": "retained"}}, files...)
				if err != nil {
					return err
				}
				raw, err := backend.ImageBigData(list.ID(), storage.ImageDigestBigDataKey)
				if err != nil {
					return err
				}
				var index v1.Index
				if err := json.Unmarshal(raw, &index); err != nil {
					return err
				}
				artifact = index.Manifests[0]
				plaintextRaw, err = backend.ImageBigData(plaintext.ImageID, storage.ImageDigestBigDataKey)
				if err != nil {
					return err
				}
				var encoded v1.Manifest
				if err := json.Unmarshal(plaintextRaw, &encoded); err != nil {
					return err
				}
				encoded.Layers = []v1.Descriptor{{MediaType: v1.MediaTypeImageLayerGzip + "+encrypted", Digest: digest.FromString("discarded ciphertext"), Size: 20}}
				encryptedRaw, err := json.Marshal(encoded)
				if err != nil {
					return err
				}
				encrypted := oci.Descriptor(v1.MediaTypeImageManifest, encryptedRaw)
				encrypted.Platform, encrypted.Annotations, encrypted.Data = &platform, map[string]string{"occurrence": "first"}, encryptedRaw
				if err := backend.SetImageBigData(plaintext.ImageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+encrypted.Digest.String(), encryptedRaw, func(raw []byte) (digest.Digest, error) { return digest.FromBytes(raw), nil }); err != nil {
					return err
				}
				duplicate := encrypted
				duplicate.Annotations = map[string]string{"occurrence": "duplicate"}
				other := sibling.Manifest
				other.Platform = &v1.Platform{OS: "linux", Architecture: "arm64"}
				index.Manifests = []v1.Descriptor{encrypted, artifact, duplicate, other}
				index.Annotations = map[string]string{"index": "retained"}
				original, err = json.Marshal(index)
				if err != nil {
					return err
				}
				root = oci.Descriptor(v1.MediaTypeImageIndex, original)
				lock, err := manifests.LockerForImage(backend, list.ID())
				if err != nil {
					return err
				}
				lock.Lock()
				_, updateErr := imagestore.FromStore(backend).UpdateStoredIndex(ctx, list.ID(), root, original, map[digest.Digest]string{encrypted.Digest: plaintext.ImageID, sibling.Manifest.Digest: sibling.ImageID}, "mixed-decrypted")
				lock.Unlock()
				if updateErr != nil {
					return updateErr
				}
				_, nativeList, err := manifests.LoadFromImage(backend, list.ID())
				if err != nil {
					return err
				}
				ref, err := nativeList.Reference(backend, imagecopy.CopySpecificImages, []digest.Digest{artifactDigest})
				if err != nil {
					return err
				}
				target, err := alltransports.ParseImageName("dir:" + filepath.Join(t.TempDir(), "native-selective"))
				if err != nil {
					return err
				}
				policyContext, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}})
				if err != nil {
					return err
				}
				_, copyErr := imagecopy.Image(ctx, policyContext, target, ref, &imagecopy.Options{ImageListSelection: imagecopy.CopySpecificImages, Instances: []digest.Digest{artifactDigest}, PreserveDigests: true})
				if err := errors.Join(copyErr, policyContext.Destroy()); err != nil {
					return err
				}
				source, err := target.NewImageSource(ctx, nil)
				if err != nil {
					return err
				}
				artifactRaw, _, err = source.GetManifest(ctx, &artifactDigest)
				if err := errors.Join(err, source.Close()); err != nil {
					return err
				}
				expected = index
				for _, i := range []int{0, 2} {
					expected.Manifests[i].Digest, expected.Manifests[i].Size, expected.Manifests[i].MediaType = plaintext.Manifest.Digest, plaintext.Manifest.Size, plaintext.Manifest.MediaType
					expected.Manifests[i].Data = plaintextRaw
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, selector := range []string{"mixed-decrypted", root.Digest.String()} {
				for _, transport := range []string{"oci", "oci-archive", "oci-dir"} {
					output := t.TempDir()
					destination := Destination{Transport: transport, Name: output, ArchiveReference: "test"}
					if transport == "oci-archive" {
						destination.Name = filepath.Join(output, "mixed.tar")
					}
					_, err := Copy(ctx, oci.Image, selector, destination, Options{BuildStore: store, SignaturePolicyPath: policy})
					if err != nil {
						t.Fatal(err)
					}
					emitted, err := oci.LayoutRoot(output)
					if transport == "oci-archive" {
						emitted, err = selectedCopyArchiveRoot(t, destination.Name), nil
					}
					if err != nil {
						t.Fatal(err)
					}
					raw := artifactOutputBlob(t, destination, emitted.Digest)
					var index v1.Index
					if err := json.Unmarshal(raw, &index); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(index, expected) {
						t.Fatalf("transformed mixed index metadata/members changed:\n got %s\nwant %+v", raw, expected)
					}
					got := artifactOutputBlob(t, destination, artifact.Digest)
					if !bytes.Equal(got, artifactRaw) {
						t.Fatal("artifact manifest changed")
					}
					var manifest v1.Manifest
					if err := json.Unmarshal(got, &manifest); err != nil {
						t.Fatal(err)
					}
					config := artifactOutputBlob(t, destination, manifest.Config.Digest)
					if string(config) != `{"native":"config"}` {
						t.Fatalf("artifact config changed: %s", config)
					}
					if fixture.files {
						got := artifactOutputBlob(t, destination, manifest.Layers[0].Digest)
						if !bytes.Equal(got, payload) {
							t.Fatal("artifact payload changed")
						}
					}
				}
			}
			reject := filepath.Join(t.TempDir(), "reject.json")
			if err := os.WriteFile(reject, []byte(`{"default":[{"type":"reject"}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			rejectedOutput := t.TempDir()
			if _, err := Copy(ctx, oci.Image, "mixed-decrypted", Destination{Transport: "oci", Name: rejectedOutput}, Options{BuildStore: store, SignaturePolicyPath: reject}); err == nil || !strings.Contains(err.Error(), "rejected by policy") {
				t.Fatalf("mixed native source policy bypass: %v", err)
			}
			if _, err := os.Stat(filepath.Join(rejectedOutput, "index.json")); !os.IsNotExist(err) {
				t.Fatalf("rejected mixed copy published an index: %v", err)
			}
			if fixture.name == "typed" {
				home, err := os.MkdirTemp("", "coopr-mixed-gpg-")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(home) })
				t.Setenv("GNUPGHOME", home)
				t.Cleanup(func() { _ = exec.Command("gpgconf", "--homedir", home, "--kill", "gpg-agent").Run() })
				fingerprint, _ := generateGPGKey(t, ctx, home, "Mixed decrypted list <mixed@example.invalid>", "")
				if err := signStoredImage(ctx, nativeListID, root.Digest, "localhost/mixed-decrypted:latest", Options{BuildStore: store, Signing: SigningOptions{SignBy: fingerprint}}); err != nil {
					t.Fatal(err)
				}
				if _, err := Copy(ctx, oci.Image, "mixed-decrypted", Destination{Transport: "dir", Name: filepath.Join(t.TempDir(), "signed")}, Options{BuildStore: store, SignaturePolicyPath: policy}); err == nil || !strings.Contains(err.Error(), "cannot change a signed stored index") {
					t.Fatalf("signed root was rewritten: %v", err)
				}
				if _, err := Copy(ctx, oci.Image, "mixed-decrypted", Destination{Transport: "oci", Name: t.TempDir()}, Options{BuildStore: store, SignaturePolicyPath: policy, Push: PushOptions{RemoveSignatures: true}}); err != nil {
					t.Fatalf("explicit signature removal did not permit mixed rewrite: %v", err)
				}
			}
			if err := buildah.WithStore(store, func(backend storage.Store) error {
				raw, err := backend.ImageBigData("localhost/mixed-decrypted:latest", storage.ImageDigestBigDataKey)
				if err == nil && !bytes.Equal(raw, original) {
					t.Fatal("copy rewrote native list head")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCopyDecryptedIndexDistinguishesMissingMembers(t *testing.T) {
	if testing.Short() {
		t.Skip("requires private native storage workers")
	}
	ctx := context.Background()
	for _, missing := range []bool{false, true} {
		name := "resident"
		if missing {
			name = "missing-sibling"
		}
		t.Run(name, func(t *testing.T) {
			store := nativeTestStore(t.TempDir())
			platform := v1.Platform{OS: "linux", Architecture: "amd64"}
			plaintext := nativeEmptyImageFixture(t, store, platform, "decrypted-index")
			sibling := nativeEmptyImageFixture(t, store, v1.Platform{OS: "linux", Architecture: "arm64"}, "decrypted-sibling")
			var root, original v1.Descriptor
			var nativeListID string
			nativePath := filepath.Join(t.TempDir(), "native-selected")
			nativeWholePath := filepath.Join(t.TempDir(), "native-whole")
			if err := buildah.WithStore(store, func(backend storage.Store) error {
				raw, err := backend.ImageBigData(plaintext.ImageID, storage.ImageDigestBigDataKey)
				if err != nil {
					return err
				}
				var manifest v1.Manifest
				if err := json.Unmarshal(raw, &manifest); err != nil {
					return err
				}
				manifest.Layers = []v1.Descriptor{{MediaType: v1.MediaTypeImageLayerGzip + "+encrypted", Digest: digest.FromString("ciphertext"), Size: 10}}
				raw, err = json.Marshal(manifest)
				if err != nil {
					return err
				}
				original = oci.Descriptor(v1.MediaTypeImageManifest, raw)
				original.Platform = &platform
				if err := backend.SetImageBigData(plaintext.ImageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+original.Digest.String(), raw, func(data []byte) (digest.Digest, error) { return digest.FromBytes(data), nil }); err != nil {
					return err
				}
				child := sibling.Manifest
				child.Platform = &v1.Platform{OS: "linux", Architecture: "arm64"}
				children := []v1.Descriptor{original, child}
				imageIDs := map[digest.Digest]string{original.Digest: plaintext.ImageID, child.Digest: sibling.ImageID}
				indexData, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: children})
				if err != nil {
					return err
				}
				root = oci.Descriptor(v1.MediaTypeImageIndex, indexData)
				if _, err := imagestore.FromStore(backend).WriteStoredIndex(ctx, root, indexData, imageIDs, "decrypted-list:latest"); err != nil {
					return err
				}
				if missing {
					if _, err := backend.DeleteImage(sibling.ImageID, true); err != nil {
						return err
					}
				}
				native, err := libimage.RuntimeFromStore(backend, nil)
				if err != nil {
					return err
				}
				listImage, _, err := native.LookupImage("decrypted-list:latest", &libimage.LookupImageOptions{ManifestList: true})
				if err != nil {
					return err
				}
				nativeListID = listImage.ID()
				if err := listImage.Tag("native-decrypted-alias:latest"); err != nil {
					return err
				}
				alias, err := backend.Image("localhost/native-decrypted-alias:latest")
				if err != nil {
					return err
				}
				if alias.ID != nativeListID {
					t.Fatalf("native Tag changed list identity: got=%s want=%s", alias.ID, nativeListID)
				}
				image, _, err := native.LookupImage("decrypted-list:latest", &libimage.LookupImageOptions{OS: platform.OS, Architecture: platform.Architecture})
				if err != nil {
					return err
				}
				if image.ID() != plaintext.ImageID {
					t.Fatalf("native lookup selected %s, want decrypted image %s", image.ID(), plaintext.ImageID)
				}
				source, err := image.StorageReference()
				if err != nil {
					return err
				}
				target, err := alltransports.ParseImageName("dir:" + nativePath)
				if err != nil {
					return err
				}
				policy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}})
				if err != nil {
					return err
				}
				defer func() {
					if err := policy.Destroy(); err != nil {
						t.Error(err)
					}
				}()
				_, err = imagecopy.Image(ctx, policy, target, source, &imagecopy.Options{ImageListSelection: imagecopy.CopySystemImage})
				if err != nil {
					return err
				}
				if !missing {
					control, err := native.CreateManifestList("plaintext-control")
					if err != nil {
						return err
					}
					for _, member := range []oci.StoredSelection{plaintext, sibling} {
						ref, err := imagestorage.Transport.NewStoreReference(backend, nil, member.ImageID)
						if err != nil {
							return err
						}
						if _, err := control.Add(ctx, transports.ImageName(ref), nil); err != nil {
							return err
						}
					}
					_, err = control.Push(ctx, "dir:"+nativeWholePath, &libimage.ManifestListPushOptions{ImageListSelection: imagecopy.CopyAllImages, CopyOptions: libimage.CopyOptions{ManifestMIMEType: v1.MediaTypeImageManifest}})
					return err
				}
				previous := imagestorage.Transport.GetStoreIfSet()
				imagestorage.Transport.SetStore(backend)
				defer imagestorage.Transport.SetStore(previous)
				_, list, err := manifests.LoadFromImage(backend, nativeListID)
				if err != nil {
					return err
				}
				allSource, err := list.Reference(backend, imagecopy.CopyAllImages, nil)
				if err != nil {
					return err
				}
				allTarget, err := alltransports.ParseImageName("dir:" + filepath.Join(t.TempDir(), "native-all"))
				if err != nil {
					return err
				}
				_, allErr := imagecopy.Image(ctx, policy, allTarget, allSource, &imagecopy.Options{ImageListSelection: imagecopy.CopyAllImages})
				if allErr == nil || !strings.Contains(allErr.Error(), "image not known") {
					t.Fatalf("native whole-list missing-member control = %v", allErr)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(nativePath, "manifest.json"))
			if err != nil || digest.FromBytes(raw) != plaintext.Manifest.Digest {
				t.Fatalf("native system-image copy did not emit plaintext: digest=%s want=%s err=%v", digest.FromBytes(raw), plaintext.Manifest.Digest, err)
			}
			for _, selector := range []string{"decrypted-list:latest", root.Digest.String()} {
				for _, explicit := range []bool{false, true} {
					if _, err := Copy(ctx, oci.Image, selector, Destination{Transport: "local", Name: "coopr-decrypted-alias:latest"}, Options{BuildStore: store, Platform: platform, PlatformExplicit: explicit}); err != nil {
						t.Fatalf("tag decrypted source %s (explicit=%t): %v", selector, explicit, err)
					}
					if err := buildah.WithStore(store, func(backend storage.Store) error {
						alias, err := backend.Image("localhost/coopr-decrypted-alias:latest")
						if err != nil {
							return err
						}
						want := nativeListID
						if explicit {
							want = plaintext.ImageID
						}
						if alias.ID != want {
							t.Fatalf("tag identity differs for %s (explicit=%t): got=%s want=%s", selector, explicit, alias.ID, want)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				output := filepath.Join(t.TempDir(), "coopr")
				_, err := Copy(ctx, oci.Image, selector, Destination{Transport: "dir", Name: output}, Options{BuildStore: store, Platform: platform})
				if missing {
					if err == nil || !strings.Contains(err.Error(), "image not known") {
						t.Fatalf("whole-list copy did not reject an unavailable original index member for %s: %v", selector, err)
					}
					if _, statErr := os.Stat(filepath.Join(output, "manifest.json")); !os.IsNotExist(statErr) {
						t.Fatalf("failed whole-list copy published a manifest: %v", statErr)
					}
					t.Logf("whole-list copy of %s rejects missing sibling: %v", selector, err)
					if _, err := Copy(ctx, oci.Image, selector, Destination{Transport: "dir", Name: output}, Options{BuildStore: store, Platform: platform, PlatformExplicit: true}); err != nil {
						t.Fatalf("explicit decrypted member copy failed for %s: %v", selector, err)
					}
				}
				if !missing && err != nil {
					t.Fatalf("copy resident plaintext for %s: %v", selector, err)
				}
				raw, err := os.ReadFile(filepath.Join(output, "manifest.json"))
				if !missing {
					var emitted v1.Index
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(raw, &emitted); err != nil {
						t.Fatal(err)
					}
					if emitted.MediaType != v1.MediaTypeImageIndex || len(emitted.Manifests) != 2 || emitted.Manifests[0].Digest != plaintext.Manifest.Digest || emitted.Manifests[1].Digest != sibling.Manifest.Digest {
						t.Fatalf("whole native list lost members: %s", raw)
					}
					control, err := os.ReadFile(filepath.Join(nativeWholePath, "manifest.json"))
					if err != nil {
						t.Fatal(err)
					}
					var nativeIndex v1.Index
					if err := json.Unmarshal(control, &nativeIndex); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(emitted.Manifests, nativeIndex.Manifests) {
						t.Fatalf("decrypted list members differ from native plaintext whole-list copy: got %s, native %s", raw, control)
					}
					archive := filepath.Join(t.TempDir(), "decrypted.tar")
					if _, err := Copy(ctx, oci.Image, selector, Destination{Transport: "oci-archive", Name: archive}, Options{BuildStore: store}); err != nil {
						t.Fatalf("archive whole transformed list: %v", err)
					}
					if archived := selectedCopyArchiveRoot(t, archive); archived.MediaType != v1.MediaTypeImageIndex {
						t.Fatalf("archive collapsed native list to %s", archived.MediaType)
					}
					continue
				}
				if err != nil || digest.FromBytes(raw) != plaintext.Manifest.Digest {
					t.Fatalf("Coopr copy did not emit selected plaintext: digest=%s want=%s err=%v", digest.FromBytes(raw), plaintext.Manifest.Digest, err)
				}
			}
			if !missing {
				if err := buildah.WithStore(store, func(backend storage.Store) error {
					data, err := backend.ImageBigData(nativeListID, storage.ImageDigestBigDataKey)
					if err != nil {
						return err
					}
					var newer v1.Index
					if err := json.Unmarshal(data, &newer); err != nil {
						return err
					}
					newer.Annotations = map[string]string{"head": "newer"}
					data, err = json.Marshal(newer)
					if err != nil {
						return err
					}
					lock, err := manifests.LockerForImage(backend, nativeListID)
					if err != nil {
						return err
					}
					lock.Lock()
					defer lock.Unlock()
					_, err = imagestore.FromStore(backend).UpdateStoredIndex(ctx, nativeListID, oci.Descriptor(v1.MediaTypeImageIndex, data), data, map[digest.Digest]string{original.Digest: plaintext.ImageID, sibling.Manifest.Digest: sibling.ImageID}, "decrypted-list:latest")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				output := filepath.Join(t.TempDir(), "historical")
				if _, err := Copy(ctx, oci.Image, root.Digest.String(), Destination{Transport: "dir", Name: output}, Options{BuildStore: store}); err != nil {
					t.Fatalf("historical transformed native list: %v", err)
				}
				raw, err := os.ReadFile(filepath.Join(output, "manifest.json"))
				if err != nil {
					t.Fatal(err)
				}
				var historical v1.Index
				if err := json.Unmarshal(raw, &historical); err != nil {
					t.Fatal(err)
				}
				if len(historical.Manifests) != 2 || historical.Annotations["head"] != "" || historical.Manifests[0].Digest != plaintext.Manifest.Digest || historical.Manifests[1].Digest != sibling.Manifest.Digest {
					t.Fatalf("historical root changed to mutable head or lost members: %s", raw)
				}
			}
		})
	}
	for _, origin := range []struct {
		name, arch, siblingArch string
		alternateFirst          bool
	}{
		{name: "native", arch: "amd64", siblingArch: "arm64"},
		{name: "literal-engine", arch: "amd64", siblingArch: "arm64"},
		{name: "qualified-engine", arch: "amd64", siblingArch: "arm64"},
		{name: "foreign", arch: "arm64", siblingArch: "amd64"},
		{name: "same-platform-alternate", arch: "amd64", siblingArch: "amd64", alternateFirst: true},
	} {
		t.Run("pulled-origin-leaf/"+origin.name, func(t *testing.T) {
			store := nativeTestStore(t.TempDir())
			sourceName := "origin-leaf:latest"
			if origin.name == "literal-engine" {
				sourceName = "docker-daemon:dev"
			}
			if origin.name == "qualified-engine" {
				sourceName = "docker-daemon:5000/name:tag"
			}
			platform := v1.Platform{OS: "linux", Architecture: origin.arch}
			leaf := nativeEmptyImageFixture(t, store, platform, "origin-leaf")
			sibling := nativeEmptyImageFixture(t, store, v1.Platform{OS: "linux", Architecture: origin.siblingArch}, "origin-sibling")
			first, second := leaf.Manifest, sibling.Manifest
			first.Platform = &platform
			second.Platform = &v1.Platform{OS: "linux", Architecture: origin.siblingArch}
			if origin.alternateFirst {
				first, second = second, first
			}
			raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{first, second}})
			if err != nil {
				t.Fatal(err)
			}
			root := oci.Descriptor(v1.MediaTypeImageIndex, raw)
			if err := nameNativeTestImage(ctx, store, sourceName, leaf); err != nil {
				t.Fatal(err)
			}
			if err := buildah.WithStore(store, func(backend storage.Store) error {
				if err := backend.SetImageBigData(leaf.ImageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+root.Digest.String(), raw, func(data []byte) (digest.Digest, error) { return digest.FromBytes(data), nil }); err != nil {
					return err
				}
				leaf.Root = root
				if err := oci.RecordStoredOrigin(ctx, backend, sourceName, leaf); err != nil {
					return err
				}
				native, err := libimage.RuntimeFromStore(backend, nil)
				if err != nil {
					return err
				}
				image, _, err := native.LookupImage(leaf.ImageID, &libimage.LookupImageOptions{ManifestList: true})
				if err != nil {
					return err
				}
				if image.ID() != leaf.ImageID {
					t.Fatalf("native lookup changed origin leaf identity: got=%s want=%s", image.ID(), leaf.ImageID)
				}
				return image.Tag("native-origin-alias:latest")
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := Copy(ctx, oci.Image, sourceName, Destination{Transport: "local", Name: "coopr-origin-alias:latest"}, Options{BuildStore: store}); err != nil {
				t.Fatal(err)
			}
			if err := buildah.WithStore(store, func(backend storage.Store) error {
				alias, err := backend.Image("localhost/coopr-origin-alias:latest")
				if err == nil && alias.ID != leaf.ImageID {
					t.Fatalf("local tag changed origin leaf identity: got=%s want=%s", alias.ID, leaf.ImageID)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			nativeOutput, cooprOutput := filepath.Join(t.TempDir(), "native-origin-export"), filepath.Join(t.TempDir(), "coopr-origin-export")
			if err := buildah.WithStore(store, func(backend storage.Store) error {
				native, err := libimage.RuntimeFromStore(backend, nil)
				if err != nil {
					return err
				}
				image, _, err := native.LookupImage(leaf.ImageID, nil)
				if err != nil {
					return err
				}
				source, err := image.StorageReference()
				if err != nil {
					return err
				}
				target, err := alltransports.ParseImageName("dir:" + nativeOutput)
				if err != nil {
					return err
				}
				policy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}})
				if err != nil {
					return err
				}
				defer func() {
					if err := policy.Destroy(); err != nil {
						t.Error(err)
					}
				}()
				_, err = imagecopy.Image(ctx, policy, target, source, &imagecopy.Options{ImageListSelection: imagecopy.CopySystemImage})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := Copy(ctx, oci.Image, sourceName, Destination{Transport: "dir", Name: cooprOutput}, Options{BuildStore: store}); err != nil {
				t.Fatal(err)
			}
			for _, output := range []string{nativeOutput, cooprOutput} {
				raw, err := os.ReadFile(filepath.Join(output, "manifest.json"))
				if err != nil || digest.FromBytes(raw) != leaf.Manifest.Digest {
					t.Fatalf("origin leaf export %s emitted %s, want %s: %v", output, digest.FromBytes(raw), leaf.Manifest.Digest, err)
				}
			}
		})
	}
}
