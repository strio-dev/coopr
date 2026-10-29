package oci

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestRemoteFileInputClassifiesArtifactsAndImages(t *testing.T) {
	_, repo, resolver, name := testRepository(t, registry.New())
	ctx := context.Background()
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	layer := push(t, repo, "text/plain", []byte("hello artifact\n"))
	layer.Annotations = map[string]string{v1.AnnotationTitle: "hello.txt"}
	config := push(t, repo, v1.MediaTypeEmptyJSON, []byte("{}"))
	for _, artifactType := range []string{"", "application/example.files"} {
		root := pushManifest(t, repo, VersionedManifest(config, []v1.Descriptor{layer}, artifactType), "files")
		resolved, err := resolver.ResolveRemoteFileInput(ctx, name+":files", platform)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Kind != Artifact || resolved.Root.Digest != root.Digest || resolved.Selected.Digest != root.Digest || len(resolved.Layers) != 1 || resolved.Layers[0].Annotations[v1.AnnotationTitle] != "hello.txt" {
			t.Fatalf("artifact selection = %+v", resolved)
		}
		if _, err := resolver.ResolveRemoteImage(ctx, name+":files", platform); err == nil {
			t.Fatal("image resolver accepted an artifact")
		}
	}
	root, _ := imageManifest(t, repo, platform, "image layer")
	resolved, err := resolver.ResolveRemoteFileInput(ctx, name+"@"+root.Digest.String(), platform)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Kind != Image || resolved.Selected.Digest != root.Digest || len(resolved.ConfigData) == 0 {
		t.Fatalf("image selection = %+v", resolved)
	}
}

func TestRemoteFileInputRetainsImagePolicyAndCancellation(t *testing.T) {
	_, repo, resolver, name := testRepository(t, registry.New())
	ctx := context.Background()
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	root, _ := imageManifest(t, repo, platform, "image layer")
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"reject"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver.system.SignaturePolicyPath = policy
	if _, err := resolver.ResolveRemoteFileInput(ctx, name+"@"+root.Digest.String(), platform); err == nil {
		t.Fatal("file-input image bypassed signature policy")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := resolver.ResolveRemoteFileInput(canceled, name+":files", platform); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled file resolution = %v", err)
	}
}

func TestRemoteFileInputSelectsArtifactIndexPlatform(t *testing.T) {
	_, repo, resolver, name := testRepository(t, registry.New())
	config := push(t, repo, v1.MediaTypeEmptyJSON, []byte("{}"))
	children := make(map[string]v1.Descriptor)
	for _, arch := range []string{"amd64", "arm64"} {
		layer := push(t, repo, "text/plain", []byte(arch))
		layer.Annotations = map[string]string{v1.AnnotationTitle: "payload"}
		child := pushManifest(t, repo, VersionedManifest(config, []v1.Descriptor{layer}, "application/example.files"), arch)
		child.Platform = &v1.Platform{OS: "linux", Architecture: arch}
		children[arch] = child
	}
	root := pushIndex(t, repo, "files", children["amd64"], children["arm64"])
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			resolved, err := resolver.ResolveRemoteFileInput(t.Context(), name+":files", v1.Platform{OS: "linux", Architecture: arch})
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Kind != Artifact || resolved.Root.Digest != root.Digest || resolved.Selected.Digest != children[arch].Digest {
				t.Fatalf("%s artifact index selection = %+v", arch, resolved)
			}
		})
	}
	if _, err := resolver.ResolveRemoteFileInput(t.Context(), name+":files", v1.Platform{OS: "linux", Architecture: "s390x"}); err == nil {
		t.Fatal("artifact index accepted an absent platform")
	}
}
