package buildah

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/oci"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/pkg/libartifact"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
)

func TestResolveFileSourceSelectsArtifactIndexPlatform(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
			t.Cleanup(server.Close)
			repository := artifactIndexTestRepository(t, server.URL)
			children := artifactIndexTestPublish(t, repository, "first")
			store, system := artifactSourceTestStore(t)
			selected, err := artifactIndexTestResolve(t, repository.Reference.String()+":latest", arch, "missing", store, system)
			if err != nil {
				t.Fatal(err)
			}
			if !selected.Artifact || selected.ImageID != "" || selected.Root.Digest != children[arch].Digest || selected.Selected.Digest != children[arch].Digest {
				t.Fatalf("%s stored artifact selection = %+v", arch, selected)
			}
			artifactIndexTestPayload(t, store, selected, "first-"+arch)
			server.Close()
			for _, policy := range []string{"missing", "never"} {
				offline, err := artifactIndexTestResolve(t, repository.Reference.String()+":latest", arch, policy, store, system)
				if err != nil || offline.Selected.Digest != children[arch].Digest {
					t.Fatalf("offline %s selection = %+v, %v", policy, offline, err)
				}
			}
		})
	}
}

func TestResolveFileSourceArtifactIndexRefreshesSelectedPlatform(t *testing.T) {
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	repository := artifactIndexTestRepository(t, server.URL)
	first := artifactIndexTestPublish(t, repository, "first")
	store, system := artifactSourceTestStore(t)
	reference := repository.Reference.String() + ":latest"
	for _, arch := range []string{"amd64", "arm64"} {
		selected, err := artifactIndexTestResolve(t, reference, arch, "missing", store, system)
		if err != nil || selected.Selected.Digest != first[arch].Digest {
			t.Fatalf("shared store %s selection = %+v, %v", arch, selected, err)
		}
		artifactIndexTestPayload(t, store, selected, "first-"+arch)
		if arch == "amd64" {
			if wrong, err := artifactIndexTestResolve(t, reference, "arm64", "never", store, system); err == nil {
				t.Fatalf("never reused amd64 artifact for uncached arm64: %+v", wrong)
			}
		}
	}
	second := artifactIndexTestPublish(t, repository, "second")
	for _, policy := range []string{"newer", "always"} {
		selected, err := artifactIndexTestResolve(t, reference, "arm64", policy, store, system)
		if err != nil || selected.Selected.Digest != second["arm64"].Digest {
			t.Fatalf("%s refreshed selection = %+v, %v", policy, selected, err)
		}
		artifactIndexTestPayload(t, store, selected, "second-arm64")
	}
}

func TestResolveFileSourceArtifactIndexByDigest(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
			t.Cleanup(server.Close)
			repository := artifactIndexTestRepository(t, server.URL)
			children := artifactIndexTestPublish(t, repository, "pinned")
			root, err := repository.Resolve(t.Context(), "latest")
			if err != nil {
				t.Fatal(err)
			}
			reference := repository.Reference.String() + "@" + root.Digest.String()
			store, system := artifactSourceTestStore(t)
			selected, err := artifactIndexTestResolve(t, reference, arch, "missing", store, system)
			if err != nil || selected.Selected.Digest != children[arch].Digest {
				t.Fatalf("digest-pinned %s selection = %+v, %v", arch, selected, err)
			}
			artifactIndexTestPayload(t, store, selected, "pinned-"+arch)
			server.Close()
			offline, err := artifactIndexTestResolve(t, reference, arch, "never", store, system)
			if err != nil || offline.Selected.Digest != children[arch].Digest {
				t.Fatalf("digest-pinned offline %s selection = %+v, %v", arch, offline, err)
			}
			artifactIndexTestPayload(t, store, offline, "pinned-"+arch)
		})
	}
}

func TestResolveFileSourceArtifactIndexRejectsPolicyAndMissingPlatform(t *testing.T) {
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	repository := artifactIndexTestRepository(t, server.URL)
	artifactIndexTestPublish(t, repository, "first")
	store, system := artifactSourceTestStore(t)
	reference := repository.Reference.String() + ":latest"
	if _, err := artifactIndexTestResolve(t, reference, "s390x", "always", store, system); err == nil {
		t.Fatal("artifact index accepted an absent platform")
	}
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"reject"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rejectedSystem := *system
	rejectedSystem.SignaturePolicyPath = policy
	selected, err := artifactIndexTestResolve(t, reference, "amd64", "always", store, &rejectedSystem)
	var rejected signature.PolicyRequirementError
	if !errors.As(err, &rejected) {
		t.Fatalf("indexed artifact rejection did not come from signature policy: selection=%+v, error=%v", selected, err)
	}
}

