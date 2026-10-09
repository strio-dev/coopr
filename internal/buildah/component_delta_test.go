package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/oci"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestApplyComponentDeltaValidatesInputs(t *testing.T) {
	if err := ApplyComponentDelta(context.Background(), nil, "", "", nil, t.TempDir()); err == nil || !strings.Contains(err.Error(), "requires store") {
		t.Fatalf("missing input error = %v", err)
	}
	if err := ApplyComponentDelta(context.Background(), nil, "caller", "result", &upstream.Builder{}, "relative"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative scratch error = %v", err)
	}
}

func TestApplyComponentDeltaPreservesCallerAndAppliesDeletion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless component delta build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	driver := os.Getenv("COOPR_TEST_BUILDAH_DRIVER")
	if driver == "" {
		driver = "vfs"
	}
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{"keep": "keep\n", "remove": "remove\n", "added": "added\n"} {
		if err := os.WriteFile(filepath.Join(contextDir, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := storage.GetStore(storage.StoreOptions{
		GraphDriverName: driver, GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.Shutdown(true); err != nil {
			t.Errorf("shutdown component delta store: %v", err)
		}
	})
	network, err := newNetworkInterface(store)
	if err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{BigFilesTemporaryDir: root}
	options := upstream.BuilderOptions{
		FromImage: "scratch", PullPolicy: define.PullNever, Isolation: define.IsolationChroot,
		Format: define.OCIv1ImageManifest, NetworkInterface: network, SystemContext: system,
	}
	newBuilder := func(base string) *upstream.Builder {
		t.Helper()
		options.FromImage = base
		builder, err := upstream.NewBuilder(ctx, store, options)
		if err != nil {
			t.Fatal(err)
		}
		return builder
	}
	commit := func(builder *upstream.Builder) string {
		t.Helper()
		imageID, _, _, err := builder.Commit(ctx, nil, upstream.CommitOptions{
			PreferredManifestType: define.OCIv1ImageManifest, SystemContext: system,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := builder.Delete(); err != nil {
			t.Fatal(err)
		}
		return imageID
	}

	base := newBuilder("scratch")
	for _, name := range []string{"keep", "remove"} {
		if err := base.Add("/"+name, false, upstream.AddAndCopyOptions{ContextDir: contextDir}, name); err != nil {
			t.Fatal(err)
		}
	}
	baseRoot, err := base.Mount(base.MountLabel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(baseRoot, "tree", "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseRoot, "tree", "old", "gone"), []byte("gone"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseRoot, "file-to-dir"), []byte("replaced"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := base.Unmount(); err != nil {
		t.Fatal(err)
	}
	baseID := commit(base)

	component := newBuilder(baseID)
	if err := component.Add("/added", false, upstream.AddAndCopyOptions{ContextDir: contextDir}, "added"); err != nil {
		t.Fatal(err)
	}
	mount, err := component.Mount(component.MountLabel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(mount, "remove")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(mount, "tree", "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(mount, "tree", "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "tree", "old", "new"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(mount, "file-to-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(mount, "file-to-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "file-to-dir", "child"), []byte("child"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("keep", filepath.Join(mount, "link")); err != nil {
		t.Fatal(err)
	}
	if err := component.Unmount(); err != nil {
		t.Fatal(err)
	}
	componentID := commit(component)
	dirty := newBuilder(baseID)
	if err := dirty.Add("/added", false, upstream.AddAndCopyOptions{ContextDir: contextDir}, "added"); err != nil {
		t.Fatal(err)
	}
	if err := ApplyComponentDelta(ctx, store, baseID, componentID, dirty, root); err == nil || !strings.Contains(err.Error(), "not fresh") {
		t.Fatalf("dirty delta destination error = %v", err)
	}
	if err := dirty.Delete(); err != nil {
		t.Fatal(err)
	}

	compacted := newBuilder(baseID)
	if err := ApplyComponentDelta(ctx, store, baseID, componentID, compacted, root); err != nil {
		t.Fatal(err)
	}
	compactedID := commit(compacted)
	configData, err := packageImageConfig(ctx, store, compactedID, system)
	if err != nil {
		t.Fatal(err)
	}
	var image v1.Image
	if err := json.Unmarshal(configData, &image); err != nil {
		t.Fatal(err)
	}
	if len(image.RootFS.DiffIDs) != 2 {
		t.Fatalf("compacted image has %d filesystem layers, want caller plus one component layer", len(image.RootFS.DiffIDs))
	}
	inspect := newBuilder(compactedID)
	defer func() { _ = inspect.Delete() }()
	rootfs, err := inspect.Mount(inspect.MountLabel)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inspect.Unmount() }()
	for _, name := range []string{"keep", "added"} {
		contents, err := os.ReadFile(filepath.Join(rootfs, name))
		if err != nil || string(contents) != name+"\n" {
			t.Fatalf("compacted %s = %q, %v", name, contents, err)
		}
	}
	if _, err := os.Stat(filepath.Join(rootfs, "remove")); !os.IsNotExist(err) {
		t.Fatalf("removed file survives component delta: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootfs, "tree", "old", "gone")); !os.IsNotExist(err) {
		t.Fatalf("removed directory entry survives component delta: %v", err)
	}
	for path, want := range map[string]string{
		filepath.Join("tree", "old", "new"):   "new",
		filepath.Join("file-to-dir", "child"): "child",
	} {
		contents, err := os.ReadFile(filepath.Join(rootfs, path))
		if err != nil || string(contents) != want {
			t.Fatalf("compacted %s = %q, %v; want %q", path, contents, err, want)
		}
	}
	link, err := os.Readlink(filepath.Join(rootfs, "link"))
	if err != nil || link != "keep" {
		t.Fatalf("compacted symlink = %q, %v; want keep", link, err)
	}

	// A scratch caller has no top layer. Its component result can still have
	// several internal layers, all of which must be folded into one output.
	zeroID := importZeroLayerImage(t, ctx, store, root)
	zeroImage, err := store.Image(zeroID)
	if err != nil {
		t.Fatal(err)
	}
	if zeroImage.TopLayer != "" {
		t.Fatalf("scratch caller unexpectedly has layer %q", zeroImage.TopLayer)
	}
	first := newBuilder(zeroID)
	if err := first.Add("/keep", false, upstream.AddAndCopyOptions{ContextDir: contextDir}, "keep"); err != nil {
		t.Fatal(err)
	}
	firstID := commit(first)
	second := newBuilder(firstID)
	if err := second.Add("/added", false, upstream.AddAndCopyOptions{ContextDir: contextDir}, "added"); err != nil {
		t.Fatal(err)
	}
	secondID := commit(second)
	scratchOutput := newBuilder(zeroID)
	if err := ApplyComponentDelta(ctx, store, zeroID, secondID, scratchOutput, root); err != nil {
		t.Fatal(err)
	}
	scratchOutputID := commit(scratchOutput)
	configData, err = packageImageConfig(ctx, store, scratchOutputID, system)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(configData, &image); err != nil {
		t.Fatal(err)
	}
	if len(image.RootFS.DiffIDs) != 1 {
		t.Fatalf("scratch component image has %d layers, want one", len(image.RootFS.DiffIDs))
	}
	scratchInspect := newBuilder(scratchOutputID)
	defer func() { _ = scratchInspect.Delete() }()
	scratchRoot, err := scratchInspect.Mount(scratchInspect.MountLabel)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = scratchInspect.Unmount() }()
	for _, name := range []string{"keep", "added"} {
		contents, err := os.ReadFile(filepath.Join(scratchRoot, name))
		if err != nil || string(contents) != name+"\n" {
			t.Fatalf("scratch component %s = %q, %v", name, contents, err)
		}
	}
	noOp := newBuilder(zeroID)
	if err := ApplyComponentDelta(ctx, store, zeroID, zeroID, noOp, root); err != nil {
		t.Fatalf("config-only scratch component delta: %v", err)
	}
	_ = commit(noOp)
}

func importZeroLayerImage(t *testing.T, ctx context.Context, store storage.Store, root string) string {
	t.Helper()
	layout := filepath.Join(root, "zero-layer-source")
	source, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	configData, err := json.Marshal(v1.Image{
		Platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH},
		RootFS:   v1.RootFS{Type: "layers"}, Config: v1.ImageConfig{Env: []string{"CALLER=scratch"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := oci.Descriptor(v1.MediaTypeImageConfig, configData)
	if err := source.Push(ctx, config, bytes.NewReader(configData)); err != nil {
		t.Fatal(err)
	}
	manifestData, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config,
	})
	if err != nil {
		t.Fatal(err)
	}
	selected := oci.Descriptor(v1.MediaTypeImageManifest, manifestData)
	if err := source.Push(ctx, selected, bytes.NewReader(manifestData)); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "zero-layer-policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	imageID, err := ImportSelectedImage(ctx, store, &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root}, layout, selected)
	if err != nil {
		t.Fatal(err)
	}
	return imageID
}
