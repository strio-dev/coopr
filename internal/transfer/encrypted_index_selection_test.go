package transfer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/imagestore"
	"coopr/internal/oci"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/storage"
)

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
			var sibling oci.StoredSelection
			if missing {
				sibling = nativeEmptyImageFixture(t, store, v1.Platform{OS: "linux", Architecture: "arm64"}, "deleted-sibling")
			}
			var root, original v1.Descriptor
			var nativeListID string
			nativePath := filepath.Join(t.TempDir(), "native-selected")
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
				children := []v1.Descriptor{original}
				imageIDs := map[digest.Digest]string{original.Digest: plaintext.ImageID}
				if missing {
					child := sibling.Manifest
					child.Platform = &v1.Platform{OS: "linux", Architecture: "arm64"}
					children = append(children, child)
					imageIDs[child.Digest] = sibling.ImageID
				}
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
				return err
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
					if err == nil || !strings.HasPrefix(err.Error(), "stored index lacks manifest ") ||
						!strings.HasSuffix(err.Error(), original.Digest.String()) && !strings.HasSuffix(err.Error(), sibling.Manifest.Digest.String()) {
						t.Fatalf("whole-list copy did not reject an unavailable original index member for %s: %v", selector, err)
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
				if err != nil || digest.FromBytes(raw) != plaintext.Manifest.Digest {
					t.Fatalf("Coopr copy did not emit selected plaintext: digest=%s want=%s err=%v", digest.FromBytes(raw), plaintext.Manifest.Digest, err)
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
