package build

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/imagestore"
	"github.com/strio-dev/coopr/internal/oci"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage"
	"go.podman.io/common/libimage/manifests"
	imagecopy "go.podman.io/image/v5/copy"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/transports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func TestManifestAppendPreservesRepeatedResidentDescriptors(t *testing.T) {
	ctx := context.Background()
	options := nativeBuildTestStore(t.TempDir())
	err := buildah.WithStore(options, func(store storage.Store) error {
		makeSelection := func(arch string) oci.StoredSelection {
			config, _ := json.Marshal(v1.Image{Platform: v1.Platform{OS: "linux", Architecture: arch}, RootFS: v1.RootFS{Type: "layers"}, History: []v1.History{{CreatedBy: "retained history", EmptyLayer: true}}})
			cfg := oci.Descriptor(v1.MediaTypeImageConfig, config)
			data, _ := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg})
			desc := oci.Descriptor(v1.MediaTypeImageManifest, data)
			id := cfg.Digest.Encoded()
			_, err := store.CreateImage(id, nil, "", "", &storage.ImageOptions{BigData: []storage.ImageBigDataOption{{Key: cfg.Digest.String(), Data: config, Digest: cfg.Digest}, {Key: storage.ImageDigestBigDataKey, Data: data, Digest: desc.Digest}, {Key: storage.ImageDigestManifestBigDataNamePrefix + "-" + desc.Digest.String(), Data: data, Digest: desc.Digest}}})
			if err != nil {
				t.Fatal(err)
			}
			return oci.StoredSelection{Root: desc, Manifest: desc, ImageID: id, ConfigData: config}
		}
		amd, arm := makeSelection("amd64"), makeSelection("arm64")
		members := []v1.Descriptor{}
		for i := 0; i < 3; i++ {
			d := amd.Manifest
			d.Platform = &v1.Platform{OS: "linux", Architecture: "amd64"}
			d.Annotations = map[string]string{"occurrence": fmt.Sprint(i)}
			members = append(members, d)
		}
		raw, _ := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: members, Annotations: map[string]string{"root": "retained"}, Subject: &amd.Manifest, ArtifactType: "application/example"})
		root := oci.Descriptor(v1.MediaTypeImageIndex, raw)
		if _, err := imagestore.FromStore(store).WriteStoredIndex(ctx, root, raw, map[digest.Digest]string{amd.Manifest.Digest: amd.ImageID}, "repeated"); err != nil {
			return err
		}
		_, stored, existing, err := oci.StoredImageSelections(ctx, store, "repeated")
		if err != nil {
			return err
		}
		var index v1.Index
		if err := json.Unmarshal(stored, &index); err != nil {
			return err
		}
		for _, d := range index.Manifests {
			if _, err := oci.ResolveStoredImage(ctx, store, d.Digest.String(), *d.Platform); err != nil {
				return err
			}
		}
		t.Logf("resident descriptors=%d, selection map entries=%d; every descriptor resolves", len(index.Manifests), len(existing))
		_, appended, _, _, appendErr := appendManifest(ctx, "repeated", "oci", options, map[string]oci.StoredSelection{"linux/arm64": arm})
		if appendErr != nil {
			return appendErr
		}
		var actual v1.Index
		if err := json.Unmarshal(appended, &actual); err != nil {
			return err
		}
		if len(actual.Manifests) != 4 || !reflect.DeepEqual(actual.Manifests[:3], members) || actual.Manifests[3].Digest != arm.Manifest.Digest || !reflect.DeepEqual(actual.Annotations, index.Annotations) || !reflect.DeepEqual(actual.Subject, index.Subject) || actual.ArtifactType != index.ArtifactType {
			t.Fatalf("append lost descriptor occurrences or root metadata: %s", appended)
		}
		_, historical, _, err := oci.StoredImageSelections(ctx, store, root.Digest.String())
		if err != nil {
			return err
		}
		if !bytes.Equal(historical, raw) {
			t.Fatal("append changed historical root")
		}
		for _, selection := range []oci.StoredSelection{amd, arm} {
			data, err := store.ImageBigData(selection.ImageID, digest.FromBytes(selection.ConfigData).String())
			if err != nil {
				return err
			}
			if !bytes.Equal(data, selection.ConfigData) {
				t.Fatal("append changed raw config")
			}
		}
		if _, err := imagestore.FromStore(store).WriteStoredIndex(ctx, root, raw, map[digest.Digest]string{amd.Manifest.Digest: amd.ImageID}, "native-control"); err != nil {
			return err
		}
		runtime, err := libimage.RuntimeFromStore(store, &libimage.RuntimeOptions{SystemContext: &types.SystemContext{}})
		if err != nil {
			return err
		}
		handle, err := runtime.LookupManifestList("localhost/native-control:latest")
		if err != nil {
			return err
		}
		armRef, err := imagestorage.Transport.NewStoreReference(store, nil, arm.ImageID)
		if err != nil {
			return err
		}
		_, nativeErr := handle.Add(ctx, transports.ImageName(armRef), nil)
		if nativeErr != nil {
			return fmt.Errorf("native Add control failed: %w", nativeErr)
		}
		data, err := store.ImageBigData(handle.ID(), storage.ImageDigestBigDataKey)
		if err != nil {
			return err
		}
		var updated v1.Index
		if err := json.Unmarshal(data, &updated); err != nil {
			return err
		}
		if len(updated.Manifests) != 4 || !reflect.DeepEqual(updated.Manifests[:3], members) {
			t.Fatalf("native control lost occurrences: %s", data)
		}
		t.Logf("Coopr and public native Add each retain all three occurrences and append one descriptor")
		return appendErr
	})
	if err != nil {
		t.Fatalf("append complete resident list: %v", err)
	}
}

