package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coopr/internal/buildah"
	"coopr/internal/componentstore"
	"coopr/internal/imagestore"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	"coopr/internal/storeactivity"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage"
	"go.podman.io/storage"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestMaintenanceCommandShape(t *testing.T) {
	root := newRootCommand()
	for _, path := range [][]string{
		{"images"}, {"image", "ls"}, {"image", "inspect"}, {"image", "rm"}, {"image", "prune"},
		{"component", "build"}, {"component", "copy"}, {"component", "ls"}, {"components"}, {"component", "inspect"}, {"component", "rm"},
		{"system", "df"}, {"system", "prune"},
	} {
		command, _, err := root.Find(path)
		if err != nil || command.Name() != path[len(path)-1] {
			t.Fatalf("command %v = %v, %v", path, command, err)
		}
	}
	for _, path := range [][]string{{"system", "prune"}, {"image", "prune"}} {
		command, _, _ := root.Find(path)
		for _, flag := range []string{"dry-run", "all"} {
			if command.Flags().Lookup(flag) == nil {
				t.Fatalf("command %v has no --%s", path, flag)
			}
		}
	}
	imagePrune, _, _ := root.Find([]string{"image", "prune"})
	systemPrune, _, _ := root.Find([]string{"system", "prune"})
	if imagePrune.Flags().Lookup("build-cache") == nil || systemPrune.Flags().Lookup("build-cache") != nil {
		t.Fatal("--build-cache must belong to image prune, as in Podman")
	}
}

