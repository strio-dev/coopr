package buildah

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	"go.podman.io/common/libimage"
	"go.podman.io/common/pkg/libartifact"
	artifacttypes "go.podman.io/common/pkg/libartifact/types"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"golang.org/x/sys/unix"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
)

func TestArtifactFileModesIgnoreHostUmask(t *testing.T) {
	if len(os.Args) >= 4 && os.Args[len(os.Args)-3] == "--" && os.Args[len(os.Args)-2] == "artifact-mode" {
		mask, err := strconv.ParseUint(os.Args[len(os.Args)-1], 8, 9)
		if err != nil {
			t.Fatal(err)
		}
		unix.Umask(int(mask))
		ctx := t.Context()
		store, system := artifactSourceTestStore(t)
		reference := "registry.example/team/modes:latest"
		artifactSourceTestAdd(t, ctx, store, reference, map[string]string{"payload": "content"})
		resolver, err := oci.NewResolver(oci.Options{PullPolicy: "never"})
		if err != nil {
			t.Fatal(err)
		}
		selected, err := resolveFileSource(ctx, resolver, reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, store, system)
		if err != nil {
			t.Fatal(err)
		}
		directory, cleanup, err := extractArtifactSource(ctx, store, selected)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cleanup() })
		info, err := os.Stat(filepath.Join(directory, "payload"))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("artifact file mode under umask %#o = %#o, want 0600", mask, got)
		}
		return
	}
	for _, mask := range []string{"000", "077"} {
		t.Run(mask, func(t *testing.T) {
			command := exec.CommandContext(t.Context(), testWorkerBinary(t), "-test.run=^TestArtifactFileModesIgnoreHostUmask$", "-test.short="+strconv.FormatBool(testing.Short()), "--", "artifact-mode", mask)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("artifact mode subprocess: %v\n%s", err, output)
			}
		})
	}
}

func TestResolveFileSourceUsesPodmanArtifactStoreOffline(t *testing.T) {
	ctx := context.Background()
	store, system := artifactSourceTestStore(t)
	reference := "registry.example/team/files:latest"
	native := artifactSourceTestAdd(t, ctx, store, reference, map[string]string{"hello.txt": "hello\n", "": "untitled\n"})
	for _, policy := range []string{"missing", "never"} {
		t.Run(policy, func(t *testing.T) {
			resolver, err := oci.NewResolver(oci.Options{PullPolicy: policy})
			if err != nil {
				t.Fatal(err)
			}
			selected, err := resolveFileSource(ctx, resolver, reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, store, system)
			if err != nil {
				t.Fatal(err)
			}
			if !selected.Artifact || selected.ImageID != "" || len(selected.ConfigData) != 0 || selected.Selected.Digest != native.Digest {
				t.Fatalf("artifact selection = %+v", selected)
			}
			directory, cleanup, err := extractArtifactSource(ctx, store, selected)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cleanup() })
			for name, want := range map[string]string{"hello.txt": "hello\n", strings.ReplaceAll(digest.FromString("untitled\n").String(), ":", "-"): "untitled\n"} {
				got, err := os.ReadFile(filepath.Join(directory, name))
				if err != nil || string(got) != want {
					t.Errorf("%s = %q, %v; want %q", name, got, err, want)
				}
			}
		})
	}
}

