package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coopr/internal/buildah"
	"coopr/internal/imagestore"
	"coopr/internal/oci"
	"coopr/internal/storeactivity"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestMaintenanceCommandShape(t *testing.T) {
	root := newRootCommand()
	for _, path := range [][]string{
		{"images"}, {"image", "ls"}, {"image", "inspect"}, {"image", "rm"},
		{"component", "build"}, {"component", "copy"}, {"component", "ls"}, {"component", "inspect"}, {"component", "rm"},
		{"system", "df"}, {"system", "prune"}, {"cache", "df"}, {"cache", "prune"},
	} {
		command, _, err := root.Find(path)
		if err != nil || command == root {
			t.Fatalf("command %v = %v, %v", path, command, err)
		}
	}
	for _, path := range [][]string{{"system", "prune"}, {"cache", "prune"}} {
		command, _, _ := root.Find(path)
		if command.Flags().Lookup("dry-run") == nil {
			t.Fatalf("command %v has no --dry-run", path)
		}
	}
}

func TestComponentListingUsesSingularCommand(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"component", "ls"}, &stdout, &stderr); code != 0 || stdout.String() != "NAME\tDIGEST\n" {
		t.Fatalf("component ls status=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"components"}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), `unknown command "components"`) {
		t.Fatalf("plural shortcut status=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--help"}, &stdout, &stderr); code != 0 || strings.Contains(stdout.String(), "\n  components ") {
		t.Fatalf("root help status=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestImageRemoveAcceptsNativeOnlyName(t *testing.T) {
	root := t.TempDir()
	options := maintenanceStoreOptions(root)
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	layout, descriptor, imageID := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: "amd64"}, "native-only")
	if _, err := store.WriteLayout(context.Background(), layout, descriptor, "native-only"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	status := run([]string{
		"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs",
		"image", "rm", "native-only",
	}, &stdout, &stderr)
	if status != 0 {
		t.Fatalf("native-only rm status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	store, err = imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if _, err := store.Resolve(context.Background(), "native-only", v1.Platform{OS: "linux", Architecture: "amd64"}); err == nil {
		t.Fatal("removed native-only name still resolves")
	}
	if _, err := store.Tag(imageID.Encoded(), "retained"); err != nil {
		t.Fatalf("native-only rm deleted the underlying image: %v", err)
	}

}

func TestImageRemoveShortNameRemovesMatchedDockerQualifiedTag(t *testing.T) {
	root := t.TempDir()
	options := maintenanceStoreOptions(root)
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	layout, descriptor, _ := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: "amd64"}, "docker-qualified")
	if _, err := store.WriteLayout(context.Background(), layout, descriptor, "docker.io/library/qualified:latest"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	status := run([]string{
		"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs",
		"image", "rm", "qualified",
	}, &stdout, &stderr)
	if status != 0 {
		t.Fatalf("qualified rm status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	store, err = imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if _, err := store.MatchedName("docker.io/library/qualified:latest"); err == nil {
		t.Fatal("short removal left the matched Docker-qualified native tag")
	}
}

func TestNativeImageListingDiscoversAllAndForeignPlatforms(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	options := maintenanceStoreOptions(root)
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	amd := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm := v1.Platform{OS: "linux", Architecture: "arm64"}
	amdLayout, amdManifest, _ := maintenanceImageLayout(t, amd, "amd64")
	armLayout, armManifest, _ := maintenanceImageLayout(t, arm, "arm64")
	indexLayout := filepath.Join(t.TempDir(), "index")
	index, _, err := oci.AssembleImageIndex(ctx, indexLayout, []oci.ImageVariant{
		{Layout: amdLayout, Manifest: amdManifest, Platform: amd},
		{Layout: armLayout, Manifest: armManifest, Platform: arm},
	}, "oci")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteIndexLayout(ctx, indexLayout, index, "multi"); err != nil {
		t.Fatal(err)
	}
	foreignLayout, foreignManifest, _ := maintenanceImageLayout(t, arm, "foreign")
	if _, err := store.WriteLayout(ctx, foreignLayout, foreignManifest, "foreign-only"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := nativeImageEntries(ctx, options, "")
	if err != nil {
		t.Fatal(err)
	}
	platformCounts := map[string]int{}
	for _, entry := range entries {
		platformCounts[entry.Reference] = len(entry.Platforms)
	}
	if got := platformCounts["localhost/multi:latest"]; got != 2 {
		t.Fatalf("multi-platform native selections = %d, want 2; entries=%+v", got, entries)
	}
	if got := platformCounts["localhost/foreign-only:latest"]; got != 1 {
		t.Fatalf("foreign-only native selections = %d, want 1; entries=%+v", got, entries)
	}
	if err := buildah.WithStore(options, func(backend storage.Store) error {
		indexedRoot, indexData, indexedSelections, err := oci.StoredImageSelections(ctx, backend, "multi")
		if err != nil {
			return err
		}
		if indexedRoot.Digest != index.Digest || len(indexData) == 0 || len(indexedSelections) != 2 {
			t.Fatalf("native index root=%s data=%d selections=%d", indexedRoot.Digest, len(indexData), len(indexedSelections))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "images"}, &stdout, &stderr)
	if status != 0 {
		t.Fatalf("images status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	rows := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(rows) != 3 {
		t.Fatalf("images should show one row per real native tag: %q", stdout.String())
	}
	for _, row := range rows[1:] {
		if !strings.HasPrefix(row, "localhost/") {
			t.Fatalf("images showed synthetic lookup alias instead of native name: %q", row)
		}
	}
}

func maintenanceStoreOptions(root string) buildah.StoreOptions {
	graphRoot := filepath.Join(root, "graph")
	runRoot := filepath.Join(root, "run")
	return buildah.StoreOptions{
		GraphRoot: graphRoot, RunRoot: runRoot, GraphDriverName: "vfs",
		Native: storage.StoreOptions{GraphRoot: graphRoot, RunRoot: runRoot, GraphDriverName: "vfs"},
	}
}

func maintenanceImageLayout(t *testing.T, platform v1.Platform, label string) (string, v1.Descriptor, digest.Digest) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := orasoci.NewWithContext(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(v1.Image{
		Platform: platform, RootFS: v1.RootFS{Type: "layers"}, Config: v1.ImageConfig{Labels: map[string]string{"test": label}},
	})
	if err != nil {
		t.Fatal(err)
	}
	imageID := digest.FromBytes(config)
	configDescriptor := v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: imageID, Size: int64(len(config))}
	if err := store.Push(ctx, configDescriptor, bytes.NewReader(config)); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: configDescriptor})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromBytes(manifest), Size: int64(len(manifest))}
	if err := store.Push(ctx, descriptor, bytes.NewReader(manifest)); err != nil {
		t.Fatal(err)
	}
	if err := store.Tag(ctx, descriptor, descriptor.Digest.String()); err != nil {
		t.Fatal(err)
	}
	return dir, descriptor, imageID
}

func TestNativeImageEntriesWaitsForNativeMaintenance(t *testing.T) {
	options := buildah.StoreOptions{GraphRoot: t.TempDir(), RunRoot: t.TempDir(), ImageStore: t.TempDir(), GraphDriverName: "vfs"}
	for _, root := range buildah.ActivityRoots(options, "") {
		exclusive, err := storeactivity.AcquireExclusive(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, readErr := nativeImageEntries(ctx, options, "")
		cancel()
		closeErr := exclusive.Close()
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		if !errors.Is(readErr, context.DeadlineExceeded) {
			t.Fatalf("native image read bypassed exclusive root %q: %v", root, readErr)
		}
	}
}