func TestCacheCommandRemoved(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if status := run([]string{"cache", "df"}, &stdout, &stderr); status == 0 || !strings.Contains(stderr.String(), `unknown command "cache"`) {
		t.Fatalf("removed cache command: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
}

func TestComponentListingSupportsBothCommands(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"component", "ls"}, &stdout, &stderr); code != 0 || strings.Join(strings.Fields(stdout.String()), " ") != "NAME DIGEST" || strings.Contains(stdout.String(), "\t") {
		t.Fatalf("component ls status=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"components"}, &stdout, &stderr); code != 0 || strings.Join(strings.Fields(stdout.String()), " ") != "NAME DIGEST" {
		t.Fatalf("plural shortcut status=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "\n  components ") {
		t.Fatalf("root help status=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestImageInspectUsesNativeImageData(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	layout, descriptor, imageID := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: "amd64"}, "inspect")
	if _, err := store.WriteLayout(context.Background(), layout, descriptor, "inspect"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image", "inspect", "inspect"}, &stdout, &stderr)
	if status != 0 {
		t.Fatalf("inspect status=%d stderr=%s", status, &stderr)
	}
	var inspected libimage.ImageData
	if err := json.Unmarshal(stdout.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.ID != imageID.Encoded() || inspected.Digest != descriptor.Digest || inspected.Architecture != "amd64" || inspected.Os != "linux" || inspected.Config == nil || inspected.Config.Labels["test"] != "inspect" {
		t.Fatalf("inspection lost native identity or configuration: %s", &stdout)
	}
	for _, internal := range []string{"ConfigData", "SourceManifest", "platforms"} {
		if strings.Contains(stdout.String(), `"`+internal+`"`) {
			t.Fatalf("inspection exposes internal %s: %s", internal, &stdout)
		}
	}
}

func TestComponentListingAlignsNamesAndDigests(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	directory, err := componentstore.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	layout, descriptor, _ := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: "amd64"}, "component-list")
	source, err := orasoci.New(layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := localstore.Put(context.Background(), directory, source, descriptor, "short:latest", "long-component-name:latest"); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if status := run([]string{"component", "ls"}, &stdout, &stderr); status != 0 {
		t.Fatalf("component ls status=%d stderr=%s", status, &stderr)
	}
	rows := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(rows) != 3 {
		t.Fatalf("component listing lost tags: %s", &stdout)
	}
	for _, row := range rows[1:] {
		if strings.Index(row, descriptor.Digest.String()) != strings.Index(rows[0], "DIGEST") {
			t.Fatalf("component columns do not align with header: %s", &stdout)
		}
	}
	want := stdout.String()
	stdout.Reset()
	stderr.Reset()
	if status := run([]string{"components"}, &stdout, &stderr); status != 0 || stdout.String() != want {
		t.Fatalf("components differs from component ls: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
}

func TestComponentInspectAcceptsLocalReferences(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	directory, err := componentstore.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	layout, descriptor, _ := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: "amd64"}, "component-inspect")
	source, err := orasoci.New(layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := localstore.Put(context.Background(), directory, source, descriptor, "inspect"); err != nil {
		t.Fatal(err)
	}
	for _, reference := range []string{"inspect", "local:inspect", descriptor.Digest.String(), "local:" + descriptor.Digest.String()} {
		t.Run(reference, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if status := run([]string{"component", "inspect", reference}, &stdout, &stderr); status != 0 {
				t.Fatalf("inspect status=%d stderr=%s", status, &stderr)
			}
			var actual v1.Descriptor
			if err := json.Unmarshal(stdout.Bytes(), &actual); err != nil {
				t.Fatal(err)
			}
			if actual.Digest != descriptor.Digest || actual.MediaType != descriptor.MediaType || actual.Size != descriptor.Size {
				t.Fatalf("inspect changed descriptor: %+v", actual)
			}
		})
	}
}

func TestImageInspectPreservesSelectedManifestFormat(t *testing.T) {
	ctx := context.Background()
	options := maintenanceStoreOptions(t.TempDir())
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	layoutPath, original, imageID := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: "amd64"}, "formats")
	layout, err := orasoci.New(layoutPath)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(layoutPath, "blobs", "sha256", original.Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	configData, err := os.ReadFile(filepath.Join(layoutPath, "blobs", "sha256", imageID.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(configData, &config); err != nil {
		t.Fatal(err)
	}
	config["comment"] = "docker-comment"
	config["history"] = []v1.History{{Comment: "oci-comment"}}
	config["config"].(map[string]any)["Healthcheck"] = map[string]any{"Test": []string{"CMD", "true"}}
	configData, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	imageID = digest.FromBytes(configData)
	manifest.Config.Digest = imageID
	manifest.Config.Size = int64(len(configData))
	if err := layout.Push(ctx, manifest.Config, bytes.NewReader(configData)); err != nil {
		t.Fatal(err)
	}
	manifest.Annotations = map[string]string{"inspect-format": "oci"}
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	original = v1.Descriptor{MediaType: manifest.MediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
	if err := layout.Push(ctx, original, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := layout.Tag(ctx, original, original.Digest.String()); err != nil {
		t.Fatal(err)
	}
	manifest.MediaType = "application/vnd.docker.distribution.manifest.v2+json"
	manifest.Config.MediaType = "application/vnd.docker.container.image.v1+json"
	manifest.Annotations = nil
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	docker := v1.Descriptor{MediaType: manifest.MediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
	if err := layout.Push(ctx, docker, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := layout.Tag(ctx, docker, docker.Digest.String()); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []struct {
		name       string
		descriptor v1.Descriptor
	}{{"formats-oci", original}, {"formats-docker", docker}} {
		if _, err := store.WriteLayout(ctx, layoutPath, variant.descriptor, variant.name); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []struct {
		name       string
		descriptor v1.Descriptor
	}{{"formats-oci", docker}, {original.Digest.String(), original}, {"localhost/formats-oci@" + original.Digest.String(), original}, {"formats-docker", docker}, {docker.Digest.String(), docker}} {
		t.Run(variant.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image", "inspect", variant.name}, &stdout, &stderr); status != 0 {
				t.Fatalf("inspect status=%d stderr=%s", status, &stderr)
			}
			var inspected libimage.ImageData
			if err := json.Unmarshal(stdout.Bytes(), &inspected); err != nil {
				t.Fatal(err)
			}
			if inspected.ID != imageID.Encoded() || inspected.Digest != variant.descriptor.Digest || inspected.ManifestType != variant.descriptor.MediaType {
				t.Fatalf("inspect selected wrong variant: id=%s digest=%s mediaType=%s want=%+v", inspected.ID, inspected.Digest, inspected.ManifestType, variant.descriptor)
			}
			if variant.descriptor.MediaType == v1.MediaTypeImageManifest {
				if inspected.Annotations["inspect-format"] != "oci" || inspected.Comment != "oci-comment" || inspected.HealthCheck != nil {
					t.Fatalf("OCI inspection used other format metadata: %+v", inspected)
				}
			} else if len(inspected.Annotations) != 0 || inspected.Comment != "docker-comment" || inspected.HealthCheck == nil || strings.Join(inspected.HealthCheck.Test, " ") != "CMD true" {
				t.Fatalf("Docker inspection used other format metadata: %+v", inspected)
			}
		})
	}
	if err := buildah.WithStore(options, func(backend storage.Store) error {
		return backend.SetNames(imageID.Encoded(), nil)
	}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image", "inspect", original.Digest.String()}, &stdout, &stderr); status != 0 {
		t.Fatalf("retained unnamed manifest inspect status=%d stderr=%s", status, &stderr)
	}
	var inspected libimage.ImageData
	if err := json.Unmarshal(stdout.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.ID != imageID.Encoded() || inspected.Digest != original.Digest || inspected.ManifestType != original.MediaType || len(inspected.RepoTags) != 0 {
		t.Fatalf("unnamed manifest inspection lost pinned identity or added names: %s", &stdout)
	}
	t.Run("never-named", func(t *testing.T) {
		options := maintenanceStoreOptions(t.TempDir())
		store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.WriteLayout(ctx, layoutPath, original, ""); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image", "inspect", original.Digest.String()}, &stdout, &stderr); status != 0 {
			t.Fatalf("never-named inspect status=%d stderr=%s", status, &stderr)
		}
		var inspected libimage.ImageData
		if err := json.Unmarshal(stdout.Bytes(), &inspected); err != nil {
			t.Fatal(err)
		}
		if inspected.ID != imageID.Encoded() || inspected.Digest != original.Digest || inspected.ManifestType != original.MediaType || inspected.Annotations["inspect-format"] != "oci" || len(inspected.RepoTags) != 0 {
			t.Fatalf("never-named inspection lost identity or added names: %s", &stdout)
		}
	})
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

	entries, err := nativeImageEntries(ctx, options)
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
		if strings.Index(row, "sha256:") != strings.Index(rows[0], "DIGEST") || strings.Index(row, "linux/") != strings.Index(rows[0], "PLATFORMS") {
			t.Fatalf("image columns do not align with headers: %q", stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "\t") {
		t.Fatalf("images uses unaligned literal tabs: %s", &stdout)
	}
	stdout.Reset()
	stderr.Reset()
	status = run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image", "inspect", "multi"}, &stdout, &stderr)
	if status != 0 {
		t.Fatalf("index inspect status=%d stderr=%s", status, &stderr)
	}
	var inspected v1.Index
	if err := json.Unmarshal(stdout.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if len(inspected.Manifests) != 2 {
		t.Fatalf("index inspection lost platforms: %s", &stdout)
	}
	for i, expected := range []v1.Descriptor{amdManifest, armManifest} {
		if inspected.Manifests[i].Digest != expected.Digest || inspected.Manifests[i].Platform == nil {
			t.Fatalf("index inspection lost child identity: %s", &stdout)
		}
	}
	updatedLayout := filepath.Join(t.TempDir(), "updated-index")
	updated, _, err := oci.AssembleImageIndex(ctx, updatedLayout, []oci.ImageVariant{
		{Layout: armLayout, Manifest: armManifest, Platform: arm},
		{Layout: amdLayout, Manifest: amdManifest, Platform: amd},
	}, "oci")
	if err != nil {
		t.Fatal(err)
	}
	store, err = imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteIndexLayout(ctx, updatedLayout, updated, "multi"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	status = run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image", "inspect", index.Digest.String()}, &stdout, &stderr)
	if status != 0 {
		t.Fatalf("retained index inspect status=%d stderr=%s", status, &stderr)
	}
	if err := json.Unmarshal(stdout.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if len(inspected.Manifests) != 2 || inspected.Manifests[0].Digest != amdManifest.Digest || inspected.Manifests[1].Digest != armManifest.Digest {
		t.Fatalf("retained index inspection used current mutable list: %s", &stdout)
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
		_, readErr := nativeImageEntries(ctx, options)
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

// maintenanceFailingWriter also exercises failures after earlier writes succeed.
type maintenanceFailingWriter struct {
	remaining int
	err       error
}

func (w *maintenanceFailingWriter) Write(data []byte) (int, error) {
	if w.remaining == 0 {
		return 0, w.err
	}
	w.remaining--
	return len(data), nil
}

func TestMaintenanceCommandsPropagateOutputErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		args   []string
		writes int
	}{
		{"df header", []string{"system", "df"}, 0},
		{"df body", []string{"system", "df"}, 1},
		{"image prune count", []string{"image", "prune", "--dry-run"}, 0},
		{"image prune summary", []string{"image", "prune", "--dry-run"}, 1},
		{"system prune count", []string{"system", "prune", "--dry-run"}, 0},
		{"system prune components", []string{"system", "prune", "--dry-run"}, 1},
		{"system prune summary", []string{"system", "prune", "--dry-run"}, 2},
		{"image rm", []string{"image", "rm", "output-error"}, 0},
		{"component rm", []string{"component", "rm", "output-error"}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			if testing.Short() && test.args[1] != "rm" {
				t.Skip("supervised df/prune workers require the rootless test runtime")
			}
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			options := maintenanceStoreOptions(t.TempDir())
			componentDir, err := componentstore.DefaultDir()
			if err != nil {
				t.Fatal(err)
			}
			if test.args[1] == "rm" {
				layout, descriptor, _ := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: "amd64"}, "output-error")
				if test.args[0] == "image" {
					store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
					if err != nil {
						t.Fatal(err)
					}
					_, writeErr := store.WriteLayout(context.Background(), layout, descriptor, "output-error")
					if err := errors.Join(writeErr, store.Close()); err != nil {
						t.Fatal(err)
					}
				} else {
					source, err := orasoci.New(layout)
					if err != nil {
						t.Fatal(err)
					}
					if err := localstore.Put(context.Background(), componentDir, source, descriptor, "output-error"); err != nil {
						t.Fatal(err)
					}
				}
			}
			want := errors.New("maintenance output failed")
			command := newRootCommand()
			command.SetOut(&maintenanceFailingWriter{remaining: test.writes, err: want})
			command.SetErr(&bytes.Buffer{})
			command.SetArgs(append([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs"}, test.args...))
			if err := command.Execute(); !errors.Is(err, want) {
				t.Errorf("got %v, want output error", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			lease, err := storeactivity.AcquireExclusive(ctx, buildah.ActivityRoots(options, componentDir)...)
			if err != nil {
				t.Fatalf("output failure retained activity lease: %v", err)
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			if test.args[1] == "rm" && test.args[0] == "component" {
				if _, found, err := localstore.Inspect(context.Background(), componentDir, "output-error"); err != nil || found {
					t.Fatalf("component removal did not complete: found=%t err=%v", found, err)
				}
			}
			if test.args[1] == "rm" && test.args[0] == "image" {
				if err := buildah.WithStore(options, func(backend storage.Store) error {
					if _, err := imagestore.FromStore(backend).MatchedName("output-error"); err == nil {
						t.Error("image name remains after removal")
					}
					return nil
				}); err != nil {
					t.Fatalf("reopen store after output error: %v", err)
				}
			}
		})
	}
}

func TestSystemDFPropagatesDevFullError(t *testing.T) {
	if testing.Short() {
		t.Skip("supervised df workers require the rootless test runtime")
	}
	output, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("/dev/full unavailable: %v", err)
	}
	defer func() { _ = output.Close() }()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	options := maintenanceStoreOptions(t.TempDir())
	var stderr bytes.Buffer
	if status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "system", "df"}, output, &stderr); status == 0 || !strings.Contains(stderr.String(), "no space left on device") {
		t.Fatalf("df /dev/full: status=%d stderr=%q", status, stderr.String())
	}
}
