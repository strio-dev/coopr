package buildah

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coopr/internal/imagecatalog"
	"coopr/internal/imagestore"
	"coopr/internal/oci"
	"coopr/internal/planner"
	registryserver "github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah/pkg/cli"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/signature"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
)

func TestEncryptedRegistryInputDecryptsIntoCanonicalStore(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for native encrypted inputs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	fixture := liveBusyBoxImage(t, ctx)
	privatePath, publicPath := testEncryptionKeys(t, root)
	encrypt, layers, err := cli.EncryptConfig([]string{"jwe:" + publicPath}, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(registryserver.New())
	defer server.Close()
	authority := strings.TrimPrefix(server.URL, "http://")
	reference := authority + "/coopr/encrypted:latest"
	destination, err := docker.ParseReference("//" + reference)
	if err != nil {
		t.Fatal(err)
	}
	source, err := imagestore.LayoutReference(fixture.layout, fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	policyPath := writeComponentTestPolicy(t, root)
	system := &types.SystemContext{SignaturePolicyPath: policyPath, DockerInsecureSkipTLSVerify: types.OptionalBoolTrue}
	policy, err := signature.DefaultPolicy(system)
	if err != nil {
		t.Fatal(err)
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = policyContext.Destroy() }()
	if _, err := imagecopy.Image(ctx, policyContext, destination, source, &imagecopy.Options{
		SourceCtx: system, DestinationCtx: system, OciEncryptConfig: encrypt, OciEncryptLayers: layers,
	}); err != nil {
		t.Fatal(err)
	}
	options := oci.Options{ImageStoreDir: filepath.Join(root, "images"), PlainHTTPRegistries: []string{authority}, SignaturePolicyPath: policyPath}
	store := StoreOptions{GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"), GraphDriverName: "vfs"}
	lease, err := acquireStore(store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	resolver, err := oci.NewResolver(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveImageSource(ctx, resolver, reference, fixture.platform, lease.store, system); err == nil {
		t.Fatal("encrypted input succeeded without a key")
	}
	options.DecryptionKeys = []string{privatePath}
	resolver, err = oci.NewResolver(options)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveImageSource(ctx, resolver, reference, fixture.platform, lease.store, system)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyStoredManifest(ctx, lease.store, system, imagecatalog.Selection{Root: resolved.Root, Manifest: resolved.Selected, ImageID: resolved.ImageID, ConfigData: resolved.ConfigData}); err != nil {
		t.Fatal(err)
	}
	nativeReference, err := imagestorage.Transport.NewStoreReference(lease.store, nil, resolved.ImageID)
	if err != nil {
		t.Fatal(err)
	}
	nativeSource, err := nativeReference.NewImageSource(ctx, system)
	if err != nil {
		t.Fatal(err)
	}
	defaultManifest, _, err := nativeSource.GetManifest(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := nativeSource.Close(); err != nil {
		t.Fatal(err)
	}
	if resolved.SourceManifest == nil || resolved.Selected.Digest == resolved.SourceManifest.Digest || digest.FromBytes(defaultManifest) != resolved.Selected.Digest {
		t.Fatalf("decrypted selection must use native plaintext manifest and retain original identity: %+v", resolved)
	}
	options.DecryptionKeys = nil
	options.PullPolicy = string(oci.PullNewer)
	resolver, err = oci.NewResolver(options)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := ResolveImageSource(ctx, resolver, reference, fixture.platform, lease.store, system)
	if err != nil {
		t.Fatalf("unchanged encrypted source required a new decryption key: %v", err)
	}
	if unchanged.Selected.Digest != resolved.Selected.Digest {
		t.Fatal("newer policy changed an unchanged decrypted image")
	}
	if _, err := lease.store.DeleteImage(resolved.ImageID, true); err != nil {
		t.Fatal(err)
	}
	options.DecryptionKeys = []string{privatePath}
	options.PullPolicy = string(oci.PullMissing)
	resolver, err = oci.NewResolver(options)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ResolveImageSource(ctx, resolver, reference, fixture.platform, lease.store, system)
	if err != nil {
		t.Fatal(err)
	}
	if restored.SourceManifest == nil || restored.SourceManifest.Digest != resolved.SourceManifest.Digest {
		t.Fatal("restoring the native image lost its encrypted source identity")
	}
	options.DecryptionKeys = nil
	options.PullPolicy = string(oci.PullNewer)
	resolver, err = oci.NewResolver(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveImageSource(ctx, resolver, reference, fixture.platform, lease.store, system); err != nil {
		t.Fatalf("restored unchanged encrypted source required the key again: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	server.Close()
	if err := os.Remove(privatePath); err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string]string{
		"tag":       reference,
		"pinned":    authority + "/coopr/encrypted@" + resolved.SourceManifest.Digest.String(),
		"from-only": reference,
	} {
		t.Run(name, func(t *testing.T) {
			body := "from \"" + input + "\"\n"
			if name != "from-only" {
				body += "run \"printf decrypted > /proof\" network=\"none\"\n"
			}
			definition := parseWorkerDefinition(t, body)
			layout := filepath.Join(root, "result-"+name)
			_, err = BuildDefinitionSupervised(ctx, definition, planner.Options{
				Mode: planner.Build, Platform: fixture.platform.OS + "/" + fixture.platform.Architecture,
			}, SupervisedPlanOptions{
				Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
				ImageStoreDir: options.ImageStoreDir, PullPolicy: string(oci.PullNever), SignaturePolicyPath: policyPath,
			})
			if err != nil {
				t.Fatal(err)
			}
			manifest, _ := readPlanImage(t, layout)
			last := manifest.Layers[len(manifest.Layers)-1]
			if name == "from-only" {
				if len(manifest.Layers) != 1 || strings.HasSuffix(last.MediaType, "+encrypted") {
					t.Fatalf("decrypted FROM-only layers = %+v", manifest.Layers)
				}
				if binary := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "bin/busybox"); binary == "" {
					t.Fatal("decrypted FROM-only image lost its executable")
				}
				return
			}
			if proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof"); proof != "decrypted" {
				t.Fatalf("offline decrypted base RUN proof = %q", proof)
			}
		})
	}
}

func TestEncryptedRegistryIndexRetainsOfflinePlatformSelections(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for native encrypted index inputs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	fixture := liveBusyBoxImage(t, ctx)
	privatePath, publicPath := testEncryptionKeys(t, root)
	encrypt, layers, err := cli.EncryptConfig([]string{"jwe:" + publicPath}, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(registryserver.New())
	defer server.Close()
	authority := strings.TrimPrefix(server.URL, "http://")
	repositoryName := authority + "/coopr/encrypted-index"
	policyPath := writeComponentTestPolicy(t, root)
	system := &types.SystemContext{SignaturePolicyPath: policyPath, DockerInsecureSkipTLSVerify: types.OptionalBoolTrue}
	policy, err := signature.DefaultPolicy(system)
	if err != nil {
		t.Fatal(err)
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = policyContext.Destroy() }()
	var leaves []v1.Descriptor
	for _, architecture := range []string{"amd64", "arm64"} {
		var config v1.Image
		if err := json.Unmarshal(fixture.configData, &config); err != nil {
			t.Fatal(err)
		}
		config.Architecture = architecture
		configData, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		configDescriptor := descriptor(v1.MediaTypeImageConfig, configData)
		if err := fixture.store.Push(ctx, configDescriptor, bytes.NewReader(configData)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
			t.Fatal(err)
		}
		manifestData, err := fixture.store.Fetch(ctx, fixture.manifest)
		if err != nil {
			t.Fatal(err)
		}
		var manifest v1.Manifest
		decodeErr := json.NewDecoder(manifestData).Decode(&manifest)
		if err := manifestData.Close(); err != nil {
			t.Fatal(err)
		}
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		manifest.Config = configDescriptor
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		plain := descriptor(v1.MediaTypeImageManifest, raw)
		if err := fixture.store.Push(ctx, plain, bytes.NewReader(raw)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
			t.Fatal(err)
		}
		source, err := imagestore.LayoutReference(fixture.layout, plain)
		if err != nil {
			t.Fatal(err)
		}
		destination, err := docker.ParseReference("//" + repositoryName + ":" + architecture)
		if err != nil {
			t.Fatal(err)
		}
		encrypted, err := imagecopy.Image(ctx, policyContext, destination, source, &imagecopy.Options{
			SourceCtx: system, DestinationCtx: system, OciEncryptConfig: encrypt, OciEncryptLayers: layers,
		})
		if err != nil {
			t.Fatal(err)
		}
		leaf := descriptor(v1.MediaTypeImageManifest, encrypted)
		leaf.Platform = &v1.Platform{OS: "linux", Architecture: architecture}
		leaves = append(leaves, leaf)
	}
	indexData, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: leaves})
	if err != nil {
		t.Fatal(err)
	}
	index := descriptor(v1.MediaTypeImageIndex, indexData)
	repository, err := remote.NewRepository(repositoryName)
	if err != nil {
		t.Fatal(err)
	}
	repository.PlainHTTP = true
	if err := repository.PushReference(ctx, index, bytes.NewReader(indexData), "latest"); err != nil {
		t.Fatal(err)
	}
	options := oci.Options{ImageStoreDir: filepath.Join(root, "images"), PlainHTTPRegistries: []string{authority}, SignaturePolicyPath: policyPath, DecryptionKeys: []string{privatePath}}
	store := StoreOptions{GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"), GraphDriverName: "vfs"}
	lease, err := acquireStore(store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	resolver, err := oci.NewResolver(options)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaf := range leaves {
		selected, err := ResolveImageSource(ctx, resolver, repositoryName+":latest", *leaf.Platform, lease.store, system)
		if err != nil {
			t.Fatal(err)
		}
		if selected.Root.Digest != index.Digest || selected.SourceManifest == nil || selected.SourceManifest.Digest != leaf.Digest {
			t.Fatalf("encrypted platform selection lost index or origin: %+v", selected)
		}
	}
	server.Close()
	if err := os.Remove(privatePath); err != nil {
		t.Fatal(err)
	}
	options.PullPolicy, options.DecryptionKeys = string(oci.PullNever), nil
	resolver, err = oci.NewResolver(options)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaf := range leaves {
		if _, err := ResolveImageSource(ctx, resolver, repositoryName+":latest", *leaf.Platform, lease.store, system); err != nil {
			t.Fatalf("offline %s input: %v", leaf.Platform.Architecture, err)
		}
		other := v1.Platform{OS: "linux", Architecture: "amd64"}
		if leaf.Platform.Architecture == "amd64" {
			other.Architecture = "arm64"
		}
		if _, err := SelectImageSource(ctx, resolver, repositoryName+"@"+leaf.Digest.String(), other); err == nil {
			t.Fatal("encrypted leaf digest resolved a different platform from its index")
		}
	}
	cached, err := imagecatalog.AvailablePlatforms(ctx, options.ImageStoreDir, repositoryName+":latest")
	if err != nil || len(cached) != 2 {
		t.Fatalf("offline platform discovery = %+v, %v", cached, err)
	}
}

func testEncryptionKeys(t *testing.T, root string) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	privatePath, publicPath := filepath.Join(root, "private.pem"), filepath.Join(root, "public.pem")
	for path, block := range map[string]*pem.Block{
		privatePath: {Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)},
		publicPath:  {Type: "PUBLIC KEY", Bytes: public},
	} {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return privatePath, publicPath
}
