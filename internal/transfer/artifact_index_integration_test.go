package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage"
	"go.podman.io/common/libimage/manifests"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/signature"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/storage"
)

func TestCopyNativeArtifactIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native artifact storage and transfer worker")
	}
	for _, artifactType := range []string{"application/vnd.coopr.test.artifact.v1", ""} {
		t.Run("type="+artifactType, func(t *testing.T) {
			ctx := context.Background()
			store := nativeTestStore(t.TempDir())
			payload := []byte("native artifact image push payload\n")
			config := []byte(`{"artifact":"config"}`)
			payloadPath := filepath.Join(t.TempDir(), "artifact.txt")
			if err := os.WriteFile(payloadPath, payload, 0600); err != nil {
				t.Fatal(err)
			}
			policy := filepath.Join(t.TempDir(), "policy.json")
			if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			control := t.TempDir()
			var artifact, root digest.Digest
			err := buildah.WithStore(store, func(backend storage.Store) error {
				previous := imagestorage.Transport.GetStoreIfSet()
				imagestorage.Transport.SetStore(backend)
				defer imagestorage.Transport.SetStore(previous)
				native, err := libimage.RuntimeFromStore(backend, &libimage.RuntimeOptions{})
				if err != nil {
					return err
				}
				list, err := native.CreateManifestList("artifact-list")
				if err != nil {
					return err
				}
				artifact, err = list.AddArtifact(ctx, &libimage.ManifestListAddArtifactOptions{Type: &artifactType, ConfigType: "application/vnd.coopr.test.config.v1+json", Config: string(config), Annotations: map[string]string{"artifact": "retained"}}, payloadPath)
				if err != nil {
					return err
				}
				root, err = list.Push(ctx, "oci:"+control+":test", &libimage.ManifestListPushOptions{ImageListSelection: imagecopy.CopyAllImages, CopyOptions: libimage.CopyOptions{SignaturePolicyPath: policy}})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, source := range []string{"artifact-list", root.String()} {
				t.Run(source, func(t *testing.T) {
					output := t.TempDir()
					_, err := Copy(ctx, oci.Image, source, Destination{Transport: "oci", Name: output, ArchiveReference: "test"}, Options{BuildStore: store, SignaturePolicyPath: policy, Push: PushOptions{Format: "oci"}})
					if err != nil {
						t.Fatal(err)
					}
					if exported, err := oci.LayoutRoot(output); err != nil || exported.Digest != root {
						t.Fatalf("exported root = %s, want %s: %v", exported.Digest, root, err)
					}
					for _, selected := range []digest.Digest{root, artifact} {
						want, err := os.ReadFile(filepath.Join(control, "blobs", selected.Algorithm().String(), selected.Encoded()))
						if err != nil {
							t.Fatal(err)
						}
						got, err := os.ReadFile(filepath.Join(output, "blobs", selected.Algorithm().String(), selected.Encoded()))
						if err != nil || !bytes.Equal(got, want) {
							t.Fatalf("exported %s differs from native control: %v", selected, err)
						}
					}
					raw, err := os.ReadFile(filepath.Join(output, "blobs", artifact.Algorithm().String(), artifact.Encoded()))
					if err != nil {
						t.Fatal(err)
					}
					var manifest v1.Manifest
					if err := json.Unmarshal(raw, &manifest); err != nil {
						t.Fatal(err)
					}
					if manifest.ArtifactType != artifactType || manifest.Annotations["artifact"] != "retained" || len(manifest.Layers) != 1 {
						t.Fatalf("artifact metadata changed: %s", raw)
					}
					for _, blob := range []struct {
						descriptor v1.Descriptor
						data       []byte
					}{{manifest.Config, config}, {manifest.Layers[0], payload}} {
						got, err := os.ReadFile(filepath.Join(output, "blobs", blob.descriptor.Digest.Algorithm().String(), blob.descriptor.Digest.Encoded()))
						if err != nil || !bytes.Equal(got, blob.data) {
							t.Fatalf("artifact payload/config %s changed: %v", blob.descriptor.Digest, err)
						}
					}
				})
			}
			t.Run("reject-policy", func(t *testing.T) {
				reject := filepath.Join(t.TempDir(), "reject.json")
				if err := os.WriteFile(reject, []byte(`{"default":[{"type":"reject"}]}`), 0600); err != nil {
					t.Fatal(err)
				}
				output := t.TempDir()
				_, err := Copy(ctx, oci.Image, "artifact-list", Destination{Transport: "oci", Name: output}, Options{BuildStore: store, SignaturePolicyPath: reject})
				if err == nil || !strings.Contains(err.Error(), "rejected by policy") {
					t.Fatalf("artifact source policy was bypassed: %v", err)
				}
				if _, err := os.Stat(filepath.Join(output, "index.json")); !os.IsNotExist(err) {
					t.Fatalf("rejected copy published destination index: %v", err)
				}
				if err := buildah.WithStore(store, func(backend storage.Store) error {
					previous := imagestorage.Transport.GetStoreIfSet()
					imagestorage.Transport.SetStore(backend)
					defer imagestorage.Transport.SetStore(previous)
					_, list, err := manifests.LoadFromImage(backend, "localhost/artifact-list:latest")
					if err != nil {
						return err
					}
					source, err := list.Reference(backend, imagecopy.CopyAllImages, nil)
					if err != nil {
						return err
					}
					destination, err := alltransports.ParseImageName("oci:" + t.TempDir() + ":test")
					if err != nil {
						return err
					}
					policy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{signature.NewPRReject()}})
					if err != nil {
						return err
					}
					defer func() {
						if err := policy.Destroy(); err != nil {
							t.Error(err)
						}
					}()
					_, nativeErr := imagecopy.Image(ctx, policy, destination, source, &imagecopy.Options{ImageListSelection: imagecopy.CopyAllImages})
					if nativeErr == nil || !strings.Contains(nativeErr.Error(), "rejected by policy") {
						t.Fatalf("native reject control: %v", nativeErr)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
			t.Run("encryption-options", func(t *testing.T) {
				key := filepath.Join(t.TempDir(), "missing-key.pem")
				_, err := Copy(ctx, oci.Image, "artifact-list", Destination{Transport: "oci", Name: t.TempDir()}, Options{BuildStore: store, SignaturePolicyPath: policy, Push: PushOptions{EncryptionKeys: []string{"jwe:" + key}}})
				if err == nil || !strings.Contains(err.Error(), "missing-key.pem") {
					t.Fatalf("encryption option/error lost: %v", err)
				}
			})
			t.Run("explicit-platform", func(t *testing.T) {
				_, err := Copy(ctx, oci.Image, "artifact-list", Destination{Transport: "oci", Name: t.TempDir()}, Options{BuildStore: store, SignaturePolicyPath: policy, PlatformExplicit: true, Platform: v1.Platform{OS: "linux", Architecture: "amd64"}})
				if err == nil || !strings.Contains(err.Error(), "platform not found") {
					t.Fatalf("artifact incorrectly selected as runnable platform: %v", err)
				}
			})
		})
	}
}
