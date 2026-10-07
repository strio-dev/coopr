package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah/define"
	buildahdocker "go.podman.io/buildah/docker"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestFromOnlyBuildReusesExactBaseUnlessOutputPolicyChanges(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live FROM-only reuse coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	platform := runtime.GOOS + "/" + runtime.GOARCH
	selected, found, err := nativeFixtureSelection(ctx, store, base.reference, v1.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH})
	if err != nil || !found {
		t.Fatalf("lookup base selection: found=%v err=%v", found, err)
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	def, err := definition.Parse(strings.NewReader(fmt.Sprintf("from %q\n", base.reference)))
	if err != nil {
		t.Fatal(err)
	}
	build := func(name, format string, epoch *int64, rewrite bool) Result {
		t.Helper()
		plannerOptions := planner.Options{Mode: planner.Build, Platform: platform}
		if epoch != nil {
			plannerOptions.Arguments = map[string]string{"SOURCE_DATE_EPOCH": fmt.Sprint(*epoch)}
		}
		result, err := BuildDefinitionSupervised(ctx, def, plannerOptions, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output:              Output{Path: filepath.Join(root, name), Format: format},
			SignaturePolicyPath: policy, RewriteTimestamp: rewrite,
			Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	exact := build("exact", "", nil, false)
	if exact.ImageID != selected.ImageID || exact.ManifestDigest != selected.Manifest.Digest.String() {
		t.Fatalf("FROM-only output = image %s manifest %s, want exact base %s/%s", exact.ImageID, exact.ManifestDigest, selected.ImageID, selected.Manifest.Digest)
	}

	docker := build("docker", "docker", nil, false)
	if docker.ManifestDigest == selected.Manifest.Digest.String() {
		t.Fatal("format-changing FROM-only output reused the OCI base manifest")
	}
	rootDescriptor, err := oci.LayoutRoot(docker.Layout)
	if err != nil {
		t.Fatal(err)
	}
	if rootDescriptor.MediaType != define.Dockerv2ImageManifest {
		t.Fatalf("format-changing output media type = %q", rootDescriptor.MediaType)
	}
	if got := importDockerAlternateForLayout(t, ctx, store, exact.Layout); got != selected.ImageID {
		t.Fatalf("base Docker alternate image ID = %s, want selected base %s", got, selected.ImageID)
	}
	// A mutable native name now uses the native default manifest. Pin the OCI
	// manifest to prove alternate formats sharing its config ID stay selectable.
	def, err = definition.Parse(strings.NewReader(fmt.Sprintf("from %q\n", base.reference+"@"+selected.Manifest.Digest.String())))
	if err != nil {
		t.Fatal(err)
	}
	exactAfterDocker := build("exact-after-docker", "", nil, false)
	if exactAfterDocker.ManifestDigest != selected.Manifest.Digest.String() {
		t.Fatalf("FROM-only output after Docker collision = %s, want selected OCI manifest %s", exactAfterDocker.ManifestDigest, selected.Manifest.Digest)
	}

	epoch := int64(1_700_000_000)
	withEpoch := build("epoch", "", &epoch, false)
	if withEpoch.ManifestDigest == selected.Manifest.Digest.String() {
		t.Fatal("SOURCE_DATE_EPOCH FROM-only output reused the base manifest")
	}
	_, epochImage := readPlanImage(t, withEpoch.Layout)
	if epochImage.Created == nil || epochImage.Created.Unix() != epoch {
		t.Fatalf("SOURCE_DATE_EPOCH output created = %v", epochImage.Created)
	}

	rewritten := build("rewrite", "", nil, true)
	if rewritten.ManifestDigest == selected.Manifest.Digest.String() {
		t.Fatal("rewrite-timestamp FROM-only output reused the base manifest")
	}
}

func TestInstructionCacheExportsSelectedManifestAfterFormatCollision(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live selected-cache-manifest coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf("from %q\nrun \"printf cache >/proof\" network=\"none\"\n", base.reference))
	epoch := int64(1_700_000_000)
	plan.SourceDateEpoch = &epoch
	build := func(name, format string) Result {
		t.Helper()
		result, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output:              Output{Path: filepath.Join(root, name), Format: format},
			SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	ociCold := build("oci-cold", "")
	if got := importDockerAlternateForLayout(t, ctx, store, ociCold.Layout); got != ociCold.ImageID {
		t.Fatalf("Docker alternate image ID = %s, want OCI config ID %s", got, ociCold.ImageID)
	}
	siblingID, sibling := importAnnotatedAlternateForLayout(t, ctx, store, ociCold.Layout)
	if siblingID != ociCold.ImageID {
		t.Fatalf("annotated alternate image ID = %s, want OCI config ID %s", siblingID, ociCold.ImageID)
	}
	assertCompressedSelectedExportIgnoresDefaultSibling(t, ctx, store, ociCold.Layout, ociCold.ImageID, sibling)
	ociWarm := build("oci-warm", "")
	if ociWarm.ManifestDigest != ociCold.ManifestDigest {
		t.Fatalf("warm OCI cache exported manifest %s after sibling %s became default, want cached OCI %s", ociWarm.ManifestDigest, sibling.Digest, ociCold.ManifestDigest)
	}
	_, warmImage := readPlanImage(t, ociWarm.Layout)
	if warmImage.Config.Labels["org.example.sibling"] != "" {
		t.Fatalf("warm OCI cache inherited sibling image configuration: %+v", warmImage.Config.Labels)
	}
	warmManifest, _ := readPlanImage(t, ociWarm.Layout)
	if warmManifest.Annotations["org.example.sibling"] != "" {
		t.Fatalf("warm OCI cache inherited sibling manifest annotations: %+v", warmManifest.Annotations)
	}
}

func assertCompressedSelectedExportIgnoresDefaultSibling(t *testing.T, ctx context.Context, options StoreOptions, selectedLayout, imageID string, sibling v1.Descriptor) {
	t.Helper()
	selected, err := oci.LayoutRoot(selectedLayout)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Errorf("close collision store: %v", err)
		}
	}()
	source, err := lease.store.Image(imageID)
	if err != nil {
		t.Fatal(err)
	}
	selectedManifest, err := lease.store.ImageBigData(imageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+selected.Digest.String())
	if err != nil {
		t.Fatal(err)
	}
	siblingManifest, err := lease.store.ImageBigData(imageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+sibling.Digest.String())
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(selectedManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	configData, err := lease.store.ImageBigData(imageID, manifest.Config.Digest.String())
	if err != nil {
		t.Fatal(err)
	}
	aliasID := digest.FromString("coopr selected compressed export collision " + t.Name()).Encoded()
	alias, err := lease.store.CreateImage(aliasID, nil, source.TopLayer, "", &storage.ImageOptions{
		Digest: sibling.Digest, Digests: []digest.Digest{sibling.Digest, selected.Digest},
		BigData: []storage.ImageBigDataOption{
			{Key: storage.ImageDigestBigDataKey, Data: siblingManifest, Digest: sibling.Digest},
			{Key: storage.ImageDigestManifestBigDataNamePrefix + "-" + sibling.Digest.String(), Data: siblingManifest, Digest: sibling.Digest},
			{Key: storage.ImageDigestManifestBigDataNamePrefix + "-" + selected.Digest.String(), Data: selectedManifest, Digest: selected.Digest},
			{Key: manifest.Config.Digest.String(), Data: configData, Digest: manifest.Config.Digest},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "selected")
	result, err := copyStoredOutputSelected(ctx, lease.store, alias.ID, Output{Path: output}, &types.SystemContext{BigFilesTemporaryDir: t.TempDir()}, &selected.Digest)
	if err != nil {
		t.Fatal(err)
	}
	root, err := oci.LayoutRoot(result.Layout)
	if err != nil {
		t.Fatal(err)
	}
	if root.Digest == sibling.Digest {
		t.Fatalf("selected compressed export followed default sibling %s instead of selected source %s", sibling.Digest, selected.Digest)
	}
	exported, _ := readPlanImage(t, result.Layout)
	if exported.Annotations["org.example.sibling"] != "" {
		t.Fatalf("selected compressed export inherited default sibling annotations: %+v", exported.Annotations)
	}
}

func importAnnotatedAlternateForLayout(t *testing.T, ctx context.Context, options StoreOptions, layout string) (string, v1.Descriptor) {
	t.Helper()
	root, err := oci.LayoutRoot(layout)
	if err != nil {
		t.Fatal(err)
	}
	read := func(item v1.Descriptor) []byte {
		data, err := os.ReadFile(filepath.Join(layout, "blobs", item.Digest.Algorithm().String(), item.Digest.Encoded()))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(read(root), &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Annotations = map[string]string{"org.example.sibling": "default"}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	sibling := descriptor(v1.MediaTypeImageManifest, manifestData)
	siblingLayout := t.TempDir()
	target, err := orasoci.NewWithContext(ctx, siblingLayout)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range append([]v1.Descriptor{manifest.Config}, manifest.Layers...) {
		if err := target.Push(ctx, item, bytes.NewReader(read(item))); err != nil {
			t.Fatal(err)
		}
	}
	if err := target.Push(ctx, sibling, bytes.NewReader(manifestData)); err != nil {
		t.Fatal(err)
	}
	lease, err := acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	imageID, importErr := ImportSelectedImage(ctx, lease.store, &types.SystemContext{BigFilesTemporaryDir: t.TempDir()}, siblingLayout, sibling)
	closeErr := lease.Close()
	if importErr != nil {
		t.Fatal(importErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return imageID, sibling
}

func importDockerAlternateForLayout(t *testing.T, ctx context.Context, options StoreOptions, layout string) string {
	t.Helper()
	root, err := oci.LayoutRoot(layout)
	if err != nil {
		t.Fatal(err)
	}
	read := func(item v1.Descriptor) []byte {
		data, err := os.ReadFile(filepath.Join(layout, "blobs", item.Digest.Algorithm().String(), item.Digest.Encoded()))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(read(root), &manifest); err != nil {
		t.Fatal(err)
	}
	dockerLayout := t.TempDir()
	target, err := orasoci.NewWithContext(ctx, dockerLayout)
	if err != nil {
		t.Fatal(err)
	}
	configData := read(manifest.Config)
	manifest.Config.MediaType = dockerImageConfigMediaType
	if err := target.Push(ctx, manifest.Config, bytes.NewReader(configData)); err != nil {
		t.Fatal(err)
	}
	for index := range manifest.Layers {
		layerData := read(manifest.Layers[index])
		if manifest.Layers[index].MediaType == v1.MediaTypeImageLayer {
			manifest.Layers[index].MediaType = buildahdocker.V2S2MediaTypeUncompressedLayer
		} else {
			manifest.Layers[index].MediaType = "application/vnd.docker.image.rootfs.diff.tar.gzip"
		}
		if err := target.Push(ctx, manifest.Layers[index], bytes.NewReader(layerData)); err != nil {
			t.Fatal(err)
		}
	}
	manifest.MediaType = define.Dockerv2ImageManifest
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	dockerRoot := descriptor(define.Dockerv2ImageManifest, manifestData)
	if err := target.Push(ctx, dockerRoot, bytes.NewReader(manifestData)); err != nil {
		t.Fatal(err)
	}
	lease, err := acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	imageID, importErr := ImportSelectedImage(ctx, lease.store, &types.SystemContext{BigFilesTemporaryDir: t.TempDir()}, dockerLayout, dockerRoot)
	closeErr := lease.Close()
	if importErr != nil {
		t.Fatal(importErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return imageID
}
