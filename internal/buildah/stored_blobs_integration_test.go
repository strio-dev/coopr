package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"coopr/internal/imagestore"
	"coopr/internal/oci"
	registryserver "github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah/pkg/cli"
	"go.podman.io/common/libimage"
	"go.podman.io/image/v5/pkg/compression"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
)

func TestPulledImageCompressionVariantsExportNativeOffline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping native compressed image imports in short mode")
	}
	ctx := context.Background()
	fixture := newRawDockerFixture(t)
	var original v1.Manifest
	if err := json.Unmarshal(fixture.manifestData, &original); err != nil {
		t.Fatal(err)
	}
	rawLayer := fixture.blobs[original.Layers[0].Digest]
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	layout := filepath.Join(t.TempDir(), "source")
	source, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	config := fixture.config
	config.MediaType = v1.MediaTypeImageConfig
	if err := source.Push(ctx, config, bytes.NewReader(fixture.configData)); err != nil {
		t.Fatal(err)
	}
	var members []v1.Descriptor
	for _, algorithm := range []compression.Algorithm{compression.Gzip, compression.Zstd} {
		var compressed bytes.Buffer
		writer, err := compression.CompressStream(&compressed, algorithm, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(rawLayer); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		layerType := v1.MediaTypeImageLayerGzip
		if algorithm.Name() == "zstd" {
			layerType = v1.MediaTypeImageLayerZstd
		}
		layer := oci.Descriptor(layerType, compressed.Bytes())
		if err := source.Push(ctx, layer, bytes.NewReader(compressed.Bytes())); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{layer}})
		if err != nil {
			t.Fatal(err)
		}
		member := oci.Descriptor(v1.MediaTypeImageManifest, data)
		member.Platform = &platform
		if algorithm.Name() == "zstd" {
			member.Annotations = map[string]string{"io.github.containers.compression.zstd": "true"}
		}
		if err := source.Push(ctx, member, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		members = append(members, member)
	}
	index := sourceTestIndex(t, ctx, source, members...)
	if err := source.Tag(ctx, index, "variants"); err != nil {
		t.Fatal(err)
	}
	registry := registryserver.New()
	var offline atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			t.Error("export accessed registry")
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		registry.ServeHTTP(w, r)
	}))
	defer server.Close()
	name := strings.TrimPrefix(server.URL, "http://") + "/coopr/compressed"
	repo, err := remote.NewRepository(name)
	if err != nil {
		t.Fatal(err)
	}
	repo.PlainHTTP = true
	if err := oras.CopyGraph(ctx, source, repo, index, oras.DefaultCopyGraphOptions); err != nil {
		t.Fatal(err)
	}
	if err := repo.Tag(ctx, index, "variants"); err != nil {
		t.Fatal(err)
	}
	encryptedRegistry := registryserver.New()
	encryptedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			t.Error("export accessed encrypted registry")
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		encryptedRegistry.ServeHTTP(w, r)
	}))
	defer encryptedServer.Close()
	encryptedName := strings.TrimPrefix(encryptedServer.URL, "http://") + "/coopr/encrypted"
	encryptedRepo, err := remote.NewRepository(encryptedName)
	if err != nil {
		t.Fatal(err)
	}
	encryptedRepo.PlainHTTP = true
	cryptoRoot := t.TempDir()
	privateKey, publicKey := testEncryptionKeys(t, cryptoRoot)
	encrypt, encryptLayers, err := cli.EncryptConfig([]string{"jwe:" + publicKey}, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	input, err := imagestore.LayoutReference(layout, members[0])
	if err != nil {
		t.Fatal(err)
	}
	destination, err := alltransports.ParseImageName("docker://" + encryptedName + ":encrypted-gzip")
	if err != nil {
		t.Fatal(err)
	}
	policy := writeComponentTestPolicy(t, cryptoRoot)
	copier, err := libimage.NewCopier(&libimage.CopyOptions{OciEncryptConfig: encrypt, OciEncryptLayers: encryptLayers, InsecureSkipTLSVerify: types.OptionalBoolTrue, SignaturePolicyPath: policy}, &types.SystemContext{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := copier.Copy(ctx, input, destination); err != nil {
		t.Fatal(err)
	}
	if err := copier.Close(); err != nil {
		t.Fatal(err)
	}
	encryptedRoot, err := encryptedRepo.Resolve(ctx, "encrypted-gzip")
	if err != nil {
		t.Fatal(err)
	}
	encryptedData, err := content.FetchAll(ctx, encryptedRepo, encryptedRoot)
	if err != nil {
		t.Fatal(err)
	}
	var encryptedManifest v1.Manifest
	if err := json.Unmarshal(encryptedData, &encryptedManifest); err != nil {
		t.Fatal(err)
	}
	if encryptedManifest.Layers[0].MediaType != v1.MediaTypeImageLayerGzip+"+encrypted" {
		t.Fatalf("fixture is not encrypted gzip: %+v", encryptedManifest.Layers)
	}

	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "existing-config"}[warm], func(t *testing.T) {
			root := t.TempDir()
			options := StoreOptions{GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"), GraphDriverName: "vfs"}
			lease, err := acquireStore(options)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := lease.Close(); err != nil {
					t.Error(err)
				}
			}()
			system := &types.SystemContext{SignaturePolicyPath: writeComponentTestPolicy(t, root), BigFilesTemporaryDir: root}
			if warm {
				warmLayout := filepath.Join(root, "warm")
				if _, err := exportRawDockerSource(ctx, &rawDockerSource{manifest: fixture.manifestData, blobs: fixture.blobs}, "warm", Output{Path: warmLayout}); err != nil {
					t.Fatal(err)
				}
				if _, err := ImportSelectedImage(ctx, lease.store, system, warmLayout, fixture.manifest); err != nil {
					t.Fatal(err)
				}
			}
			resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false), PullPolicy: "always", SignaturePolicyPath: system.SignaturePolicyPath})
			if err != nil {
				t.Fatal(err)
			}
			decrypting, err := oci.NewResolver(oci.Options{TLSVerify: new(false), PullPolicy: "always", SignaturePolicyPath: system.SignaturePolicyPath, DecryptionKeys: []string{privateKey}})
			if err != nil {
				t.Fatal(err)
			}
			decrypted, err := ResolveImageSource(ctx, decrypting, encryptedName+":encrypted-gzip", platform, lease.store, system)
			if err != nil {
				t.Fatal(err)
			}
			if decrypted.SourceManifest == nil {
				t.Fatal("lost ciphertext provenance")
			}
			decryptedData, err := lease.store.ImageBigData(decrypted.ImageID, "manifest-"+decrypted.Selected.Digest.String())
			if err != nil {
				t.Fatal(err)
			}
			var decryptedManifest v1.Manifest
			if err := json.Unmarshal(decryptedData, &decryptedManifest); err != nil {
				t.Fatal(err)
			}
			if decryptedManifest.Layers[0].MediaType != v1.MediaTypeImageLayerGzip {
				t.Fatalf("decrypted gzip unexpectedly changed compression: %+v", decryptedManifest.Layers)
			}

			offline.Store(true)
			if _, err := exportStoredImageVariantRaw(ctx, lease.store, decrypted.ImageID, Output{Path: filepath.Join(root, "decrypted-gzip")}, system, &decrypted.Selected.Digest); err != nil {
				t.Fatal(err)
			}
			decryptedRoot, err := oci.LayoutRoot(filepath.Join(root, "decrypted-gzip"))
			if err != nil {
				t.Fatal(err)
			}
			var decryptedOutput v1.Manifest
			if err := json.Unmarshal(readRawDockerBlob(t, filepath.Join(root, "decrypted-gzip"), decryptedRoot), &decryptedOutput); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(readRawDockerBlob(t, filepath.Join(root, "decrypted-gzip"), decryptedOutput.Config), fixture.configData) {
				t.Fatal("decryption export changed raw configuration")
			}
			offline.Store(false)
			selected, err := ResolveImageSource(ctx, resolver, name+":variants", platform, lease.store, system)
			if err != nil {
				t.Fatal(err)
			}
			if selected.Selected.Digest != members[1].Digest {
				t.Fatalf("selected %s, want zstd %s", selected.Selected.Digest, members[1].Digest)
			}
			reloaded := StoreOptions{GraphRoot: filepath.Join(root, "reload-graph"), RunRoot: filepath.Join(root, "reload-run"), GraphDriverName: "vfs"}
			for _, member := range []v1.Descriptor{members[1], members[0]} {
				pulled, err := ResolveImageSource(ctx, resolver, name+"@"+member.Digest.String(), platform, lease.store, system)
				if err != nil {
					t.Fatal(err)
				}
				destination := filepath.Join(root, member.Digest.Encoded())
				offline.Store(true)
				rejectPath := filepath.Join(t.TempDir(), "reject.json")
				if err := os.WriteFile(rejectPath, []byte(`{"default":[{"type":"reject"}]}`), 0o600); err != nil {
					t.Fatal(err)
				}
				rejected := *system
				rejected.SignaturePolicyPath = rejectPath
				if _, err := exportStoredImageVariantRaw(ctx, lease.store, pulled.ImageID, Output{Path: destination + "-rejected"}, &rejected, &member.Digest); err == nil {
					t.Fatal("native fallback ignored reject policy")
				}
				result, err := exportStoredImageVariantRaw(ctx, lease.store, pulled.ImageID, Output{Path: destination}, system, &member.Digest)
				if err != nil {
					t.Fatal(err)
				}
				actual, err := oci.LayoutRoot(destination)
				if err != nil {
					t.Fatal(err)
				}
				if actual.Digest.String() != result.ManifestDigest {
					t.Fatal("result does not name emitted manifest")
				}
				if _, err := exportStoredImageVariantRaw(ctx, lease.store, pulled.ImageID, Output{Path: destination + "-again"}, system, &actual.Digest); err != nil {
					t.Fatalf("emitted manifest not usable as next stage: %v", err)
				}
				var encoded v1.Manifest
				if err := json.Unmarshal(readRawDockerBlob(t, destination, actual), &encoded); err != nil {
					t.Fatal(err)
				}
				if encoded.Config.Digest != config.Digest || len(encoded.Layers) != 1 {
					t.Fatal("configuration or layer chain changed")
				}
				if !bytes.Equal(readRawDockerBlob(t, destination, encoded.Config), fixture.configData) {
					t.Fatal("raw configuration/history/DiffIDs changed")
				}
				if retained, err := retainedLayerIDs(lease.store, pulled.ImageID); err != nil || len(retained) != 0 {
					t.Fatalf("pull created compressed retention: %v %v", retained, err)
				}

				if _, err := LoadImages(ctx, reloaded, destination, oci.Options{SignaturePolicyPath: system.SignaturePolicyPath}, nil); err != nil {
					t.Fatal(err)
				}
				if err := WithImageStore(ctx, reloaded, func(store storage.Store) error {
					_, err := exportStoredImageVariantRaw(ctx, store, config.Digest.Encoded(), Output{Path: destination + "-reloaded"}, system, &actual.Digest)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				offline.Store(false)
			}
			transported, err := resolveTransportSource(ctx, resolver, "oci:"+layout+":variants", platform, lease.store, system)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := exportStoredImageVariantRaw(ctx, lease.store, transported.ImageID, Output{Path: filepath.Join(root, "transported")}, system, &transported.Selected.Digest); err != nil {
				t.Fatal(err)
			}

		})
	}
	t.Run("docker-schema2", func(t *testing.T) {
		root := t.TempDir()
		data, err := content.FetchAll(ctx, source, members[0])
		if err != nil {
			t.Fatal(err)
		}
		var dockerManifest v1.Manifest
		if err := json.Unmarshal(data, &dockerManifest); err != nil {
			t.Fatal(err)
		}
		dockerManifest.MediaType = dockerManifestMediaType
		dockerManifest.Config.MediaType = dockerImageConfigMediaType
		dockerManifest.Layers[0].MediaType = dockerLayerMediaType + ".gzip"
		data, err = json.Marshal(dockerManifest)
		if err != nil {
			t.Fatal(err)
		}
		descriptor := oci.Descriptor(dockerManifestMediaType, data)
		if err := source.Push(ctx, descriptor, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		options := StoreOptions{GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"), GraphDriverName: "vfs"}
		if err := WithImageStore(ctx, options, func(store storage.Store) error {
			system := &types.SystemContext{SignaturePolicyPath: writeComponentTestPolicy(t, root)}
			id, err := ImportSelectedImage(ctx, store, system, layout, descriptor)
			if err != nil {
				return err
			}
			result, err := exportStoredImageVariantRaw(ctx, store, id, Output{Path: filepath.Join(root, "export")}, system, &descriptor.Digest)
			if err != nil {
				return err
			}
			actual, err := oci.LayoutRoot(result.Layout)
			if err != nil {
				return err
			}
			if actual.MediaType != dockerManifestMediaType || actual.Digest.String() != result.ManifestDigest {
				t.Fatal("Docker representation/result differs")
			}
			var emitted v1.Manifest
			if err := json.Unmarshal(readRawDockerBlob(t, result.Layout, actual), &emitted); err != nil {
				return err
			}
			if !bytes.Equal(readRawDockerBlob(t, result.Layout, emitted.Config), fixture.configData) {
				t.Fatal("Docker configuration/history/DiffIDs changed")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("all-tags", func(t *testing.T) {
		root := t.TempDir()
		options := StoreOptions{GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"), GraphDriverName: "vfs"}
		policy := writeComponentTestPolicy(t, root)
		for i, tag := range []string{"gzip", "zstd"} {
			if err := repo.Tag(ctx, members[i], tag); err != nil {
				t.Fatal(err)
			}
		}
		if err := WithImageStore(ctx, options, func(store storage.Store) error {
			warmLayout := filepath.Join(root, "warm")
			if _, err := exportRawDockerSource(ctx, &rawDockerSource{manifest: fixture.manifestData, blobs: fixture.blobs}, "warm", Output{Path: warmLayout}); err != nil {
				return err
			}
			_, err := ImportSelectedImage(ctx, store, &types.SystemContext{SignaturePolicyPath: policy}, warmLayout, fixture.manifest)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		ids, err := PullAllTags(ctx, options, oci.Options{TLSVerify: new(false), PullPolicy: "always", SignaturePolicyPath: policy}, name, platform)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 3 {
			t.Fatalf("all-tags returned %v", ids)
		}
		if err := WithImageStore(ctx, options, func(store storage.Store) error {
			for _, member := range members {
				if _, err := exportStoredImageVariantRaw(ctx, store, ids[0], Output{Path: filepath.Join(root, member.Digest.Encoded())}, &types.SystemContext{SignaturePolicyPath: policy}, &member.Digest); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
