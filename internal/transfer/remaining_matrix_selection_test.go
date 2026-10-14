package transfer

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/imagestore"
	"coopr/internal/localstore"
	"coopr/internal/oci"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage"
	"go.podman.io/common/libimage/manifests"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func TestCopyIncompleteIndexDoesNotSilentlySelectOneMember(t *testing.T) {
	if testing.Short() {
		t.Skip("requires private native storage workers")
	}
	ctx := context.Background()
	for _, selectorKind := range []string{"tag", "root-digest", "mixed-root-digest"} {
		t.Run(selectorKind, func(t *testing.T) {
			store := nativeTestStore(t.TempDir())
			root, rootData, members := signedIndexFixture(t, store, "incomplete-index")
			selector := "index:latest"
			if selectorKind != "tag" {
				selector = root.Digest.String()
			}
			var nativeErr error
			var systemCopyErr error
			var nativeIndexID string
			if err := buildah.WithStore(store, func(backend storage.Store) error {
				if selectorKind == "mixed-root-digest" {
					old, err := backend.Image("localhost/index:latest")
					if err != nil {
						return err
					}
					if _, err = backend.DeleteImage(old.ID, true); err != nil {
						return err
					}
					leaf := members["linux/amd64"]
					if err = backend.SetImageBigData(leaf.ImageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+root.Digest.String(), rootData, func([]byte) (digest.Digest, error) { return root.Digest, nil }); err != nil {
						return err
					}
					if err = backend.AddNames(leaf.ImageID, []string{"localhost/pulled-origin:latest"}); err != nil {
						return err
					}
					leaf.Root = root
					if err = oci.RecordStoredOrigin(ctx, backend, "pulled-origin:latest", leaf); err != nil {
						return err
					}
					ids := map[digest.Digest]string{}
					for _, member := range members {
						ids[member.Manifest.Digest] = member.ImageID
					}
					if _, err = imagestore.FromStore(backend).WriteStoredIndex(ctx, root, rootData, ids, "index:latest"); err != nil {
						return err
					}
					candidates, err := backend.ImagesByDigest(root.Digest)
					if err != nil {
						return err
					}
					if len(candidates) != 2 || candidates[0].ID != leaf.ImageID {
						t.Fatalf("mixed root fixture did not put retained leaf before native list: %+v", candidates)
					}
				}
				if _, err := backend.DeleteImage(members["linux/arm64"].ImageID, true); err != nil {
					return err
				}
				record, err := backend.Image("localhost/index:latest")
				if err != nil {
					return err
				}
				_, list, err := manifests.LoadFromImage(backend, record.ID)
				if err != nil {
					return err
				}
				source, err := list.Reference(backend, imagecopy.CopyAllImages, nil)
				if err != nil {
					return err
				}
				target, err := alltransports.ParseImageName("dir:" + filepath.Join(t.TempDir(), "native"))
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
				_, nativeErr = imagecopy.Image(ctx, policy, target, source, &imagecopy.Options{ImageListSelection: imagecopy.CopyAllImages})
				nativeIndexID = record.ID
				runtime, err := libimage.RuntimeFromStore(backend, nil)
				if err != nil {
					return err
				}
				image, _, err := runtime.LookupImage("index:latest", &libimage.LookupImageOptions{OS: "linux", Architecture: "amd64"})
				if err != nil {
					return err
				}
				systemSource, err := image.StorageReference()
				if err != nil {
					return err
				}
				systemTarget, err := alltransports.ParseImageName("dir:" + filepath.Join(t.TempDir(), "native-system"))
				if err != nil {
					return err
				}
				_, systemCopyErr = imagecopy.Image(ctx, policy, systemTarget, systemSource, &imagecopy.Options{ImageListSelection: imagecopy.CopySystemImage})
				listImage, _, err := runtime.LookupImage("index:latest", &libimage.LookupImageOptions{ManifestList: true})
				if err != nil {
					return err
				}
				if err = listImage.Tag("native-incomplete-alias:latest"); err != nil {
					return err
				}
				alias, err := backend.Image("localhost/native-incomplete-alias:latest")
				if err != nil {
					return err
				}
				if alias.ID != record.ID {
					t.Fatalf("native tag changed actual list record: got=%s want=%s", alias.ID, record.ID)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if systemCopyErr != nil {
				t.Fatalf("native system-image control failed: %v", systemCopyErr)
			}
			if nativeErr == nil || !strings.Contains(nativeErr.Error(), "image not known") {
				t.Fatalf("native all-image control did not reject missing member: %v", nativeErr)
			}
			if _, err := Copy(ctx, oci.Image, selector, Destination{Transport: "dir", Name: filepath.Join(t.TempDir(), "explicit-amd64")}, Options{BuildStore: store, Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, PlatformExplicit: true}); err != nil {
				t.Fatalf("explicit resident platform failed: %v", err)
			}
			if _, err := Copy(ctx, oci.Image, selector, Destination{Transport: "local", Name: "coopr-incomplete-alias:latest"}, Options{BuildStore: store}); err != nil {
				t.Fatalf("local alias of incomplete list failed whereas native Tag succeeds: %v", err)
			}
			if err := buildah.WithStore(store, func(backend storage.Store) error {
				alias, err := backend.Image("localhost/coopr-incomplete-alias:latest")
				if err != nil {
					return err
				}
				if alias.ID != nativeIndexID {
					t.Fatalf("local copy silently tagged member rather than actual list: got=%s want=%s", alias.ID, nativeIndexID)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			_, err := Copy(ctx, oci.Image, selector, Destination{Transport: "dir", Name: filepath.Join(t.TempDir(), "coopr")}, Options{BuildStore: store})
			if err == nil {
				t.Fatalf("copy of incomplete index %s silently emitted one member; native all-image copy rejects it: %v", selector, nativeErr)
			}
		})
	}
}

func TestCopyPulledLeafRetainsOriginWithoutBecomingManifestList(t *testing.T) {
	if testing.Short() {
		t.Skip("requires private native pull and loopback registry")
	}
	ctx := context.Background()
	source, root, _ := multiPlatformImageFixture(t, ctx)
	layout := filepath.Join(t.TempDir(), "layout")
	if err := localstore.Put(ctx, layout, source, root); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	name := strings.TrimPrefix(server.URL, "http://") + "/origin:latest"
	policyPath := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policyPath, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyRoot(ctx, oci.Image, layout, root, Destination{Transport: "registry", Name: name}, Options{TLSVerify: new(false), SignaturePolicyPath: policyPath}); err != nil {
		t.Fatal(err)
	}
	store := nativeTestStore(t.TempDir())
	id, err := buildah.PullImage(ctx, store, oci.Options{TLSVerify: new(false), SignaturePolicyPath: policyPath, PullPolicy: string(oci.PullAlways)}, name, v1.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	nativePath, cooprPath := filepath.Join(t.TempDir(), "native"), filepath.Join(t.TempDir(), "coopr")
	if err := buildah.WithStore(store, func(backend storage.Store) error {
		raw, err := backend.ImageBigData(id, storage.ImageDigestBigDataKey)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), `"manifests"`) {
			t.Fatalf("pulled record is an index rather than a leaf: %s", raw)
		}
		origin, _, indexed, err := oci.StoredImageIndex(ctx, backend, name)
		if err != nil {
			return err
		}
		if !indexed || origin.Digest != root.Digest {
			t.Fatalf("pulled leaf did not retain actual origin-index metadata: indexed=%t root=%s want=%s", indexed, origin.Digest, root.Digest)
		}
		runtime, err := libimage.RuntimeFromStore(backend, &libimage.RuntimeOptions{SystemContext: &types.SystemContext{SignaturePolicyPath: policyPath}})
		if err != nil {
			return err
		}
		_, err = runtime.Push(ctx, name, "dir:"+nativePath, &libimage.PushOptions{CopyOptions: libimage.CopyOptions{SignaturePolicyPath: policyPath}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(ctx, oci.Image, name, Destination{Transport: "dir", Name: cooprPath}, Options{BuildStore: store, SignaturePolicyPath: policyPath}); err != nil {
		t.Fatal(err)
	}
	native, err := os.ReadFile(filepath.Join(nativePath, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	coopr, err := os.ReadFile(filepath.Join(cooprPath, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(native, coopr) {
		t.Fatalf("pulled leaf copy differs from native system-image export:\nnative=%s\ncoopr=%s", native, coopr)
	}
}
