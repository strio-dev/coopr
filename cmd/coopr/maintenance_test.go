package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/imagecatalog"
	"coopr/internal/imagestore"
	"coopr/internal/oci"
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
		{"components"}, {"component", "ls"}, {"component", "inspect"}, {"component", "rm"},
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
	catalog, err := selectedStoreCatalog(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := refreshSharedNativeCatalog(context.Background(), options, catalog, ""); err != nil {
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
	entries, err := imagecatalog.List(context.Background(), catalog)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Reference == "native-only:latest" || entry.Reference == "localhost/native-only:latest" {
			t.Fatalf("removed native alias remains cataloged: %s", entry.Reference)
		}
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
	catalog, err := selectedStoreCatalog(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := refreshSharedNativeCatalog(context.Background(), options, catalog, "qualified"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := imagecatalog.Lookup(context.Background(), catalog, "qualified:latest", v1.Platform{OS: "linux", Architecture: "amd64"}); err != nil || !found {
		entries, _ := imagecatalog.List(context.Background(), catalog)
		t.Fatalf("short lookup alias for Docker-qualified native name: found=%t err=%v entries=%+v", found, err, entries)
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

func TestRefreshSharedNativeCatalogDiscoversAllAndForeignPlatforms(t *testing.T) {
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

	catalog := filepath.Join(root, "catalog")
	if err := refreshSharedNativeCatalog(ctx, options, catalog, ""); err != nil {
		t.Fatal(err)
	}
	entries, err := imagecatalog.List(ctx, catalog)
	if err != nil {
		t.Fatal(err)
	}
	platformCounts := map[string]int{}
	for _, entry := range entries {
		platformCounts[entry.Reference] = len(entry.Platforms)
	}
	if got := platformCounts["multi:latest"]; got != 2 {
		t.Fatalf("multi-platform catalog selections = %d, want 2; entries=%+v", got, entries)
	}
	if got := platformCounts["foreign-only:latest"]; got != 1 {
		t.Fatalf("foreign-only catalog selections = %d, want 1; entries=%+v", got, entries)
	}
	indexedRoot, indexData, indexedSelections, complete, err := imagecatalog.LookupIndex(ctx, catalog, "multi:latest")
	if err != nil {
		t.Fatal(err)
	}
	if !complete || indexedRoot.Digest != index.Digest || len(indexData) == 0 || len(indexedSelections) != 2 {
		t.Fatalf("refreshed native index complete=%t root=%s data=%d selections=%d", complete, indexedRoot.Digest, len(indexData), len(indexedSelections))
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
		GraphRoot: graphRoot, RunRoot: runRoot, GraphDriverName: "vfs", Shared: true,
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