// This synthetic fixture models native storage after decryption: the runnable
// default and retained encrypted source share an image record. It exercises
// provenance and list mutation, not encryption or decryption algorithms.
func TestManifestAppendRetainsEncryptedOrigin(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%t", mixed), func(t *testing.T) {
			ctx := context.Background()
			options := nativeBuildTestStore(t.TempDir())
			err := buildah.WithStore(options, func(store storage.Store) error {
				makeSelection := func(arch, marker string) oci.StoredSelection {
					config, _ := json.Marshal(v1.Image{Platform: v1.Platform{OS: "linux", Architecture: arch}, RootFS: v1.RootFS{Type: "layers"}, History: []v1.History{{CreatedBy: marker, EmptyLayer: true}}})
					cfg := oci.Descriptor(v1.MediaTypeImageConfig, config)
					raw, _ := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg})
					desc := oci.Descriptor(v1.MediaTypeImageManifest, raw)
					id := cfg.Digest.Encoded()
					if _, err := store.CreateImage(id, nil, "", "", &storage.ImageOptions{BigData: []storage.ImageBigDataOption{
						{Key: cfg.Digest.String(), Data: config, Digest: cfg.Digest},
						{Key: storage.ImageDigestBigDataKey, Data: raw, Digest: desc.Digest},
						{Key: storage.ImageDigestManifestBigDataNamePrefix + "-" + desc.Digest.String(), Data: raw, Digest: desc.Digest},
					}}); err != nil {
						t.Fatal(err)
					}
					return oci.StoredSelection{Root: desc, Manifest: desc, ImageID: id, ConfigData: config}
				}
				amd := makeSelection("amd64", "original config history")
				arm := makeSelection("arm64", "appended")
				ppc := makeSelection("ppc64le", "repeated append")
				replacement := makeSelection("amd64", "explicit replacement")
				encryptedRaw, _ := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: oci.Descriptor(v1.MediaTypeImageConfig, amd.ConfigData), Layers: []v1.Descriptor{oci.Descriptor(v1.MediaTypeImageLayerGzip+"+encrypted", []byte("synthetic ciphertext"))}})
				origin := oci.Descriptor(v1.MediaTypeImageManifest, encryptedRaw)
				if err := store.SetImageBigData(amd.ImageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+origin.Digest.String(), encryptedRaw, func(data []byte) (digest.Digest, error) { return digest.FromBytes(data), nil }); err != nil {
					return err
				}
				members := []v1.Descriptor{origin, origin}
				if mixed {
					members[1] = amd.Manifest
				}
				for i := range members {
					members[i].Platform = &v1.Platform{OS: "linux", Architecture: "amd64"}
					members[i].Annotations = map[string]string{"occurrence": fmt.Sprint(i)}
				}
				original := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: members, Annotations: map[string]string{"root": "preserved"}, Subject: &origin, ArtifactType: "application/example"}
				raw, _ := json.Marshal(original)
				oldRoot := oci.Descriptor(v1.MediaTypeImageIndex, raw)
				if _, err := imagestore.FromStore(store).WriteStoredIndex(ctx, oldRoot, raw, map[digest.Digest]string{origin.Digest: amd.ImageID, amd.Manifest.Digest: amd.ImageID}, "encrypted-resident"); err != nil {
					return err
				}
				_, _, resident, err := oci.StoredImageSelections(ctx, store, "encrypted-resident")
				if err != nil {
					return err
				}
				selected := resident["linux/amd64"]
				if selected.Manifest.Digest != amd.Manifest.Digest || selected.SourceManifest == nil || selected.SourceManifest.Digest != origin.Digest {
					t.Fatalf("fixture did not expose runnable manifest and encrypted origin: %+v", selected)
				}
				if mixed {
					sibling, ok := resident[amd.Manifest.Digest.String()]
					if !ok || sibling.SourceManifest != nil || sibling.Manifest.Digest != amd.Manifest.Digest {
						t.Fatalf("fixture did not expose separate plaintext sibling: %+v", resident)
					}
				}
				for i, added := range []oci.StoredSelection{arm, ppc, replacement} {
					platform, err := added.Platform()
					if err != nil {
						return err
					}
					_, appended, result, _, err := appendManifest(ctx, "encrypted-resident", "oci", options, map[string]oci.StoredSelection{platform.OS + "/" + platform.Architecture: added})
					if err != nil {
						return err
					}
					var actual v1.Index
					if err := json.Unmarshal(appended, &actual); err != nil {
						return err
					}
					if !reflect.DeepEqual(actual.Annotations, original.Annotations) || !reflect.DeepEqual(actual.Subject, original.Subject) || actual.ArtifactType != original.ArtifactType {
						t.Fatalf("append %d changed root metadata: %s", i, appended)
					}
					if i < 2 {
						if len(actual.Manifests) != 3+i || !reflect.DeepEqual(actual.Manifests[:2], members) {
							t.Fatalf("append %d lost origin occurrences: %s", i, appended)
						}
						if got := result["linux/amd64"]; got.ImageID != amd.ImageID || got.SourceManifest == nil || got.SourceManifest.Digest != origin.Digest {
							t.Fatalf("append %d pruned retained origin selection: %+v", i, result)
						}
					} else {
						wantMembers := 3
						if mixed {
							wantMembers++
						}
						if len(actual.Manifests) != wantMembers {
							t.Fatalf("replacement lost a distinct sibling or retained old occurrences: %s", appended)
						}
						for _, member := range actual.Manifests {
							if member.Digest == origin.Digest {
								t.Fatalf("replacement retained encrypted origin: %s", appended)
							}
						}
						for key, selection := range result {
							if selection.ImageID == amd.ImageID && (!mixed || key != amd.Manifest.Digest.String() || selection.SourceManifest != nil) {
								t.Fatalf("replacement retained wrong lookup alias %s: %+v", key, selection)
							}
						}
						if mixed {
							if sibling, ok := result[amd.Manifest.Digest.String()]; !ok || sibling.Manifest.Digest != amd.Manifest.Digest || sibling.ImageID != amd.ImageID || sibling.SourceManifest != nil {
								t.Fatalf("replacement lost plaintext sibling lookup: %+v", result)
							}
							found := false
							for _, member := range actual.Manifests {
								found = found || member.Digest == amd.Manifest.Digest
							}
							if !found {
								t.Fatal("replacement lost plaintext sibling descriptor")
							}
						}
					}
					if _, _, _, err := oci.StoredImageSelections(ctx, store, "encrypted-resident"); err != nil {
						return err
					}
					_, historical, _, err := oci.StoredImageSelections(ctx, store, oldRoot.Digest.String())
					if err != nil {
						return err
					}
					if !bytes.Equal(historical, raw) {
						t.Fatal("append changed original root bytes")
					}
					config, err := store.ImageBigData(amd.ImageID, digest.FromBytes(amd.ConfigData).String())
					if err != nil {
						return err
					}
					if !bytes.Equal(config, amd.ConfigData) {
						t.Fatal("append changed original config/history")
					}
				}
				// The public native Add path is a control for retained source references.
				if _, err := imagestore.FromStore(store).WriteStoredIndex(ctx, oldRoot, raw, map[digest.Digest]string{origin.Digest: amd.ImageID, amd.Manifest.Digest: amd.ImageID}, "encrypted-native-control"); err != nil {
					return err
				}
				runtime, err := libimage.RuntimeFromStore(store, &libimage.RuntimeOptions{SystemContext: &types.SystemContext{}})
				if err != nil {
					return err
				}
				handle, err := runtime.LookupManifestList("localhost/encrypted-native-control:latest")
				if err != nil {
					return err
				}
				ref, err := imagestorage.Transport.NewStoreReference(store, nil, arm.ImageID)
				if err != nil {
					return err
				}
				if _, err := handle.Add(ctx, transports.ImageName(ref), nil); err != nil {
					return fmt.Errorf("native Add encrypted-origin control: %w", err)
				}
				nativeData, err := store.ImageBigData(handle.ID(), storage.ImageDigestBigDataKey)
				if err != nil {
					return err
				}
				var nativeIndex v1.Index
				if err := json.Unmarshal(nativeData, &nativeIndex); err != nil {
					return err
				}
				if len(nativeIndex.Manifests) != 3 || !reflect.DeepEqual(nativeIndex.Manifests[:2], members) {
					t.Fatalf("native Add lost encrypted-origin occurrences: %s", nativeData)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("append resident encrypted-origin list: %v", err)
			}
		})
	}
}