func TestResolveFileSourceNativeImagePrecedesArtifact(t *testing.T) {
	for _, test := range []struct{ reference, stored string }{
		{"registry.example/team/shared:latest", "registry.example/team/shared:latest"},
		{"coopr-demo:base", "localhost/coopr-demo:base"},
	} {
		t.Run(test.reference, func(t *testing.T) {
			ctx := context.Background()
			store, system := artifactSourceTestStore(t)
			_ = artifactSourceTestAdd(t, ctx, store, test.stored, map[string]string{"payload": "artifact"})
			layout := t.TempDir()
			source, err := orasoci.NewWithContext(ctx, layout)
			if err != nil {
				t.Fatal(err)
			}
			platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
			manifest, _ := sourceTestImage(t, ctx, source, platform, "native-image")
			imageID, err := ImportSelectedImage(ctx, store, system, layout, manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.AddNames(imageID, []string{test.stored}); err != nil {
				t.Fatal(err)
			}
			resolver, err := oci.NewResolver(oci.Options{PullPolicy: "never"})
			if err != nil {
				t.Fatal(err)
			}
			selected, err := resolveFileSource(ctx, resolver, test.reference, platform, store, system)
			if err != nil {
				t.Fatal(err)
			}
			if selected.Artifact || selected.ImageID != imageID {
				t.Fatalf("selected %+v instead of native image %s", selected, imageID)
			}
		})
	}
}

func TestExtractArtifactSourceVerifiesStoredBlobs(t *testing.T) {
	ctx := context.Background()
	store, system := artifactSourceTestStore(t)
	reference := "registry.example/team/files:latest"
	native := artifactSourceTestAdd(t, ctx, store, reference, map[string]string{"payload": "content"})
	resolver, err := oci.NewResolver(oci.Options{PullPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := resolveFileSource(ctx, resolver, reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, store, system)
	if err != nil {
		t.Fatal(err)
	}
	blob := native.Manifest.Layers[0]
	if err := os.WriteFile(filepath.Join(store.GraphRoot(), "artifacts", "blobs", blob.Digest.Algorithm().String(), blob.Digest.Encoded()), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if directory, cleanup, err := extractArtifactSource(ctx, store, selected); err == nil {
		_ = cleanup()
		t.Fatalf("accepted corrupt artifact blob into %s", directory)
	}
}

func TestExtractArtifactSourceRejectsUnsafeTitle(t *testing.T) {
	ctx := context.Background()
	store, system := artifactSourceTestStore(t)
	reference := "registry.example/team/files:latest"
	_ = artifactSourceTestAdd(t, ctx, store, reference, map[string]string{"../outside": "content"})
	resolver, err := oci.NewResolver(oci.Options{PullPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := resolveFileSource(ctx, resolver, reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, store, system)
	if err != nil {
		t.Fatal(err)
	}
	if directory, cleanup, err := extractArtifactSource(ctx, store, selected); err == nil {
		_ = cleanup()
		t.Fatalf("accepted unsafe artifact title into %s", directory)
	}
}

func TestExtractArtifactSourceDoesNotFollowUpdatedTag(t *testing.T) {
	ctx := context.Background()
	store, system := artifactSourceTestStore(t)
	reference := "registry.example/team/files:latest"
	_ = artifactSourceTestAdd(t, ctx, store, reference, map[string]string{"payload": "first"})
	resolver, err := oci.NewResolver(oci.Options{PullPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := resolveFileSource(ctx, resolver, reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, store, system)
	if err != nil {
		t.Fatal(err)
	}
	_ = artifactSourceTestAdd(t, ctx, store, reference, map[string]string{"payload": "second"})
	if directory, cleanup, err := extractArtifactSource(ctx, store, selected); err == nil {
		defer func() { _ = cleanup() }()
		if data, readErr := os.ReadFile(filepath.Join(directory, "payload")); readErr != nil || string(data) != "first" {
			t.Fatalf("frozen artifact selection followed updated tag: %q, %v", data, readErr)
		}
	} else if !errors.Is(err, artifacttypes.ErrArtifactNotExist) {
		t.Fatalf("frozen artifact extraction failed unexpectedly: %v", err)
	}
}

func TestExtractArtifactSourceRejectsRemovedSelection(t *testing.T) {
	ctx := t.Context()
	store, system := artifactSourceTestStore(t)
	reference := "registry.example/team/files:latest"
	native := artifactSourceTestAdd(t, ctx, store, reference, map[string]string{"payload": "original"})
	resolver, err := oci.NewResolver(oci.Options{PullPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := resolveFileSource(ctx, resolver, reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, store, system)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := libartifact.NewArtifactStore(filepath.Join(store.GraphRoot(), "artifacts"), system)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := libartifact.NewArtifactStorageReference(native.Digest.Encoded())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifacts.Remove(ctx, ref); err != nil {
		t.Fatal(err)
	}
	directory, cleanup, err := extractArtifactSource(ctx, store, selected)
	if cleanup != nil {
		t.Cleanup(func() { _ = cleanup() })
	}
	if !errors.Is(err, artifacttypes.ErrArtifactNotExist) {
		t.Fatalf("extraction after native removal = %v, want ErrArtifactNotExist", err)
	}
	if directory != "" || cleanup != nil {
		t.Fatalf("failed extraction returned directory %q or cleanup (present: %t)", directory, cleanup != nil)
	}
}

func TestExtractArtifactSourcePreparedFilesSurviveRemoval(t *testing.T) {
	ctx := t.Context()
	store, system := artifactSourceTestStore(t)
	reference := "registry.example/team/files:latest"
	native := artifactSourceTestAdd(t, ctx, store, reference, map[string]string{"payload": "original"})
	resolver, err := oci.NewResolver(oci.Options{PullPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := resolveFileSource(ctx, resolver, reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, store, system)
	if err != nil {
		t.Fatal(err)
	}
	directory, cleanup, err := extractArtifactSource(ctx, store, selected)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })
	artifacts, err := libartifact.NewArtifactStore(filepath.Join(store.GraphRoot(), "artifacts"), system)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := libartifact.NewArtifactStorageReference(native.Digest.Encoded())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifacts.Remove(ctx, ref); err != nil {
		t.Fatal(err)
	}
	// A later use must still honor the original selection after its tag is reused.
	_ = artifactSourceTestAdd(t, ctx, store, reference, map[string]string{"payload": "replacement"})
	reusedDirectory, reusedCleanup, err := extractArtifactSource(ctx, store, selected)
	if reusedCleanup != nil {
		t.Cleanup(func() { _ = reusedCleanup() })
	}
	if !errors.Is(err, artifacttypes.ErrArtifactNotExist) {
		t.Fatalf("second extraction after native removal and tag reuse = %v, want ErrArtifactNotExist", err)
	}
	if reusedDirectory != "" || reusedCleanup != nil {
		t.Fatalf("failed second extraction returned directory %q or cleanup (present: %t)", reusedDirectory, reusedCleanup != nil)
	}
	if data, err := os.ReadFile(filepath.Join(directory, "payload")); err != nil || string(data) != "original" {
		t.Fatalf("prepared payload after native removal and tag reuse = %q, %v; want original", data, err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepared directory after cleanup: %v, want os.ErrNotExist", err)
	}
}

func TestResolveFileSourceRegistryPoliciesAndTypeChanges(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	reference := strings.TrimPrefix(server.URL, "http://") + "/files:latest"
	producer, producerSystem := artifactSourceTestStore(t)
	consumer, consumerSystem := artifactSourceTestStore(t)
	first := artifactSourceTestAdd(t, ctx, producer, reference, map[string]string{"message": "first"})
	artifacts, err := libartifact.NewArtifactStore(filepath.Join(producer.GraphRoot(), "artifacts"), producerSystem)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := libartifact.NewArtifactReference(reference)
	if err != nil {
		t.Fatal(err)
	}
	push := func() {
		t.Helper()
		if _, err := artifacts.Push(ctx, ref, ref, libimage.CopyOptions{InsecureSkipTLSVerify: types.OptionalBoolTrue, Writer: io.Discard}); err != nil {
			t.Fatal(err)
		}
	}
	push()
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	resolve := func(policy string) (ResolvedImageSource, error) {
		t.Helper()
		resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false), PullPolicy: policy, SignaturePolicyPath: consumerSystem.SignaturePolicyPath, RetrySet: true, Retry: 0})
		if err != nil {
			t.Fatal(err)
		}
		return resolveFileSource(ctx, resolver, reference, platform, consumer, consumerSystem)
	}
	selected, err := resolve("missing")
	if err != nil || !selected.Artifact || selected.Selected.Digest != first.Digest {
		t.Fatalf("first artifact selection = %+v, %v", selected, err)
	}

	layout := t.TempDir()
	source, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	image, _ := sourceTestImage(t, ctx, source, platform, "image")
	if err := source.Tag(ctx, image, "image"); err != nil {
		t.Fatal(err)
	}
	repository, err := remote.NewRepository(strings.TrimSuffix(reference, ":latest"))
	if err != nil {
		t.Fatal(err)
	}
	repository.PlainHTTP = true
	if _, err := oras.Copy(ctx, source, "image", repository, "latest", oras.DefaultCopyOptions); err != nil {
		t.Fatal(err)
	}
	selected, err = resolve("newer")
	if err != nil || selected.Artifact || selected.Selected.Digest != image.Digest || selected.ImageID == "" {
		t.Fatalf("artifact-to-image selection = %+v, %v", selected, err)
	}

	second := artifactSourceTestAdd(t, ctx, producer, reference, map[string]string{"message": "second"})
	push()
	selected, err = resolve("newer")
	if err != nil || !selected.Artifact || selected.Selected.Digest != second.Digest {
		t.Fatalf("image-to-artifact selection = %+v, %v", selected, err)
	}
	directory, cleanup, err := extractArtifactSource(ctx, consumer, selected)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })
	if data, err := os.ReadFile(filepath.Join(directory, "message")); err != nil || string(data) != "second" {
		t.Fatalf("updated artifact file = %q, %v", data, err)
	}
	rejectPolicy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(rejectPolicy, []byte(`{"default":[{"type":"reject"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rejectResolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false), PullPolicy: "always", SignaturePolicyPath: rejectPolicy, RetrySet: true, Retry: 0})
	if err != nil {
		t.Fatal(err)
	}
	if rejected, err := resolveFileSource(ctx, rejectResolver, reference, platform, consumer, &types.SystemContext{SignaturePolicyPath: rejectPolicy}); err == nil {
		t.Fatalf("artifact pull bypassed reject policy: %+v", rejected)
	}
	// Remove the cached image's tag to exercise the artifact-only offline path.
	if err := consumer.RemoveNames(selectedImageID(t, consumer, reference), []string{reference}); err != nil {
		t.Fatal(err)
	}
	server.Close()
	for _, policy := range []string{"missing", "never", "newer"} {
		selected, err := resolve(policy)
		if err != nil || !selected.Artifact || selected.Selected.Digest != second.Digest {
			t.Fatalf("offline %s selection = %+v, %v", policy, selected, err)
		}
	}
	if selected, err := resolve("always"); err == nil {
		t.Fatalf("always accepted offline cached input: %+v", selected)
	}
}

func selectedImageID(t *testing.T, store storage.Store, reference string) string {
	t.Helper()
	image, err := store.Image(reference)
	if err != nil {
		t.Fatal(err)
	}
	return image.ID
}

func artifactSourceTestStore(t *testing.T) (storage.Store, *types.SystemContext) {
	t.Helper()
	root := t.TempDir()
	store, err := storage.GetStore(storage.StoreOptions{GraphDriverName: "vfs", GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = store.Shutdown(true) })
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return store, &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root}
}

func artifactSourceTestAdd(t *testing.T, ctx context.Context, store storage.Store, reference string, files map[string]string) *libartifact.Artifact {
	t.Helper()
	artifacts, err := libartifact.NewArtifactStore(filepath.Join(store.GraphRoot(), "artifacts"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := libartifact.NewArtifactReference(reference)
	if err != nil {
		t.Fatal(err)
	}
	blobs := make([]artifacttypes.ArtifactBlob, 0, len(files))
	for name, value := range files {
		blobs = append(blobs, artifacttypes.ArtifactBlob{FileName: name, BlobReader: bytes.NewBufferString(value)})
	}
	if _, err := artifacts.Add(ctx, ref, blobs, &artifacttypes.AddOptions{Replace: true, ArtifactMIMEType: "application/vnd.coopr.test.files", FileMIMEType: "application/octet-stream"}); err != nil {
		t.Fatal(err)
	}
	artifact, err := artifacts.Inspect(ctx, ref.ToArtifactStoreReference())
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}