func TestResolveFileSourceArtifactIndexReplacementLifecycle(t *testing.T) {
	t.Run("remote flat and index replacements", func(t *testing.T) {
		server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
		t.Cleanup(server.Close)
		repository := artifactIndexTestRepository(t, server.URL)
		reference := repository.Reference.String() + ":latest"
		store, system := artifactSourceTestStore(t)
		flat := artifactIndexTestPublish(t, repository, "flat")
		if err := repository.Tag(t.Context(), flat["amd64"], "latest"); err != nil {
			t.Fatal(err)
		}
		selected, err := artifactIndexTestResolve(t, reference, "amd64", "missing", store, system)
		if err != nil || !selected.Artifact || selected.Selected.Digest != flat["amd64"].Digest {
			t.Fatalf("initial flat selection = %+v, %v", selected, err)
		}
		indexed := artifactIndexTestPublish(t, repository, "indexed")
		selected, err = artifactIndexTestResolve(t, reference, "amd64", "newer", store, system)
		if err != nil || selected.Selected.Digest != indexed["amd64"].Digest {
			t.Fatalf("flat-to-index selection = %+v, %v", selected, err)
		}
		for _, policy := range []string{"missing", "never"} {
			selected, err = artifactIndexTestResolve(t, reference, "amd64", policy, store, system)
			if err != nil || selected.Selected.Digest != indexed["amd64"].Digest {
				t.Fatalf("%s reused stale flat input: %+v, %v", policy, selected, err)
			}
		}
		flat = artifactIndexTestPublish(t, repository, "replacement")
		if err := repository.Tag(t.Context(), flat["amd64"], "latest"); err != nil {
			t.Fatal(err)
		}
		selected, err = artifactIndexTestResolve(t, reference, "amd64", "newer", store, system)
		if err != nil || selected.Selected.Digest != flat["amd64"].Digest {
			t.Fatalf("index-to-flat selection = %+v, %v", selected, err)
		}
		server.Close()
		selected, err = artifactIndexTestResolve(t, reference, "amd64", "never", store, system)
		if err != nil || selected.Selected.Digest != flat["amd64"].Digest {
			t.Fatalf("offline replacement selection = %+v, %v", selected, err)
		}
		artifactIndexTestPayload(t, store, selected, "replacement-amd64")
		artifactIndexTestRemove(t, store, system, reference)
		if selected, err := artifactIndexTestResolve(t, reference, "amd64", "never", store, system); err == nil {
			t.Fatalf("removed flat input resurrected old index: %+v", selected)
		}
	})
	t.Run("native flat replacement", func(t *testing.T) {
		server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
		t.Cleanup(server.Close)
		repository := artifactIndexTestRepository(t, server.URL)
		reference := repository.Reference.String() + ":latest"
		store, system := artifactSourceTestStore(t)
		artifactIndexTestPublish(t, repository, "indexed")
		if _, err := artifactIndexTestResolve(t, reference, "amd64", "missing", store, system); err != nil {
			t.Fatal(err)
		}
		server.Close()
		flat := artifactSourceTestAdd(t, t.Context(), store, reference, map[string]string{"payload": "native replacement"})
		selected, err := artifactIndexTestResolve(t, reference, "amd64", "never", store, system)
		if err != nil || selected.Selected.Digest != flat.Digest {
			t.Fatalf("native replacement selection = %+v, %v", selected, err)
		}
		artifactIndexTestPayload(t, store, selected, "native replacement")
		artifactIndexTestRemove(t, store, system, reference)
		if selected, err := artifactIndexTestResolve(t, reference, "amd64", "never", store, system); err == nil {
			t.Fatalf("removed native replacement resurrected old index: %+v", selected)
		}
	})
	t.Run("image replacement", func(t *testing.T) {
		server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
		t.Cleanup(server.Close)
		repository := artifactIndexTestRepository(t, server.URL)
		reference := repository.Reference.String() + ":latest"
		store, system := artifactSourceTestStore(t)
		artifactIndexTestPublish(t, repository, "indexed")
		if _, err := artifactIndexTestResolve(t, reference, "amd64", "missing", store, system); err != nil {
			t.Fatal(err)
		}
		source, err := orasoci.NewWithContext(t.Context(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		image, _ := sourceTestImage(t, t.Context(), source, v1.Platform{OS: "linux", Architecture: "amd64"}, "replacement")
		if err := source.Tag(t.Context(), image, "image"); err != nil {
			t.Fatal(err)
		}
		if _, err := oras.Copy(t.Context(), source, "image", repository, "latest", oras.DefaultCopyOptions); err != nil {
			t.Fatal(err)
		}
		selected, err := artifactIndexTestResolve(t, reference, "amd64", "newer", store, system)
		if err != nil || selected.Artifact || selected.Selected.Digest != image.Digest || selected.ImageID == "" {
			t.Fatalf("index-to-image selection = %+v, %v", selected, err)
		}
		if err := store.RemoveNames(selected.ImageID, []string{reference}); err != nil {
			t.Fatal(err)
		}
		server.Close()
		if selected, err := artifactIndexTestResolve(t, reference, "amd64", "never", store, system); err == nil {
			t.Fatalf("removed image name resurrected old index: %+v", selected)
		}
	})
}

func artifactIndexTestRemove(t *testing.T, store storage.Store, system *types.SystemContext, reference string) {
	t.Helper()
	artifacts, err := libartifact.NewArtifactStore(filepath.Join(store.GraphRoot(), "artifacts"), system)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := libartifact.NewArtifactStorageReference(reference)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifacts.Remove(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
}

func artifactIndexTestRepository(t *testing.T, serverURL string) *remote.Repository {
	t.Helper()
	repository, err := remote.NewRepository(strings.TrimPrefix(serverURL, "http://") + "/files")
	if err != nil {
		t.Fatal(err)
	}
	repository.PlainHTTP = true
	return repository
}

func artifactIndexTestPublish(t *testing.T, repository *remote.Repository, marker string) map[string]v1.Descriptor {
	t.Helper()
	push := func(mediaType string, data []byte) v1.Descriptor {
		desc := content.NewDescriptorFromBytes(mediaType, data)
		if err := repository.Push(t.Context(), desc, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		return desc
	}
	config := push(v1.MediaTypeEmptyJSON, []byte("{}"))
	children := make(map[string]v1.Descriptor)
	for _, arch := range []string{"amd64", "arm64"} {
		layer := push("text/plain", []byte(marker+"-"+arch))
		layer.Annotations = map[string]string{v1.AnnotationTitle: "payload"}
		data, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: "application/example.files", Config: config, Layers: []v1.Descriptor{layer}})
		if err != nil {
			t.Fatal(err)
		}
		child := push(v1.MediaTypeImageManifest, data)
		child.Platform = &v1.Platform{OS: "linux", Architecture: arch}
		children[arch] = child
	}
	data, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{children["amd64"], children["arm64"]}})
	if err != nil {
		t.Fatal(err)
	}
	root := push(v1.MediaTypeImageIndex, data)
	if err := repository.Tag(t.Context(), root, "latest"); err != nil {
		t.Fatal(err)
	}
	return children
}

func artifactIndexTestResolve(t *testing.T, reference, arch, policy string, store storage.Store, system *types.SystemContext) (ResolvedImageSource, error) {
	t.Helper()
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false), PullPolicy: policy, SignaturePolicyPath: system.SignaturePolicyPath, RetrySet: true, Retry: 0})
	if err != nil {
		t.Fatal(err)
	}
	return resolveFileSource(t.Context(), resolver, reference, v1.Platform{OS: "linux", Architecture: arch}, store, system)
}

func artifactIndexTestPayload(t *testing.T, store storage.Store, selected ResolvedImageSource, want string) {
	t.Helper()
	directory, cleanup, err := extractArtifactSource(t.Context(), store, selected)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })
	data, err := os.ReadFile(filepath.Join(directory, "payload"))
	if err != nil || string(data) != want {
		t.Fatalf("artifact payload = %q, %v; want %q", data, err, want)
	}
}