func TestManifestAppendPreservesNativeArtifacts(t *testing.T) {
	for _, artifactType := range []string{"application/vnd.coopr.test.artifact.v1", ""} {
		t.Run("type="+artifactType, func(t *testing.T) {
			ctx := context.Background()
			options := nativeBuildTestStore(t.TempDir())
			err := buildah.WithStore(options, func(store storage.Store) error {
				// Native artifact Reference consults the global storage transport.
				previousStore := imagestorage.Transport.GetStoreIfSet()
				imagestorage.Transport.SetStore(store)
				defer imagestorage.Transport.SetStore(previousStore)
				rootDir := t.TempDir()
				policy := filepath.Join(rootDir, "policy.json")
				if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
					return err
				}
				system := &types.SystemContext{SignaturePolicyPath: policy}
				runtime, err := libimage.RuntimeFromStore(store, &libimage.RuntimeOptions{SystemContext: system})
				if err != nil {
					return err
				}
				payload := []byte("native artifact payload\n")
				payloadPath := filepath.Join(rootDir, "artifact.txt")
				if err := os.WriteFile(payloadPath, payload, 0600); err != nil {
					return err
				}
				config := `{"native":"artifact config"}`
				var artifact digest.Digest
				for _, name := range []string{"artifact-append", "artifact-control"} {
					handle, err := runtime.CreateManifestList(name)
					if err != nil {
						return err
					}
					artifact, err = handle.AddArtifact(ctx, &libimage.ManifestListAddArtifactOptions{Type: &artifactType, ConfigType: "application/vnd.coopr.test.config.v1+json", Config: config, Annotations: map[string]string{"artifact": "retained"}}, payloadPath)
					if err != nil {
						return err
					}
				}
				original, err := store.ImageBigData("localhost/artifact-append:latest", storage.ImageDigestBigDataKey)
				if err != nil {
					return err
				}
				originalRoot := oci.Descriptor(v1.MediaTypeImageIndex, original)
				var originalIndex v1.Index
				if err := json.Unmarshal(original, &originalIndex); err != nil {
					return err
				}
				if len(originalIndex.Manifests) != 1 || originalIndex.Manifests[0].ArtifactType != artifactType {
					t.Fatalf("native artifact fixture: %s", original)
				}
				makeSelection := func(arch string) oci.StoredSelection {
					rawConfig, _ := json.Marshal(v1.Image{Platform: v1.Platform{OS: "linux", Architecture: arch}, RootFS: v1.RootFS{Type: "layers"}})
					cfg := oci.Descriptor(v1.MediaTypeImageConfig, rawConfig)
					raw, _ := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: cfg})
					desc := oci.Descriptor(v1.MediaTypeImageManifest, raw)
					id := cfg.Digest.Encoded()
					if _, err := store.CreateImage(id, nil, "", "", &storage.ImageOptions{BigData: []storage.ImageBigDataOption{
						{Key: cfg.Digest.String(), Data: rawConfig, Digest: cfg.Digest},
						{Key: storage.ImageDigestBigDataKey, Data: raw, Digest: desc.Digest},
						{Key: storage.ImageDigestManifestBigDataNamePrefix + "-" + desc.Digest.String(), Data: raw, Digest: desc.Digest},
					}}); err != nil {
						t.Fatal(err)
					}
					return oci.StoredSelection{Root: desc, Manifest: desc, ImageID: id, ConfigData: rawConfig}
				}
				amd, arm := makeSelection("amd64"), makeSelection("arm64")
				verifyExport := func(name string, wantMembers int) error {
					_, list, err := manifests.LoadFromImage(store, name)
					if err != nil {
						return err
					}
					files, err := list.Files(artifact)
					if err != nil || len(files) != 1 || files[0] != payloadPath {
						return fmt.Errorf("artifact file locations changed: %v, %v", files, err)
					}
					handle, err := runtime.LookupManifestList(name)
					if err != nil {
						return err
					}
					layout := t.TempDir()
					if _, err := handle.Push(ctx, "oci:"+layout+":exported", &libimage.ManifestListPushOptions{ImageListSelection: imagecopy.CopyAllImages, CopyOptions: libimage.CopyOptions{SignaturePolicyPath: policy}}); err != nil {
						return fmt.Errorf("native push %s: %w", name, err)
					}
					artifactRaw, err := os.ReadFile(filepath.Join(layout, "blobs", "sha256", artifact.Encoded()))
					if err != nil {
						return err
					}
					var manifest v1.Manifest
					if err := json.Unmarshal(artifactRaw, &manifest); err != nil {
						return err
					}
					if manifest.ArtifactType != artifactType || manifest.Annotations["artifact"] != "retained" || len(manifest.Layers) != 1 {
						return fmt.Errorf("artifact manifest changed: %s", artifactRaw)
					}
					for _, blob := range []struct {
						desc v1.Descriptor
						data []byte
					}{{manifest.Config, []byte(config)}, {manifest.Layers[0], payload}} {
						raw, err := os.ReadFile(filepath.Join(layout, "blobs", "sha256", blob.desc.Digest.Encoded()))
						if err != nil {
							return err
						}
						if !bytes.Equal(raw, blob.data) {
							return fmt.Errorf("artifact blob %s changed", blob.desc.Digest)
						}
					}
					data, err := store.ImageBigData(name, storage.ImageDigestBigDataKey)
					if err != nil {
						return err
					}
					var index v1.Index
					if err := json.Unmarshal(data, &index); err != nil {
						return err
					}
					if len(index.Manifests) != wantMembers || !reflect.DeepEqual(index.Manifests[0], originalIndex.Manifests[0]) {
						return fmt.Errorf("native artifact descriptor changed: %s", data)
					}
					return nil
				}
				control, err := runtime.LookupManifestList("localhost/artifact-control:latest")
				if err != nil {
					return err
				}
				ref, err := imagestorage.Transport.NewStoreReference(store, nil, amd.ImageID)
				if err != nil {
					return err
				}
				if _, err := control.Add(ctx, transports.ImageName(ref), nil); err != nil {
					return err
				}
				if err := verifyExport(control.ID(), 2); err != nil {
					return fmt.Errorf("public native Add control: %w", err)
				}
				for i, selection := range []oci.StoredSelection{amd, arm} {
					platform, err := selection.Platform()
					if err != nil {
						return err
					}
					_, _, _, id, err := appendManifest(ctx, "artifact-append", "oci", options, map[string]oci.StoredSelection{platform.OS + "/" + platform.Architecture: selection})
					if err != nil {
						return err
					}
					if err := verifyExport(id, i+2); err != nil {
						return err
					}
					_, historical, _, err := oci.StoredImageSelections(ctx, store, originalRoot.Digest.String())
					if err != nil {
						return err
					}
					if !bytes.Equal(historical, original) {
						t.Fatal("append changed historical artifact root")
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
