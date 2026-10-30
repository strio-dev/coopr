package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/opencontainers/go-digest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/oci"
	"go.podman.io/common/libimage/manifests"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/signature"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func TestStoredSignatureActionsFollowNativeDestinations(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers and GPG")
	}
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("GNUPGHOME", home)
	t.Cleanup(func() { _ = exec.Command("gpgconf", "--homedir", home, "--kill", "gpg-agent").Run() })
	fingerprint, _ := generateGPGKey(t, ctx, home, "Stored actions <stored@example.invalid>", "")
	for _, transport := range []string{"local", "oci", "oci-dir", "oci-archive"} {
		for _, placement := range []string{"single", "index-root", "index-member"} {
			t.Run(transport+"/"+placement, func(t *testing.T) {
				st := nativeTestStore(t.TempDir())
				selected := nativeEmptyImageFixture(t, st, v1.Platform{OS: "linux", Architecture: "amd64"}, "stored-actions")
				selector := "source"
				var indexSelections map[string]oci.StoredSelection
				if placement == "single" {
					if err := nameNativeTestImage(ctx, st, selector, selected); err != nil {
						t.Fatal(err)
					}
				} else {
					root, _, selections := signedIndexFixture(t, st, "actions-index")
					indexSelections = selections
					selector = "index:latest"
					if placement == "index-member" {
						selected = selections["linux/amd64"]
					} else {
						if err := buildah.WithStore(st, func(backend storage.Store) error {
							image, err := backend.Image("localhost/index:latest")
							selected = oci.StoredSelection{Root: root, Manifest: root}
							if err == nil {
								selected.ImageID = image.ID
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := signStoredImage(ctx, selected.ImageID, selected.Manifest.Digest, "localhost/source:latest", Options{BuildStore: st, Signing: SigningOptions{SignBy: fingerprint}}); err != nil {
					t.Fatal(err)
				}

				if transport == "local" && placement == "index-root" {
					for _, member := range indexSelections {
						if err := signStoredImage(ctx, member.ImageID, member.Manifest.Digest, "localhost/source:latest", Options{BuildStore: st, Signing: SigningOptions{SignBy: fingerprint}}); err != nil {
							t.Fatal(err)
						}
					}
				}

				destination := Destination{Transport: transport, Name: filepath.Join(t.TempDir(), "output")}
				if transport == "local" {
					destination.Name = "localhost/removed:latest"
				} else {
					// Public native copying rejects GPG signature retention in OCI outputs.
					if err := buildah.WithStore(st, func(backend storage.Store) error {
						var src types.ImageReference
						src, err := imagestorage.Transport.NewStoreReference(backend, nil, selected.ImageID)
						if err != nil {
							return err
						}
						if placement != "single" {
							image, err := backend.Image("localhost/index:latest")
							if err != nil {
								return err
							}
							_, list, err := manifests.LoadFromImage(backend, image.ID)
							if err != nil {
								return err
							}
							src, err = list.Reference(backend, imagecopy.CopyAllImages, nil)
							if err != nil {
								return err
							}
							// The public list adapter drops index-root signatures;
							// seed its true signed root via a native directory copy.
							if placement == "index-root" {
								stage := filepath.Join(t.TempDir(), "signed-native-source")
								if _, err := Copy(ctx, oci.Image, selector, Destination{Transport: "dir", Name: stage}, Options{BuildStore: st}); err != nil {
									return err
								}
								src, err = alltransports.ParseImageName("dir:" + stage)
								if err != nil {
									return err
								}
							}
						}
						native := destination
						native.Name = filepath.Join(t.TempDir(), "native")
						if transport == "oci-dir" {
							native.Transport = "oci"
						}
						dst, err := alltransports.ParseImageName(native.String())
						if err != nil {
							return err
						}
						policy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}})
						if err != nil {
							return err
						}
						defer func() {
							if err := policy.Destroy(); err != nil {
								t.Errorf("destroy native policy: %v", err)
							}
						}()
						_, err = imagecopy.Image(ctx, policy, dst, src, &imagecopy.Options{ImageListSelection: imagecopy.CopyAllImages})
						if err == nil || !strings.Contains(strings.ToLower(err.Error()), "signature") {
							t.Fatalf("native signed OCI control did not reject retention: %v", err)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if _, err := Copy(ctx, oci.Image, selector, destination, Options{BuildStore: st}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "signature") {
						t.Fatalf("signed OCI output did not reject implicit removal: %v", err)
					}
				}
				if _, err := Copy(ctx, oci.Image, selector, destination, Options{BuildStore: st, Push: PushOptions{RemoveSignatures: true}}); err != nil {
					t.Fatal(err)
				}
				if err := buildah.WithStore(st, func(backend storage.Store) error {
					var ref types.ImageReference
					var err error
					if transport == "local" {
						ref, err = imagestorage.Transport.ParseStoreReference(backend, destination.Name)
					} else {
						out := destination
						if transport == "oci-dir" {
							out.Transport = "oci"
						}
						ref, err = alltransports.ParseImageName(out.String())
					}
					if err != nil {
						return err
					}
					source, err := ref.NewImageSource(ctx, &types.SystemContext{})
					if err != nil {
						return err
					}
					defer func() {
						if err := source.Close(); err != nil {
							t.Errorf("close native source: %v", err)
						}
					}()
					blobs, err := source.GetSignatures(ctx, nil)
					if err != nil {
						return err
					}
					if len(blobs) != 0 {
						t.Fatalf("explicit removal retained %d signatures", len(blobs))
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if transport == "local" && placement == "index-root" {
					if err := buildah.WithStore(st, func(backend storage.Store) error {
						for _, member := range indexSelections {
							state, err := oci.CaptureStoredSignatures(backend, member.ImageID)
							if err != nil {
								return err
							}
							raw, err := backend.ImageBigData(member.ImageID, storage.ImageDigestBigDataKey)
							if err != nil {
								return err
							}
							blobs, err := oci.StoredSignatureBlobs(state, member.Manifest.Digest, digest.FromBytes(raw) == member.Manifest.Digest)
							if err != nil {
								return err
							}
							if len(blobs) != 0 {
								t.Fatalf("local index removal retained %d member signatures", len(blobs))
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}

			})
		}
	}
}

func TestLocalSignatureRemovalPreservesSiblingManifest(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers and GPG")
	}
	ctx := context.Background()
	st := nativeTestStore(t.TempDir())
	home := t.TempDir()
	t.Setenv("GNUPGHOME", home)
	t.Cleanup(func() { _ = exec.Command("gpgconf", "--homedir", home, "--kill", "gpg-agent").Run() })
	key, _ := generateGPGKey(t, ctx, home, "Sibling removal <sibling@example.invalid>", "")
	path, older, newer := selectedSigningSharedConfigLayoutFixture(t, ctx)
	for _, selected := range []oci.StoredSelection{older, newer} {
		if _, err := buildah.ImportLayoutSupervised(ctx, st, path, selected.Manifest); err != nil {
			t.Fatal(err)
		}
	}
	for _, selected := range []oci.StoredSelection{older, newer} {
		if err := signStoredImage(ctx, selected.ImageID, selected.Manifest.Digest, "localhost/source:latest", Options{BuildStore: st, Signing: SigningOptions{SignBy: key}}); err != nil {
			t.Fatal(err)
		}
	}
	read := func(selected oci.StoredSelection) [][]byte {
		t.Helper()
		var blobs [][]byte
		if err := buildah.WithStore(st, func(backend storage.Store) error {
			raw, err := backend.ImageBigData(selected.ImageID, storage.ImageDigestBigDataKey)
			if err != nil {
				return err
			}
			state, err := oci.CaptureStoredSignatures(backend, selected.ImageID)
			if err != nil {
				return err
			}
			blobs, err = oci.StoredSignatureBlobs(state, selected.Manifest.Digest, digest.FromBytes(raw) == selected.Manifest.Digest)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return blobs
	}
	before := read(newer)
	if len(before) == 0 || len(read(older)) == 0 {
		t.Fatal("fixture is not signed")
	}
	if err := buildah.WithStore(st, func(backend storage.Store) error {
		raw, err := backend.Metadata(older.ImageID)
		if err != nil {
			return err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			return err
		}
		fields["coopr-removal-test"] = json.RawMessage(`{"retain":true}`)
		data, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		return backend.SetMetadata(older.ImageID, string(data))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(ctx, oci.Image, older.Manifest.Digest.String(), Destination{Transport: "local", Name: "localhost/removed:latest"}, Options{BuildStore: st, Push: PushOptions{RemoveSignatures: true}}); err != nil {
		t.Fatal(err)
	}
	if len(read(older)) != 0 {
		t.Fatal("selected manifest signature was retained")
	}
	after := read(newer)
	if len(after) != len(before) {
		t.Fatal("sibling signature count changed")
	}
	for i := range before {
		if !bytes.Equal(before[i], after[i]) {
			t.Fatal("sibling signature bytes changed")
		}
	}
	if err := buildah.WithStore(st, func(backend storage.Store) error {
		raw, err := backend.Metadata(older.ImageID)
		if err != nil {
			return err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			return err
		}
		if !bytes.Equal(fields["coopr-removal-test"], []byte(`{"retain":true}`)) {
			t.Fatal("unrelated native metadata changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
