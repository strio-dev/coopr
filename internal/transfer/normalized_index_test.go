package transfer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"coopr/internal/buildah"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/signature/sigstore"
	nativeimage "go.podman.io/image/v5/storage"
	"go.podman.io/storage"
	"net/http"
	"net/http/httptest"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRewrittenImageIndexPreservesMetadataAndDuplicateMembers(t *testing.T) {
	original := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","artifactType":"application/example","annotations":{"index":"retained"},"subject":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":10},"example":{"preserved":true},"manifests":[]}`)
	root := oci.Descriptor(v1.MediaTypeImageIndex, original)
	root.Annotations = map[string]string{"root": "retained"}
	root.ArtifactType = "application/example-root"
	root.Data = original
	platform := &v1.Platform{OS: "linux", Architecture: "amd64", OSVersion: "version", OSFeatures: []string{"feature"}}
	normalized := oci.Descriptor(v1.MediaTypeImageManifest, []byte("same emitted manifest"))
	first, second := normalized, normalized
	first.Platform, second.Platform = platform, platform
	first.Annotations, second.Annotations = map[string]string{"instance": "gzip"}, map[string]string{"instance": "zstd"}
	changed, data, err := rewrittenImageIndex(root, original, []v1.Descriptor{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Digest == root.Digest || changed.Digest != digest.FromBytes(data) || changed.Size != int64(len(data)) || !reflect.DeepEqual(changed.Annotations, root.Annotations) || changed.ArtifactType != root.ArtifactType || !bytes.Equal(changed.Data, data) {
		t.Fatalf("invalid actual root: %+v", changed)
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 2 || !reflect.DeepEqual(index.Manifests, []v1.Descriptor{first, second}) || index.Annotations["index"] != "retained" || index.Subject == nil || index.ArtifactType != "application/example" {
		t.Fatalf("lost index metadata/instances: %+v", index)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fields["example"], []byte(`{"preserved":true}`)) {
		t.Fatalf("discarded root extension: %s", data)
	}
}

func TestStoredSelectionRequiresResidentManifestNotOrigin(t *testing.T) {
	origin := oci.Descriptor(v1.MediaTypeImageManifest, []byte("origin"))
	emitted := oci.Descriptor(v1.MediaTypeImageManifest, []byte("emitted"))
	selection := oci.StoredSelection{Manifest: emitted, SourceManifest: &origin}
	if storedSelectionHasManifest(selection, origin) || !storedSelectionHasManifest(selection, emitted) {
		t.Fatal("origin must not count as a resident runnable manifest")
	}
	mismatched := emitted
	mismatched.Size++
	if storedSelectionHasManifest(selection, mismatched) {
		t.Fatal("accepted mismatched resident descriptor")
	}
}

func TestNativeNormalizedIndexCopyRetainsDuplicateMembersAndActualRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("native image export uses supervised workers")
	}
	ctx := context.Background()
	store := nativeTestStore(t.TempDir())
	sourcePath := filepath.Join(t.TempDir(), "source")
	source, err := orasoci.NewWithContext(ctx, sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	push := func(media string, data []byte) v1.Descriptor {
		desc := oci.Descriptor(media, data)
		if err := source.Push(ctx, desc, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		return desc
	}
	var rawLayer bytes.Buffer
	tarWriter := tar.NewWriter(&rawLayer)
	payload := []byte("normalized image layer")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "payload", Mode: 0644, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	diffID := digest.FromBytes(rawLayer.Bytes())
	configData, err := json.Marshal(v1.Image{Platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{diffID}}, History: []v1.History{{CreatedBy: "fixture"}}})
	if err != nil {
		t.Fatal(err)
	}
	config := push(v1.MediaTypeImageConfig, configData)
	members := []v1.Descriptor{}
	for _, name := range []string{"first", "second", "third"} {
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		writer.Comment = name
		if _, err := writer.Write(rawLayer.Bytes()); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		layer := push(v1.MediaTypeImageLayerGzip, compressed.Bytes())
		raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{layer}})
		if err != nil {
			t.Fatal(err)
		}
		child := push(v1.MediaTypeImageManifest, raw)
		child.Platform = &v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
		child.Annotations = map[string]string{"instance": name}
		members = append(members, child)
	}
	indexData, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: members, Annotations: map[string]string{"index": "retained"}, Subject: &config})
	if err != nil {
		t.Fatal(err)
	}
	original := push(v1.MediaTypeImageIndex, indexData)
	original.Annotations = map[string]string{v1.AnnotationRefName: "localhost/normalized:latest"}
	archive := filepath.Join(t.TempDir(), "input.tar")
	if err := localstore.WriteArchive(ctx, source, original, archive); err != nil {
		t.Fatal(err)
	}
	if _, err := buildah.LoadImages(ctx, store, archive, oci.Options{}, nil); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "normalized.tar")
	if _, err := Copy(ctx, oci.Image, "normalized", Destination{Transport: "oci-archive", Name: output}, Options{BuildStore: store, ArchiveReference: "localhost/normalized:latest"}); err != nil {
		t.Fatal(err)
	}
	saved, err := orasoci.NewFromTar(ctx, output)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := saved.Resolve(ctx, "localhost/normalized:latest")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := content.FetchAll(ctx, saved, actual)
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 3 || index.Manifests[0].Annotations["instance"] != "first" || index.Manifests[1].Annotations["instance"] != "second" || index.Manifests[2].Annotations["instance"] != "third" || index.Annotations["index"] != "retained" || !reflect.DeepEqual(index.Subject, &config) {
		t.Fatalf("lost normalized index metadata/members: %+v", index)
	}
	if actual.Digest == original.Digest {
		t.Fatal("fixture did not require native normalization")
	}
	if index.Manifests[0].Digest != index.Manifests[1].Digest || index.Manifests[0].Digest != index.Manifests[2].Digest {
		t.Fatal("fixture must normalize three descriptors to one manifest identity")
	}
	singleOutput := filepath.Join(t.TempDir(), "single.tar")
	if _, err := Copy(ctx, oci.Image, members[0].Digest.String(), Destination{Transport: "oci-archive", Name: singleOutput}, Options{BuildStore: store}); err != nil {
		t.Fatal(err)
	}
	if selectedCopyArchiveRoot(t, singleOutput).Digest != index.Manifests[0].Digest {
		t.Fatal("single export did not propagate actual descriptor")
	}

	for _, child := range index.Manifests {
		raw, err := content.FetchAll(ctx, saved, child)
		if err != nil {
			t.Fatal(err)
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.Config.Digest != config.Digest || len(manifest.Layers) != 1 {
			t.Fatalf("changed config/layer chain: %+v", manifest)
		}
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	repository := strings.TrimPrefix(server.URL, "http://") + "/normalized:latest"
	published, err := Copy(ctx, oci.Image, "normalized", Destination{Transport: "registry", Name: repository}, Options{BuildStore: store, TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}

	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	remoteData, _, err := resolver.RemoteImageManifest(ctx, repository)
	if err != nil || !strings.HasSuffix(published, "@"+digest.FromBytes(remoteData).String()) {
		t.Fatalf("registry content differs from reported actual root: %v", err)
	}

	var remoteIndex v1.Index
	if err := json.Unmarshal(remoteData, &remoteIndex); err != nil {
		t.Fatal(err)
	}
	if len(remoteIndex.Manifests) != 3 || remoteIndex.Manifests[0].Annotations["instance"] != "first" || remoteIndex.Manifests[1].Annotations["instance"] != "second" || remoteIndex.Manifests[2].Annotations["instance"] != "third" || !reflect.DeepEqual(remoteIndex.Subject, &config) {
		t.Fatalf("lost native published metadata: %+v", remoteIndex)
	}

	storageOnlyPolicy := `{"default":[{"type":"reject"}],"transports":{"containers-storage":{"": [{"type":"insecureAcceptAnything"}]},"dir":{"": [{"type":"reject"}]},"oci":{"": [{"type":"reject"}]}}}`
	storageOnlyPolicyPath := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(storageOnlyPolicyPath, []byte(storageOnlyPolicy), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, stored, err := storedSelections(ctx, Options{BuildStore: store}, "normalized")
	if err != nil {
		t.Fatal(err)
	}
	originalScopes := map[string][]map[string]string{}
	if err := buildah.WithStore(store, func(backend storage.Store) error {
		for _, selected := range stored {
			ref, err := nativeimage.Transport.NewStoreReference(backend, nil, selected.ImageID)
			if err != nil {
				return err
			}
			originalScopes[ref.PolicyConfigurationIdentity()] = []map[string]string{{"type": "insecureAcceptAnything"}}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	originalPolicy, err := json.Marshal(map[string]any{
		"default":    []map[string]string{{"type": "reject"}},
		"transports": map[string]any{"containers-storage": originalScopes},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name           string
		policy         string
		allowed        bool
		externalOCI    bool
		singleRegistry bool
	}{
		{
			name:    "allow_exact_original_storage_identity",
			policy:  string(originalPolicy),
			allowed: true,
		},
		{
			name:           "allow_exact_original_storage_identity_single_registry",
			policy:         string(originalPolicy),
			allowed:        true,
			singleRegistry: true,
		},
		{
			name:    "allow_original_storage_reject_staging",
			policy:  storageOnlyPolicy,
			allowed: true,
		},
		{
			name:        "reject_external_oci_source",
			policy:      storageOnlyPolicy,
			externalOCI: true,
		},
		{
			name:   "reject_original_storage_allow_staging",
			policy: `{"default":[{"type":"insecureAcceptAnything"}],"transports":{"containers-storage":{"": [{"type":"reject"}]}}}`,
		},
		{
			name:   "reject_all_sources",
			policy: `{"default":[{"type":"reject"}]}`,
		},
		{
			name:   "malformed_policy",
			policy: `{"default":`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			policyPath := filepath.Join(t.TempDir(), "policy.json")
			if err := os.WriteFile(policyPath, []byte(test.policy), 0600); err != nil {
				t.Fatal(err)
			}
			opts := Options{
				BuildStore: store, TLSVerify: new(false), SignaturePolicyPath: policyPath,
			}
			for _, transport := range []string{"oci-dir", "oci-archive"} {
				for _, input := range []struct {
					name string
					root v1.Descriptor
				}{
					{name: "index", root: original},
					{name: "image", root: members[0]},
				} {
					t.Run(transport+"/"+input.name, func(t *testing.T) {
						path := filepath.Join(t.TempDir(), "output")
						destination := Destination{Transport: transport, Name: path}
						var err error
						if test.externalOCI {
							_, err = CopyRoot(ctx, oci.Image, sourcePath, input.root, destination, opts)
						} else {
							selector := "normalized"
							if input.name == "image" {
								selector = input.root.Digest.String()
							}
							_, err = Copy(ctx, oci.Image, selector, destination, opts)
						}
						if !test.allowed {
							if err == nil {
								t.Fatal("source with rejected or invalid policy was exported")
							}
							if !test.externalOCI {
								if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
									t.Fatalf("rejected source created destination: %v (copy: %v)", statErr, err)
								}
							}
							return
						}
						if err != nil {
							t.Fatalf("policy permitting original storage rejected archive staging: %v", err)
						}
						var reader content.ReadOnlyStorage
						var root v1.Descriptor
						if transport == "oci-dir" {
							root, err = oci.LayoutRoot(path)
							if err == nil {
								reader, err = orasoci.NewWithContext(ctx, path)
							}
						} else {
							root = selectedCopyArchiveRoot(t, path)
							reader, err = orasoci.NewFromTar(ctx, path)
						}
						if err != nil {
							t.Fatal(err)
						}
						raw, err := content.FetchAll(ctx, reader, root)
						if err != nil || root.Digest != digest.FromBytes(raw) {
							t.Fatalf("export root differs from actual content: %v", err)
						}
						children := []v1.Descriptor{root}
						if input.name == "index" {
							var exported v1.Index
							if err := json.Unmarshal(raw, &exported); err != nil {
								t.Fatal(err)
							}
							if !reflect.DeepEqual(exported, index) {
								t.Fatalf("policy changed exported index: got %+v, want %+v", exported, index)
							}
							children = exported.Manifests
						}
						for _, child := range children {
							raw, err := content.FetchAll(ctx, reader, child)
							if err != nil {
								t.Fatal(err)
							}
							var manifest v1.Manifest
							if err := json.Unmarshal(raw, &manifest); err != nil {
								t.Fatal(err)
							}
							if manifest.Config.Digest != config.Digest || len(manifest.Layers) != 1 {
								t.Fatalf("policy changed exported config or layers: %+v", manifest)
							}
						}
					})
				}
			}
			var writes atomic.Int64
			handler := registry.New()
			policyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet && r.Method != http.MethodHead {
					writes.Add(1)
				}
				handler.ServeHTTP(w, r)
			}))
			defer policyServer.Close()
			name := strings.TrimPrefix(policyServer.URL, "http://") + "/policy:latest"
			destination := Destination{Transport: "registry", Name: name}
			var result string
			var err error
			if test.externalOCI {
				result, err = CopyRoot(ctx, oci.Image, sourcePath, original, destination, opts)
			} else {
				selector := "normalized"
				if test.singleRegistry {
					selector = members[0].Digest.String()
				}
				result, err = Copy(ctx, oci.Image, selector, destination, opts)
			}
			if !test.allowed {
				if err == nil {
					t.Fatal("source with rejected or invalid policy was published")
				}
				if writes.Load() != 0 {
					t.Fatalf("rejected source caused %d registry writes: %v", writes.Load(), err)
				}
				return
			}
			if err != nil {
				t.Fatalf("policy permitting the original storage source rejected private staging: %v", err)
			}
			resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false)})
			if err != nil {
				t.Fatal(err)
			}
			remote, _, err := resolver.RemoteImageManifest(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(result, "@"+digest.FromBytes(remote).String()) {
				t.Fatalf("published identity does not describe actual registry content: %s", result)
			}
			if test.singleRegistry {
				var manifest v1.Manifest
				if err := json.Unmarshal(remote, &manifest); err != nil {
					t.Fatal(err)
				}
				if manifest.Config.Digest != config.Digest || len(manifest.Layers) != 1 {
					t.Fatalf("policy changed published image config or layers: %+v", manifest)
				}
				return
			}
			var published v1.Index
			if err := json.Unmarshal(remote, &published); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(published, remoteIndex) {
				t.Fatalf("source policy changed normalized members or metadata: got %+v, want %+v", published, remoteIndex)
			}
		})
	}

	synthetic := []byte("\x00sigstore-json\n{\"mimeType\":\"application/vnd.dev.cosign.simplesigning.v1+json\",\"payload\":\"cGF5bG9hZA==\",\"annotations\":{\"coopr.test\":\"preserve\"}}")
	for _, selected := range stored {
		if selected.Manifest.Digest == members[0].Digest {
			injectStoredSignatureFixture(t, store, selected.ImageID, selected.Manifest.Digest, synthetic, json.RawMessage(`{"preserve":true}`))
		}
	}
	signedDestination := Destination{Transport: "registry", Name: strings.TrimPrefix(server.URL, "http://") + "/signed-normalized:latest"}
	signedOptions := Options{BuildStore: store, TLSVerify: new(false), Push: PushOptions{CompressionFormat: "zstd", ForceCompression: true}}
	if _, err := Copy(ctx, oci.Image, "normalized", signedDestination, signedOptions); err == nil {
		t.Fatal("signed source was rewritten without removing signatures")
	}
	signedOptions.Push.RemoveSignatures = true
	removed, err := Copy(ctx, oci.Image, "normalized", signedDestination, signedOptions)
	if err != nil {
		t.Fatalf("explicit signature removal did not permit native rewrite: %v", err)
	}
	removedData, _, err := resolver.RemoteImageManifest(ctx, signedDestination.Name)
	if err != nil || !strings.HasSuffix(removed, "@"+digest.FromBytes(removedData).String()) {
		t.Fatalf("removed-signature publication identity differs: %s %v", removed, err)
	}

	recipient, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	recipientData, err := x509.MarshalPKIXPublicKey(&recipient.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	passphrase := []byte("index signing passphrase")
	keys, err := sigstore.GenerateKeyPair(passphrase)
	if err != nil {
		t.Fatal(err)
	}
	cryptoDir := t.TempDir()
	publicRecipient := filepath.Join(cryptoDir, "recipient.pem")
	privateSigning, publicSigning, password := filepath.Join(cryptoDir, "cosign.key"), filepath.Join(cryptoDir, "cosign.pub"), filepath.Join(cryptoDir, "password")
	for path, data := range map[string][]byte{publicRecipient: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: recipientData}), privateSigning: keys.PrivateKey, publicSigning: keys.PublicKey, password: passphrase} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	encryptedName := strings.TrimPrefix(server.URL, "http://") + "/encrypted-signed:latest"
	encryptedResult, err := Copy(ctx, oci.Image, "normalized", Destination{Transport: "registry", Name: encryptedName}, Options{
		BuildStore: store, TLSVerify: new(false), SignaturePolicyPath: storageOnlyPolicyPath,
		Push:    PushOptions{RemoveSignatures: true, EncryptionKeys: []string{"jwe:" + publicRecipient}},
		Signing: SigningOptions{SigstorePrivateKeyFile: privateSigning, PassphraseFile: password},
	})
	if err != nil {
		t.Fatalf("encrypt and sign native index: %v", err)
	}
	encryptedData, _, err := resolver.RemoteImageManifest(ctx, encryptedName)
	if err != nil || !strings.HasSuffix(encryptedResult, "@"+digest.FromBytes(encryptedData).String()) {
		t.Fatalf("encrypted publication identity differs: %s %v", encryptedResult, err)
	}
	var encryptedIndex v1.Index
	if err := json.Unmarshal(encryptedData, &encryptedIndex); err != nil {
		t.Fatal(err)
	}
	if len(encryptedIndex.Manifests) != 3 {
		t.Fatalf("encrypted index lost members: %+v", encryptedIndex)
	}
	verifySignedReference(t, ctx, encryptedName, publicSigning, imagecopy.CopyAllImages, true)
	for _, child := range encryptedIndex.Manifests {
		name := strings.TrimSuffix(encryptedName, ":latest") + "@" + child.Digest.String()
		raw, _, err := resolver.RemoteImageManifest(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatal(err)
		}
		if len(manifest.Layers) != 1 || !strings.HasSuffix(manifest.Layers[0].MediaType, "+encrypted") || manifest.Config.Digest != config.Digest {
			t.Fatalf("encrypted child changed config or has invalid layer: %+v", manifest)
		}
		verifySignedReference(t, ctx, name, publicSigning, imagecopy.CopySystemImage, true)
	}

	reloaded := nativeTestStore(t.TempDir())
	if _, err := buildah.LoadImages(ctx, reloaded, output, oci.Options{}, nil); err != nil {
		t.Fatal(err)
	}
	again := filepath.Join(t.TempDir(), "again.tar")
	if _, err := Copy(ctx, oci.Image, "normalized", Destination{Transport: "oci-archive", Name: again}, Options{BuildStore: reloaded, ArchiveReference: "localhost/normalized:latest"}); err != nil {
		t.Fatal(err)
	}
	second, err := orasoci.NewFromTar(ctx, again)
	if err != nil {
		t.Fatal(err)
	}
	final, err := second.Resolve(ctx, "localhost/normalized:latest")
	if err != nil || final.Digest != actual.Digest {
		t.Fatalf("save/load/save lost duplicate members or root: %+v %v", final, err)
	}
}
