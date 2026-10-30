package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/localstore"
	"github.com/strio-dev/coopr/internal/oci"
	"go.podman.io/common/libimage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestLoadImagesRetainsImplicitPlatformMembers(t *testing.T) {
	if testing.Short() {
		t.Skip("requires live rootless native storage")
	}
	native := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	foreign := v1.Platform{OS: "linux", Architecture: "arm64"}
	if native.Architecture == foreign.Architecture {
		foreign.Architecture = "amd64"
	}
	for _, format := range []string{"oci", "oci-archive"} {
		for _, test := range []struct {
			name      string
			platforms []v1.Platform
			explicit  []bool
		}{
			{"native", []v1.Platform{native}, []bool{false}},
			{"foreign", []v1.Platform{foreign}, []bool{false}},
			{"mixed", []v1.Platform{foreign, native}, []bool{true, false}},
		} {
			t.Run(format+"/"+test.name, func(t *testing.T) {
				ctx := context.Background()
				layout := t.TempDir()
				content, err := orasoci.NewWithContext(ctx, layout)
				if err != nil {
					t.Fatal(err)
				}
				index := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex}
				for i, platform := range test.platforms {
					configData, err := json.Marshal(v1.Image{Platform: platform, Config: v1.ImageConfig{Cmd: []string{"/bin/true"}}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{}}})
					if err != nil {
						t.Fatal(err)
					}
					config := descriptor(v1.MediaTypeImageConfig, configData)
					manifestData, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{}})
					if err != nil {
						t.Fatal(err)
					}
					child := descriptor(v1.MediaTypeImageManifest, manifestData)
					for _, blob := range []struct {
						desc v1.Descriptor
						data []byte
					}{{config, configData}, {child, manifestData}} {
						if err := content.Push(ctx, blob.desc, bytes.NewReader(blob.data)); err != nil {
							t.Fatal(err)
						}
					}
					if test.explicit[i] {
						child.Platform = &platform
					}
					index.Manifests = append(index.Manifests, child)
				}
				data, err := json.Marshal(index)
				if err != nil {
					t.Fatal(err)
				}
				root := descriptor(v1.MediaTypeImageIndex, data)
				if err := content.Push(ctx, root, bytes.NewReader(data)); err != nil {
					t.Fatal(err)
				}
				const tag = "localhost/implicit-platform:latest"
				tagged := root
				tagged.Annotations = map[string]string{v1.AnnotationRefName: tag}
				top, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []v1.Descriptor{tagged}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(layout, "index.json"), top, 0600); err != nil {
					t.Fatal(err)
				}
				path := layout
				if format == "oci-archive" {
					path = filepath.Join(t.TempDir(), "image.tar")
					if err := localstore.WriteArchive(ctx, content, tagged, path); err != nil {
						t.Fatal(err)
					}
				}
				dir := t.TempDir()
				store := StoreOptions{RunRoot: filepath.Join(dir, "run"), GraphRoot: filepath.Join(dir, "graph"), GraphDriverName: "vfs"}
				names, err := LoadImages(ctx, store, path, oci.Options{SignaturePolicyPath: writeComponentTestPolicy(t, t.TempDir())}, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				if len(names) != 1 || format == "oci-archive" && names[0] != tag {
					t.Fatalf("loaded names = %q", names)
				}
				loadedName := names[0]
				if format == "oci-archive" && len(test.platforms) == 1 {
					baseline := cacheTestStore(t.TempDir())
					platform := test.platforms[0]
					err := WithStore(baseline, func(backend storage.Store) error {
						native, err := libimage.RuntimeFromStore(backend, &libimage.RuntimeOptions{SystemContext: &types.SystemContext{OSChoice: platform.OS, ArchitectureChoice: platform.Architecture}})
						if err != nil {
							return err
						}
						loaded, err := native.Load(ctx, path, &libimage.LoadOptions{CopyOptions: libimage.CopyOptions{SignaturePolicyPath: writeComponentTestPolicy(t, t.TempDir())}})
						if err != nil {
							return err
						}
						image, _, err := native.LookupImage(loaded[0], nil)
						if err != nil {
							return err
						}
						inspected, err := image.Inspect(ctx, nil)
						if err != nil {
							return err
						}
						if inspected.Os != platform.OS || inspected.Architecture != platform.Architecture {
							t.Errorf("native load inspected %s/%s, want %s/%s", inspected.Os, inspected.Architecture, platform.OS, platform.Architecture)
						}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				err = WithStore(store, func(backend storage.Store) error {
					actual, encoded, indexed, err := oci.StoredImageIndex(ctx, backend, loadedName)
					if err != nil {
						return err
					}
					if !indexed || actual.Digest != root.Digest || !bytes.Equal(encoded, data) {
						t.Errorf("index changed: %s, want %s", actual.Digest, root.Digest)
					}
					for i, platform := range test.platforms {
						selected, err := oci.ResolveStoredImage(ctx, backend, loadedName, platform)
						if err != nil {
							return err
						}
						if selected.Selected.Digest != index.Manifests[i].Digest {
							t.Errorf("selected %s, want %s", selected.Selected.Digest, index.Manifests[i].Digest)
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
}

func TestLoadImagesRejectsArtifactOnlyIndexWithoutReplacingTag(t *testing.T) {
	testLoadImagesRejectsInvalidIndexWithoutReplacingTag(t, false)
}

func TestLoadImagesRejectsCorruptConfigWithoutReplacingTag(t *testing.T) {
	testLoadImagesRejectsInvalidIndexWithoutReplacingTag(t, true)
}

func testLoadImagesRejectsInvalidIndexWithoutReplacingTag(t *testing.T, corruptConfig bool) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires live native storage")
	}
	ctx := context.Background()
	const tag = "localhost/artifact-boundary:latest"
	rootDir := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(rootDir, "run"), GraphRoot: filepath.Join(rootDir, "graph"), GraphDriverName: "vfs"}
	policy := writeComponentTestPolicy(t, t.TempDir())
	var originalRoot digest.Digest
	for _, invalid := range []bool{false, true} {
		layout := t.TempDir()
		content, err := orasoci.NewWithContext(ctx, layout)
		if err != nil {
			t.Fatal(err)
		}
		configData, err := json.Marshal(v1.Image{Platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, Config: v1.ImageConfig{Env: []string{"INTEGRITY=good"}}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{}}})
		if err != nil {
			t.Fatal(err)
		}
		config := descriptor(v1.MediaTypeImageConfig, configData)
		encoded := v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{}}
		if invalid && !corruptConfig {
			encoded.ArtifactType = "application/vnd.coopr.test-artifact"
		}
		data, err := json.Marshal(encoded)
		if err != nil {
			t.Fatal(err)
		}
		child := descriptor(v1.MediaTypeImageManifest, data)
		for _, blob := range []struct {
			desc v1.Descriptor
			data []byte
		}{{config, configData}, {child, data}} {
			if err := content.Push(ctx, blob.desc, bytes.NewReader(blob.data)); err != nil {
				t.Fatal(err)
			}
		}
		data, err = json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{child}})
		if err != nil {
			t.Fatal(err)
		}
		root := descriptor(v1.MediaTypeImageIndex, data)
		if err := content.Push(ctx, root, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		root.Annotations = map[string]string{v1.AnnotationRefName: tag}
		top, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []v1.Descriptor{root}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(layout, "index.json"), top, 0600); err != nil {
			t.Fatal(err)
		}
		archivePath := filepath.Join(t.TempDir(), "image.tar")
		if err := localstore.WriteArchive(ctx, content, root, archivePath); err != nil {
			t.Fatal(err)
		}
		loadPath := archivePath
		if invalid && corruptConfig {
			// Preserve the claimed digest and size while replacing the blob with
			// different valid JSON, so platform discovery must verify integrity.
			corrupt := bytes.Replace(configData, []byte("INTEGRITY=good"), []byte("INTEGRITY=evil"), 1)
			if bytes.Equal(corrupt, configData) {
				t.Fatal("configuration corruption did not change bytes")
			}
			if err := os.WriteFile(filepath.Join(layout, "blobs", config.Digest.Algorithm().String(), config.Digest.Encoded()), corrupt, 0600); err != nil {
				t.Fatal(err)
			}
			loadPath = layout
		}
		_, err = LoadImages(ctx, store, loadPath, oci.Options{SignaturePolicyPath: policy}, io.Discard)
		if invalid {
			if err == nil {
				t.Fatal("invalid image accepted as runnable index")
			}
			if corruptConfig && (!strings.HasPrefix(err.Error(), "read archived platform ") || !strings.Contains(strings.ToLower(err.Error()), "digest")) {
				t.Fatalf("expected config integrity failure before native load, got %v", err)
			}
			if err := WithStore(store, func(backend storage.Store) error {
				selected, err := oci.ResolveStoredImage(ctx, backend, tag, v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
				if err != nil {
					return err
				}
				if selected.Root.Digest != originalRoot {
					t.Errorf("failed load replaced original tag: %s, want %s", selected.Root.Digest, originalRoot)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		} else if err != nil {
			t.Fatal(err)
		} else {
			originalRoot = root.Digest
		}
	}
}
