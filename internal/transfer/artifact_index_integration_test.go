package transfer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"github.com/google/go-containerregistry/pkg/registry"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage"
	"go.podman.io/common/libimage/manifests"
	imagecopy "go.podman.io/image/v5/copy"
	imagereference "go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/pkg/blobinfocache/memory"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/signature/sigstore"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestNativeOCIArchiveSaveOptions(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers")
	}
	ctx := context.Background()
	store := nativeTestStore(t.TempDir())
	layoutPath := t.TempDir()
	layout, err := orasoci.NewWithContext(ctx, layoutPath)
	if err != nil {
		t.Fatal(err)
	}
	push := func(media string, data []byte) v1.Descriptor {
		desc := oci.Descriptor(media, data)
		if err := layout.Push(ctx, desc, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		return desc
	}
	var layer bytes.Buffer
	writer := tar.NewWriter(&layer)
	payload := []byte("native archive layer")
	if err := writer.WriteHeader(&tar.Header{Name: "payload", Mode: 0644, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	configData := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["` + digest.FromBytes(layer.Bytes()).String() + `"]}}`)
	config := push(v1.MediaTypeImageConfig, configData)
	layerDescriptor := push(v1.MediaTypeImageLayer, layer.Bytes())
	raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{layerDescriptor}})
	if err != nil {
		t.Fatal(err)
	}
	root := push(v1.MediaTypeImageManifest, raw)
	if err := localstore.CopyGraphLayout(ctx, layoutPath, layout, root); err != nil {
		t.Fatal(err)
	}
	id, err := buildah.ImportLayoutSupervised(ctx, store, layoutPath, root)
	if err != nil {
		t.Fatal(err)
	}
	selection := oci.StoredSelection{ImageID: id, Manifest: root, Root: root, ConfigData: configData}
	if err := nameNativeTestImage(ctx, store, "archive-options", selection); err != nil {
		t.Fatal(err)
	}
	member := root
	member.Platform = &v1.Platform{OS: "linux", Architecture: "amd64"}
	indexRaw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{member}})
	if err != nil {
		t.Fatal(err)
	}
	indexRoot := oci.Descriptor(v1.MediaTypeImageIndex, indexRaw)
	if err := nativeTestIndex(ctx, store, "archive-index", indexRoot, indexRaw, map[string]oci.StoredSelection{"linux/amd64": selection}); err != nil {
		t.Fatal(err)
	}
	dockerIndexRaw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: "application/vnd.docker.distribution.manifest.list.v2+json", Manifests: []v1.Descriptor{member}})
	if err != nil {
		t.Fatal(err)
	}
	dockerIndexRoot := oci.Descriptor("application/vnd.docker.distribution.manifest.list.v2+json", dockerIndexRaw)
	if err := nativeTestIndex(ctx, store, "archive-docker-index", dockerIndexRoot, dockerIndexRaw, map[string]oci.StoredSelection{"linux/amd64": selection}); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"archive-options", "archive-index", "archive-docker-index"} {
		for _, accept := range []*bool{nil, new(false), new(true)} {
			name := "default"
			if accept != nil {
				name = map[bool]string{false: "compressed", true: "uncompressed"}[*accept]
			}
			for _, transport := range []string{"oci-archive", "oci-dir"} {
				t.Run(source+"/"+name+"/"+transport, func(t *testing.T) {
					control := Destination{Transport: transport, Name: filepath.Join(t.TempDir(), "native"), ArchiveReference: "localhost/saved:latest"}
					if transport == "oci-dir" {
						control.Transport = "oci"
					}
					err := buildah.WithStore(store, func(backend storage.Store) error {
						previous := imagestorage.Transport.GetStoreIfSet()
						imagestorage.Transport.SetStore(backend)
						defer imagestorage.Transport.SetStore(previous)
						var ref types.ImageReference
						if source != "archive-options" {
							_, list, err := manifests.LoadFromImage(backend, "localhost/"+source+":latest")
							if err != nil {
								return err
							}
							ref, err = list.Reference(backend, imagecopy.CopyAllImages, nil)
							if err != nil {
								return err
							}
						} else {
							var err error
							ref, err = imagestorage.Transport.NewStoreReference(backend, nil, id)
							if err != nil {
								return err
							}
						}
						destination, err := alltransports.ParseImageName(control.String())
						if err != nil {
							return err
						}
						policy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}})
						if err != nil {
							return err
						}
						system := &types.SystemContext{}
						if accept != nil {
							system.OCIAcceptUncompressedLayers = *accept
						}
						_, copyErr := imagecopy.Image(ctx, policy, destination, ref, &imagecopy.Options{ImageListSelection: imagecopy.CopyAllImages, DestinationCtx: system})
						return errors.Join(copyErr, policy.Destroy())
					})
					if err != nil {
						t.Fatal(err)
					}
					output := Destination{Transport: transport, Name: filepath.Join(t.TempDir(), "coopr")}
					if _, err := Copy(ctx, oci.Image, source, output, Options{BuildStore: store, ArchiveReference: control.ArchiveReference, ArchiveUncompressed: accept}); err != nil {
						t.Fatal(err)
					}
					var exported, nativeRoot v1.Descriptor
					if transport == "oci-archive" {
						exported, nativeRoot = selectedCopyArchiveRoot(t, output.Name), selectedCopyArchiveRoot(t, control.Name)
					} else {
						var err error
						exported, err = oci.LayoutRoot(output.Name)
						if err != nil {
							t.Fatal(err)
						}
						nativeRoot, err = oci.LayoutRoot(control.Name)
						if err != nil {
							t.Fatal(err)
						}
					}
					if exported.Digest != nativeRoot.Digest || exported.Annotations[v1.AnnotationRefName] != control.ArchiveReference {
						t.Fatalf("archive root/name = %+v, native %+v", exported, nativeRoot)
					}
					if source == "archive-docker-index" && (exported.MediaType != v1.MediaTypeImageIndex || exported.Digest == dockerIndexRoot.Digest) {
						t.Fatalf("native Docker to OCI conversion lost: %+v", exported)
					}
					if source != "archive-options" {
						var index v1.Index
						if err := json.Unmarshal(artifactOutputBlob(t, output, exported.Digest), &index); err != nil {
							t.Fatal(err)
						}
						exported = index.Manifests[0]
					}
					var manifest v1.Manifest
					if err := json.Unmarshal(artifactOutputBlob(t, output, exported.Digest), &manifest); err != nil {
						t.Fatal(err)
					}
					wantUncompressed := accept != nil && *accept
					if (manifest.Layers[0].MediaType == v1.MediaTypeImageLayer) != wantUncompressed {
						t.Fatalf("native archive compression option lost: layer=%s", manifest.Layers[0].MediaType)
					}
					data := artifactOutputBlob(t, output, manifest.Layers[0].Digest)
					if !wantUncompressed {
						reader, err := gzip.NewReader(bytes.NewReader(data))
						if err != nil {
							t.Fatal(err)
						}
						data, err = io.ReadAll(reader)
						if err != nil {
							t.Fatal(err)
						}
						if err := reader.Close(); err != nil {
							t.Fatal(err)
						}
					}
					if !bytes.Equal(data, layer.Bytes()) {
						t.Fatal("archive changed layer payload")
					}
				})
			}
		}
	}
}

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
			nativeArchive := filepath.Join(t.TempDir(), "native.tar")
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
				if err != nil {
					return err
				}
				_, storedList, err := manifests.LoadFromImage(backend, list.ID())
				if err != nil {
					return err
				}
				ref, err := storedList.Reference(backend, imagecopy.CopyAllImages, nil)
				if err != nil {
					return err
				}
				destination, err := alltransports.ParseImageName("oci-archive:" + nativeArchive + ":test")
				if err != nil {
					return err
				}
				policyContext, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}})
				if err != nil {
					return err
				}
				_, copyErr := imagecopy.Image(ctx, policyContext, destination, ref, &imagecopy.Options{ImageListSelection: imagecopy.CopyAllImages})
				return errors.Join(copyErr, policyContext.Destroy())
			})
			if err != nil {
				t.Fatal(err)
			}
			if exported := selectedCopyArchiveRoot(t, nativeArchive); exported.Digest != root {
				t.Fatalf("native archive root = %s, want %s", exported.Digest, root)
			}
			for _, source := range []string{"artifact-list", root.String()} {
				t.Run(source, func(t *testing.T) {
					for _, transport := range []string{"oci", "oci-archive", "oci-dir"} {
						t.Run(transport, func(t *testing.T) {
							output := t.TempDir()
							destination := Destination{Transport: transport, Name: output, ArchiveReference: "test"}
							if transport == "oci-archive" {
								destination.Name = filepath.Join(output, "artifact.tar")
							}
							_, err := Copy(ctx, oci.Image, source, destination, Options{BuildStore: store, SignaturePolicyPath: policy, Push: PushOptions{Format: "oci"}})
							if err != nil {
								t.Fatal(err)
							}
							exported, err := oci.LayoutRoot(output)
							if transport == "oci-archive" {
								exported, err = selectedCopyArchiveRoot(t, destination.Name), nil
							}
							if err != nil || exported.Digest != root {
								t.Fatalf("exported root = %s, want %s: %v", exported.Digest, root, err)
							}
							if exported.Annotations[v1.AnnotationRefName] != "test" {
								t.Fatalf("parsed destination reference lost: %+v", exported)
							}
							for _, selected := range []digest.Digest{root, artifact} {
								want, err := os.ReadFile(filepath.Join(control, "blobs", selected.Algorithm().String(), selected.Encoded()))
								if err != nil {
									t.Fatal(err)
								}
								got := artifactOutputBlob(t, destination, selected)
								if !bytes.Equal(got, want) {
									t.Fatalf("exported %s differs from native control", selected)
								}
							}
							raw := artifactOutputBlob(t, destination, artifact)
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
								got := artifactOutputBlob(t, destination, blob.descriptor.Digest)
								if !bytes.Equal(got, blob.data) {
									t.Fatalf("artifact payload/config %s changed", blob.descriptor.Digest)
								}
							}
						})
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
			t.Run("signed-root", func(t *testing.T) {
				passphrase := []byte("artifact-signature-test")
				keys, err := sigstore.GenerateKeyPair(passphrase)
				if err != nil {
					t.Fatal(err)
				}
				privateKey := filepath.Join(t.TempDir(), "sigstore.key")
				if err := os.WriteFile(privateKey, keys.PrivateKey, 0600); err != nil {
					t.Fatal(err)
				}
				var id string
				nativeCopy := func(output string, options *imagecopy.Options) error {
					return buildah.WithStore(store, func(backend storage.Store) error {
						previous := imagestorage.Transport.GetStoreIfSet()
						imagestorage.Transport.SetStore(backend)
						defer imagestorage.Transport.SetStore(previous)
						image, err := backend.Image("localhost/artifact-list:latest")
						if err != nil {
							return err
						}
						id = image.ID
						_, list, err := manifests.LoadFromImage(backend, id)
						if err != nil {
							return err
						}
						source, err := list.Reference(backend, imagecopy.CopyAllImages, nil)
						if err != nil {
							return err
						}
						destination, err := alltransports.ParseImageName(output)
						if err != nil {
							return err
						}
						policy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}})
						if err != nil {
							return err
						}
						_, copyErr := imagecopy.Image(ctx, policy, destination, source, options)
						return errors.Join(copyErr, policy.Destroy())
					})
				}
				signed := filepath.Join(t.TempDir(), "signed")
				identity, err := imagereference.ParseNormalizedNamed("localhost/artifact-list:latest")
				if err != nil {
					t.Fatal(err)
				}
				if err := nativeCopy("dir:"+signed, &imagecopy.Options{ImageListSelection: imagecopy.CopyAllImages, SignBySigstorePrivateKeyFile: privateKey, SignSigstorePrivateKeyPassphrase: passphrase, SignIdentity: identity}); err != nil {
					t.Fatal(err)
				}
				blob, err := os.ReadFile(filepath.Join(signed, "signature-1"))
				if err != nil {
					t.Fatal(err)
				}
				if err := buildah.WithStore(store, func(backend storage.Store) error {
					raw, err := backend.ImageBigData(id, storage.ImageDigestBigDataKey)
					if err != nil {
						return err
					}
					if digest.FromBytes(raw) != root {
						t.Fatal("signed native root differs from selected root")
					}
					state, err := oci.CaptureStoredSignatures(backend, id)
					if err != nil {
						return err
					}
					return oci.WriteStoredManifestSignatures(backend, id, root, true, [][]byte{blob}, state)
				}); err != nil {
					t.Fatal(err)
				}
				capture := func() any {
					t.Helper()
					var data any
					if err := buildah.WithStore(store, func(backend storage.Store) error {
						state, err := oci.CaptureStoredSignatures(backend, id)
						if err != nil {
							return err
						}
						blobs, err := oci.StoredSignatureBlobs(state, root, true)
						if err != nil {
							return err
						}
						if len(blobs) == 0 {
							t.Fatal("artifact index has no signed root")
						}
						data = state
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					return data
				}
				before := capture()
				// The public list adapter currently drops non-simple signatures.
				// Native copy therefore silently accepts this Sigstore-signed root.
				if err := nativeCopy("oci:"+t.TempDir()+":test", &imagecopy.Options{ImageListSelection: imagecopy.CopyAllImages}); err != nil {
					t.Fatalf("native omitted-signature control: %v", err)
				}
				checkRetentionExports := func(label string) {
					t.Helper()
					for _, transport := range []string{"dir", "registry"} {
						t.Run(label+"/"+transport, func(t *testing.T) {
							output := filepath.Join(t.TempDir(), "output")
							destination := Destination{Transport: transport, Name: output}
							var writes atomic.Int32
							var server *httptest.Server
							if transport == "registry" {
								handler := registry.New()
								server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
									if r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodPut || r.Method == http.MethodDelete {
										writes.Add(1)
									}
									handler.ServeHTTP(w, r)
								}))
								defer server.Close()
								destination.Name = strings.TrimPrefix(server.URL, "http://") + "/coopr/artifact:test"
							}
							options := Options{BuildStore: store, SignaturePolicyPath: policy, TLSVerify: new(false)}
							_, err := Copy(ctx, oci.Image, "artifact-list", destination, options)
							if err == nil || !strings.Contains(err.Error(), "root signatures") {
								t.Errorf("artifact export did not reject hidden root signatures: %v", err)
							}
							if transport == "dir" {
								if _, err := os.Stat(output); !os.IsNotExist(err) {
									t.Errorf("rejected export created directory: %v", err)
								}
							} else if writes.Load() != 0 {
								t.Errorf("rejected export made %d registry writes", writes.Load())
							}
							options.Push.RemoveSignatures = true
							if _, err := Copy(ctx, oci.Image, "artifact-list", destination, options); err != nil {
								t.Fatal(err)
							}
							ref, err := alltransports.ParseImageName(destination.String())
							if err != nil {
								t.Fatal(err)
							}
							source, err := ref.NewImageSource(ctx, &types.SystemContext{DockerInsecureSkipTLSVerify: types.NewOptionalBool(true)})
							if err != nil {
								t.Fatal(err)
							}
							defer func() {
								if err := source.Close(); err != nil {
									t.Error(err)
								}
							}()
							raw, _, err := source.GetManifest(ctx, nil)
							if err != nil || digest.FromBytes(raw) != root {
								t.Fatalf("explicit removal changed root: %v", err)
							}
							raw, _, err = source.GetManifest(ctx, &artifact)
							if err != nil || digest.FromBytes(raw) != artifact {
								t.Fatalf("explicit removal changed artifact: %v", err)
							}
							var manifest v1.Manifest
							if err := json.Unmarshal(raw, &manifest); err != nil {
								t.Fatal(err)
							}
							for _, want := range []struct {
								descriptor v1.Descriptor
								data       []byte
							}{{manifest.Config, config}, {manifest.Layers[0], payload}} {
								reader, _, err := source.GetBlob(ctx, types.BlobInfo{Digest: want.descriptor.Digest, Size: want.descriptor.Size}, memory.New())
								if err != nil {
									t.Fatal(err)
								}
								data, readErr := io.ReadAll(reader)
								if err := errors.Join(readErr, reader.Close()); err != nil {
									t.Fatal(err)
								}
								if !bytes.Equal(data, want.data) {
									t.Fatal("explicit removal changed artifact configuration/payload")
								}
							}
							if transport == "dir" {
								files, err := filepath.Glob(filepath.Join(output, "signature-*"))
								if err != nil || len(files) != 0 {
									t.Fatalf("removed signatures remain: %v %v", files, err)
								}
							} else {
								response, err := http.Get(server.URL + "/v2/coopr/artifact/manifests/sha256-" + root.Encoded() + ".sig")
								if err != nil {
									t.Fatal(err)
								}
								if err := response.Body.Close(); err != nil {
									t.Fatal(err)
								}
								if response.StatusCode != http.StatusNotFound {
									t.Fatalf("removed root Sigstore attachment status=%d", response.StatusCode)
								}
							}
							if !reflect.DeepEqual(before, capture()) {
								t.Fatal("export changed stored source signatures")
							}
						})
					}
				}
				checkRetentionExports("sigstore-only")

				for _, transport := range []string{"oci", "oci-dir", "oci-archive"} {
					t.Run(transport, func(t *testing.T) {
						output := filepath.Join(t.TempDir(), "output")
						destination := Destination{Transport: transport, Name: output, ArchiveReference: "test"}
						if transport != "oci-dir" {
							var err error
							destination, err = ParsePushDestination(transport+":"+output+":test", oci.Image)
							if err != nil {
								t.Fatal(err)
							}
						}
						_, err := Copy(ctx, oci.Image, "artifact-list", destination, Options{BuildStore: store, SignaturePolicyPath: policy})
						if err == nil || !strings.Contains(strings.ToLower(err.Error()), "signature") {
							t.Errorf("signed artifact root silently lost on %s export: %v", transport, err)
						}
						if _, err := os.Stat(output); !os.IsNotExist(err) {
							t.Errorf("rejected signed export published destination: %v", err)
						}
						if _, err := Copy(ctx, oci.Image, "artifact-list", destination, Options{BuildStore: store, SignaturePolicyPath: policy, Push: PushOptions{RemoveSignatures: true}}); err != nil {
							t.Fatal(err)
						}
						if got := artifactOutputBlob(t, destination, root); digest.FromBytes(got) != root {
							t.Fatal("explicit removal changed artifact index root")
						}
						raw := artifactOutputBlob(t, destination, artifact)
						var manifest v1.Manifest
						if err := json.Unmarshal(raw, &manifest); err != nil {
							t.Fatal(err)
						}
						if !bytes.Equal(artifactOutputBlob(t, destination, manifest.Config.Digest), config) || !bytes.Equal(artifactOutputBlob(t, destination, manifest.Layers[0].Digest), payload) {
							t.Fatal("explicit removal changed artifact config/payload")
						}
						if !reflect.DeepEqual(before, capture()) {
							t.Fatal("export changed stored source signatures")
						}
					})
				}
				home, err := os.MkdirTemp("", "coopr-artifact-gpg-")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(home) })
				t.Setenv("GNUPGHOME", home)
				t.Cleanup(func() { _ = exec.Command("gpgconf", "--homedir", home, "--kill", "gpg-agent").Run() })
				fingerprint, _ := generateGPGKey(t, ctx, home, "Artifact GPG <artifact-gpg@example.invalid>", "")
				rootData, err := os.ReadFile(filepath.Join(control, "blobs", root.Algorithm().String(), root.Encoded()))
				if err != nil {
					t.Fatal(err)
				}
				mechanism, err := signature.NewGPGSigningMechanism()
				if err != nil {
					t.Fatal(err)
				}
				gpg, signErr := signature.SignDockerManifest(rootData, "localhost/artifact-list:latest", mechanism, fingerprint)
				if err := errors.Join(signErr, mechanism.Close()); err != nil {
					t.Fatal(err)
				}
				setRootSignatures := func(signatures [][]byte) {
					t.Helper()
					if err := buildah.WithStore(store, func(backend storage.Store) error {
						state, err := oci.CaptureStoredSignatures(backend, id)
						if err != nil {
							return err
						}
						return oci.WriteStoredManifestSignatures(backend, id, root, true, signatures, state)
					}); err != nil {
						t.Fatal(err)
					}
					before = capture()
				}
				setRootSignatures([][]byte{gpg, blob})
				checkRetentionExports("mixed-gpg-sigstore")
				setRootSignatures([][]byte{gpg})
				t.Run("supported-gpg", func(t *testing.T) {
					for _, mode := range []string{"native", "coopr"} {
						output := filepath.Join(t.TempDir(), mode)
						if mode == "native" {
							if err := nativeCopy("dir:"+output, &imagecopy.Options{ImageListSelection: imagecopy.CopyAllImages}); err != nil {
								t.Fatal(err)
							}
						} else if _, err := Copy(ctx, oci.Image, "artifact-list", Destination{Transport: "dir", Name: output}, Options{BuildStore: store, SignaturePolicyPath: policy}); err != nil {
							t.Fatal(err)
						}
						retained, err := os.ReadFile(filepath.Join(output, "signature-1"))
						if err != nil || !bytes.Equal(retained, gpg) {
							t.Fatalf("%s changed supported GPG root signature: %v", mode, err)
						}
						raw, err := os.ReadFile(filepath.Join(output, "manifest.json"))
						if err != nil || !bytes.Equal(raw, rootData) {
							t.Fatalf("%s changed signed root: %v", mode, err)
						}
						if !reflect.DeepEqual(before, capture()) {
							t.Fatal("GPG export changed stored source signatures")
						}
					}
				})
				// A historical signature contributor must not be lost merely
				// because the chosen current native list exposes its own GPG root.
				historical := nativeEmptyImageFixture(t, store, v1.Platform{OS: "linux", Architecture: "amd64"}, "artifact-signature-history")
				var historicalState any
				if err := buildah.WithStore(store, func(backend storage.Store) error {
					if err := backend.SetImageBigData(historical.ImageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+root.String(), rootData, func([]byte) (digest.Digest, error) { return root, nil }); err != nil {
						return err
					}
					state, err := oci.CaptureStoredSignatures(backend, historical.ImageID)
					if err != nil {
						return err
					}
					if err := oci.WriteStoredManifestSignatures(backend, historical.ImageID, root, false, [][]byte{blob}, state); err != nil {
						return err
					}
					historicalState, err = oci.CaptureStoredSignatures(backend, historical.ImageID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				checkRetentionExports("historical-root-contributor")
				if err := buildah.WithStore(store, func(backend storage.Store) error {
					state, err := oci.CaptureStoredSignatures(backend, historical.ImageID)
					if err == nil && !reflect.DeepEqual(historicalState, state) {
						t.Fatal("export changed historical signature contributor")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}

			})
		})
	}
}

func artifactOutputBlob(t *testing.T, destination Destination, selected digest.Digest) []byte {
	t.Helper()
	name := filepath.Join("blobs", selected.Algorithm().String(), selected.Encoded())
	if destination.Transport != "oci-archive" {
		data, err := os.ReadFile(filepath.Join(destination.Name, name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	file, err := os.Open(destination.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			t.Fatalf("archive lacks %s", name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == name {
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
	}
}
