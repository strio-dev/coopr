package buildah

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	buildahdocker "go.podman.io/buildah/docker"
	"go.podman.io/image/v5/types"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestSelectedBaseAliasInitializesExactSameConfigManifest(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live selected-base alias coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	fixture := liveBusyBoxImage(t, ctx)
	storeOptions := cacheTestStore(root)
	lease, err := acquireStore(storeOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	system := &types.SystemContext{BigFilesTemporaryDir: root}
	ociLayout, ociManifest := annotatedVariant(t, ctx, fixture, v1.MediaTypeImageManifest, "oci")
	imageID, err := ImportSelectedImage(ctx, lease.store, system, ociLayout, ociManifest)
	if err != nil {
		t.Fatal(err)
	}
	dockerLayout, dockerManifest := annotatedVariant(t, ctx, fixture, define.Dockerv2ImageManifest, "docker")
	if got, err := ImportSelectedImage(ctx, lease.store, system, dockerLayout, dockerManifest); err != nil {
		t.Fatal(err)
	} else if got != imageID {
		t.Fatalf("Docker variant image ID = %s, want shared config ID %s", got, imageID)
	}
	defaultBefore, err := storedImageDefaultManifestDigest(lease.store, imageID)
	if err != nil {
		t.Fatal(err)
	}
	if defaultBefore != dockerManifest.Digest {
		t.Fatalf("default manifest before aliases = %s, want Docker %s", defaultBefore, dockerManifest.Digest)
	}
	layersBefore, err := lease.store.Layers()
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	aliasIDs := make(chan string, 8)
	errors := make(chan error, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			aliasID, err := selectedBuilderBase(ctx, lease.store, imageID, ociManifest.Digest)
			if err != nil {
				errors <- err
				return
			}
			aliasIDs <- aliasID
		}()
	}
	wait.Wait()
	close(aliasIDs)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	concurrentAlias := ""
	for aliasID := range aliasIDs {
		if concurrentAlias == "" {
			concurrentAlias = aliasID
		} else if aliasID != concurrentAlias {
			t.Fatalf("concurrent alias IDs = %s and %s", concurrentAlias, aliasID)
		}
	}

	for _, test := range []struct {
		name       string
		selected   digest.Digest
		annotation string
	}{
		{name: "oci", selected: ociManifest.Digest, annotation: "oci"},
		// Docker schema 2 has no manifest-annotation field.  The extra JSON
		// member deliberately distinguishes the selected raw manifest, while
		// Buildah correctly leaves ImageAnnotations empty for that format.
		{name: "docker", selected: dockerManifest.Digest},
	} {
		t.Run(test.name, func(t *testing.T) {
			aliasID, err := selectedBuilderBase(ctx, lease.store, imageID, test.selected)
			if err != nil {
				t.Fatal(err)
			}
			alias, err := lease.store.Image(aliasID)
			if err != nil {
				t.Fatal(err)
			}
			source, err := lease.store.Image(imageID)
			if err != nil {
				t.Fatal(err)
			}
			if alias.TopLayer != source.TopLayer {
				t.Fatalf("alias top layer = %s, want shared %s", alias.TopLayer, source.TopLayer)
			}
			builder, err := upstream.NewBuilder(ctx, lease.store, upstream.BuilderOptions{
				FromImage: aliasID, PullPolicy: define.PullNever, Format: define.OCIv1ImageManifest,
				Isolation: define.IsolationOCIRootless, SystemContext: system,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = builder.Delete() }()
			if got := digest.FromBytes(builder.Manifest); got != test.selected {
				t.Fatalf("builder manifest = %s, want %s", got, test.selected)
			}
			if got := builder.ImageAnnotations["coopr.test.variant"]; got != test.annotation {
				t.Fatalf("builder annotation = %q, want %q", got, test.annotation)
			}
		})
	}
	defaultAfter, err := storedImageDefaultManifestDigest(lease.store, imageID)
	if err != nil {
		t.Fatal(err)
	}
	if defaultAfter != defaultBefore {
		t.Fatalf("selected aliases changed source default from %s to %s", defaultBefore, defaultAfter)
	}
	layersAfter, err := lease.store.Layers()
	if err != nil {
		t.Fatal(err)
	}
	if len(layersAfter) != len(layersBefore) {
		t.Fatalf("selected aliases changed layer count from %d to %d", len(layersBefore), len(layersAfter))
	}
}

func TestSelectedBaseAliasSupportsConfigOnlyImage(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live config-only selected-base coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	storeOptions := cacheTestStore(root)
	base, err := BuildPlan(ctx, testPlan(t, "from \"scratch\"\nenv CONFIG_ONLY=\"1\"\n"), PlanOptions{
		Store: storeOptions, ContextDir: root, Isolation: "rootless", Runtime: "crun",
		Output: Output{Path: filepath.Join(root, "oci")},
	})
	if err != nil {
		t.Fatal(err)
	}
	ociManifest, err := oci.LayoutRoot(base.Layout)
	if err != nil {
		t.Fatal(err)
	}
	if got := importDockerAlternateForLayout(t, ctx, storeOptions, base.Layout); got != base.ImageID {
		t.Fatalf("Docker variant image ID = %s, want shared config ID %s", got, base.ImageID)
	}
	lease, err := acquireStore(storeOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	dockerManifest, err := storedImageDefaultManifestDigest(lease.store, base.ImageID)
	if err != nil {
		t.Fatal(err)
	}
	layersBefore, err := lease.store.Layers()
	if err != nil {
		t.Fatal(err)
	}
	if len(layersBefore) != 0 {
		t.Fatalf("config-only base has %d storage layers, want zero", len(layersBefore))
	}
	for _, selected := range []digest.Digest{ociManifest.Digest, dockerManifest} {
		aliasID, err := selectedBuilderBase(ctx, lease.store, base.ImageID, selected)
		if err != nil {
			t.Fatal(err)
		}
		alias, err := lease.store.Image(aliasID)
		if err != nil {
			t.Fatal(err)
		}
		if alias.TopLayer != "" {
			t.Fatalf("config-only alias top layer = %q, want empty", alias.TopLayer)
		}
		builder, err := upstream.NewBuilder(ctx, lease.store, upstream.BuilderOptions{
			FromImage: aliasID, PullPolicy: define.PullNever, Format: define.OCIv1ImageManifest,
			Isolation: define.IsolationOCIRootless, SystemContext: &types.SystemContext{BigFilesTemporaryDir: root},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := digest.FromBytes(builder.Manifest); got != selected {
			_ = builder.Delete()
			t.Fatalf("config-only builder manifest = %s, want %s", got, selected)
		}
		if err := builder.Delete(); err != nil {
			t.Fatal(err)
		}
	}
	layersAfter, err := lease.store.Layers()
	if err != nil {
		t.Fatal(err)
	}
	if len(layersAfter) != len(layersBefore) {
		t.Fatalf("config-only aliases changed layer count from %d to %d", len(layersBefore), len(layersAfter))
	}
}

func TestGraphBuildConsumesPlanningSelectedManifestAfterCollision(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live selected graph-base coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	fixture := liveBusyBoxImage(t, ctx)
	storeOptions := cacheTestStore(root)
	lease, err := acquireStore(storeOptions)
	if err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{BigFilesTemporaryDir: root}
	ociLayout, ociManifest := compressedAnnotatedVariant(t, ctx, fixture, "selected-oci")
	imageID, err := ImportSelectedImage(ctx, lease.store, system, ociLayout, ociManifest)
	if err != nil {
		t.Fatal(err)
	}
	dockerLayout, dockerManifest := annotatedVariant(t, ctx, fixture, define.Dockerv2ImageManifest, "later-docker")
	if _, err := ImportSelectedImage(ctx, lease.store, system, dockerLayout, dockerManifest); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}

	const reference = "fixture.local/selected:latest"
	platform := fixture.platform.OS + "/" + fixture.platform.Architecture
	plan := testPlan(t, "from \""+reference+"\"\nenv BUILT=\"1\"\n")
	var firstLayer digest.Digest
	for _, attempt := range []string{"cold", "warm"} {
		result, err := BuildPlan(ctx, plan, PlanOptions{
			Store: storeOptions, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output: Output{Path: filepath.Join(root, attempt), DisableCompression: false}, SystemContext: system,
			ResolvedBases: map[ResolvedBaseKey]ResolvedImageSource{
				{Reference: reference, Platform: platform}: {
					ImageID: imageID, Selected: ociManifest, ConfigData: fixture.configData,
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, result.Layout)
		if got := manifest.Annotations["coopr.test.variant"]; got != "selected-oci" {
			t.Fatalf("%s graph inherited annotation %q, want planning-selected OCI annotation", attempt, got)
		}
		if len(manifest.Layers) != 1 || len(image.RootFS.DiffIDs) != 1 {
			t.Fatalf("%s metadata-only graph layers=%d diffIDs=%d, want selected base layer only", attempt, len(manifest.Layers), len(image.RootFS.DiffIDs))
		}
		if manifest.Layers[0].MediaType != v1.MediaTypeImageLayerGzip {
			t.Fatalf("%s layer media type = %q, want gzip", attempt, manifest.Layers[0].MediaType)
		}
		blob, err := os.Open(filepath.Join(result.Layout, "blobs", manifest.Layers[0].Digest.Algorithm().String(), manifest.Layers[0].Digest.Encoded()))
		if err != nil {
			t.Fatal(err)
		}
		compressed, gzipErr := gzip.NewReader(blob)
		if gzipErr == nil {
			_, gzipErr = io.Copy(io.Discard, compressed)
			gzipErr = errors.Join(gzipErr, compressed.Close())
		}
		closeErr := blob.Close()
		if gzipErr != nil || closeErr != nil {
			t.Fatalf("%s layer is not valid gzip: %v, close: %v", attempt, gzipErr, closeErr)
		}
		if attempt == "cold" {
			firstLayer = manifest.Layers[0].Digest
		} else if manifest.Layers[0].Digest != firstLayer {
			t.Fatalf("warm metadata-only graph changed gzip layer from %s to %s", firstLayer, manifest.Layers[0].Digest)
		}
	}
}

func compressedAnnotatedVariant(t *testing.T, ctx context.Context, fixture liveBusyBoxGraph, annotation string) (string, v1.Descriptor) {
	t.Helper()
	fetch := func(descriptor v1.Descriptor) []byte {
		stream, err := fixture.store.Fetch(ctx, descriptor)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = stream.Close() }()
		var buffer bytes.Buffer
		if _, err := buffer.ReadFrom(stream); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(fetch(fixture.manifest), &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Layers) != 1 {
		t.Fatalf("fixture layers=%d, want one", len(manifest.Layers))
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(fetch(manifest.Layers[0])); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	manifest.Layers[0] = descriptor(v1.MediaTypeImageLayerGzip, compressed.Bytes())
	manifest.Annotations = map[string]string{"coopr.test.variant": annotation}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	selected := descriptor(v1.MediaTypeImageManifest, data)
	layout := filepath.Join(t.TempDir(), "compressed-variant")
	store, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	for _, blob := range []struct {
		descriptor v1.Descriptor
		data       []byte
	}{
		{manifest.Config, fetch(manifest.Config)},
		{manifest.Layers[0], compressed.Bytes()},
		{selected, data},
	} {
		if err := store.Push(ctx, blob.descriptor, bytes.NewReader(blob.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Tag(ctx, selected, "latest"); err != nil {
		t.Fatal(err)
	}
	return layout, selected
}

func annotatedVariant(t *testing.T, ctx context.Context, fixture liveBusyBoxGraph, mediaType, annotation string) (string, v1.Descriptor) {
	t.Helper()
	fetch := func(descriptor v1.Descriptor) []byte {
		stream, err := fixture.store.Fetch(ctx, descriptor)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = stream.Close() }()
		var buffer bytes.Buffer
		if _, err := buffer.ReadFrom(stream); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(fetch(fixture.manifest), &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.MediaType = mediaType
	manifest.Annotations = map[string]string{"coopr.test.variant": annotation}
	if mediaType == define.Dockerv2ImageManifest {
		manifest.Config.MediaType = dockerImageConfigMediaType
		for index := range manifest.Layers {
			if manifest.Layers[index].MediaType == v1.MediaTypeImageLayer {
				manifest.Layers[index].MediaType = buildahdocker.V2S2MediaTypeUncompressedLayer
			} else {
				manifest.Layers[index].MediaType = "application/vnd.docker.image.rootfs.diff.tar.gzip"
			}
		}
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := v1.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
	layout := filepath.Join(t.TempDir(), "variant")
	store, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	for _, blob := range append([]v1.Descriptor{manifest.Config}, manifest.Layers...) {
		data := fetch(blob)
		if err := store.Push(ctx, blob, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Push(ctx, descriptor, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := store.Tag(ctx, descriptor, "latest"); err != nil {
		t.Fatal(err)
	}
	return layout, descriptor
}
