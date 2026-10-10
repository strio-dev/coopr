package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"coopr/internal/buildah"
	"coopr/internal/componentstore"
	"coopr/internal/imagestore"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	"coopr/internal/storeactivity"
	"github.com/containerd/platforms"
	"github.com/google/go-containerregistry/pkg/registry"
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
		for _, flag := range []string{"dry-run", "all", "force"} {
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
	var inspectedRows []libimage.ImageData
	if err := json.Unmarshal(stdout.Bytes(), &inspectedRows); err != nil {
		t.Fatal(err)
	}
	if len(inspectedRows) != 1 {
		t.Fatalf("expected one inspection: %s", &stdout)
	}
	inspected := inspectedRows[0]
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
			var inspectedRows []libimage.ImageData
			if err := json.Unmarshal(stdout.Bytes(), &inspectedRows); err != nil {
				t.Fatal(err)
			}
			if len(inspectedRows) != 1 {
				t.Fatalf("expected one inspection: %s", &stdout)
			}
			inspected := inspectedRows[0]
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
	var inspectedRows []libimage.ImageData
	if err := json.Unmarshal(stdout.Bytes(), &inspectedRows); err != nil {
		t.Fatal(err)
	}
	if len(inspectedRows) != 1 {
		t.Fatalf("expected one inspection: %s", &stdout)
	}
	inspected := inspectedRows[0]
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
		var inspectedRows []libimage.ImageData
		if err := json.Unmarshal(stdout.Bytes(), &inspectedRows); err != nil {
			t.Fatal(err)
		}
		if len(inspectedRows) != 1 {
			t.Fatalf("expected one inspection: %s", &stdout)
		}
		inspected := inspectedRows[0]
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
	if _, err := store.Tag(imageID.Encoded(), "retained"); err == nil {
		t.Fatal("rm retained the final untagged underlying image")
	}
	if !strings.Contains(stdout.String(), "Untagged: localhost/native-only:latest") || !strings.Contains(stdout.String(), "Deleted: "+imageID.Encoded()) {
		t.Fatalf("missing native removal reports: %s", &stdout)
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

	var listing, listingErr bytes.Buffer
	if status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "images"}, &listing, &listingErr); status != 0 {
		t.Fatalf("listing: status=%d stderr=%s", status, &listingErr)
	}
	if !strings.Contains(listing.String(), "localhost/multi") || !strings.Contains(listing.String(), "localhost/foreign-only") {
		t.Fatalf("index/foreign images absent: %s", &listing)
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
	if len(rows) != 5 || strings.Count(stdout.String(), "<none>") != 2 {
		t.Fatalf("images should show named index, two digest-pinned instances and foreign tag: %q", stdout.String())
	}
	for _, row := range rows[1:] {
		if !strings.HasPrefix(row, "localhost/") {
			t.Fatalf("images showed synthetic lookup alias instead of native name: %q", row)
		}
		fields := strings.Fields(row)
		if len(fields) < 5 || len(fields[2]) != 12 {
			t.Fatalf("invalid native image report row: %q", row)
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
	var inspectedRows []libimage.ImageData
	if err := json.Unmarshal(stdout.Bytes(), &inspectedRows); err != nil {
		t.Fatal(err)
	}
	selectedManifest := amdManifest
	if platforms.DefaultSpec().Architecture == "arm64" {
		selectedManifest = armManifest
	}
	if len(inspectedRows) != 1 || inspectedRows[0].Digest != selectedManifest.Digest || inspectedRows[0].Architecture != platforms.DefaultSpec().Architecture {
		t.Fatalf("image inspection did not select host platform: %s", &stdout)
	}
	stdout.Reset()
	stderr.Reset()
	if status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "manifest", "inspect", "multi"}, &stdout, &stderr); status != 0 {
		t.Fatalf("manifest inspection status=%d stderr=%s", status, &stderr)
	}
	var inspectedIndex v1.Index
	if err := json.Unmarshal(stdout.Bytes(), &inspectedIndex); err != nil || len(inspectedIndex.Manifests) != 2 {
		t.Fatalf("manifest inspection lost raw index: %s, %v", &stdout, err)
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
	if err := json.Unmarshal(stdout.Bytes(), &inspectedRows); err != nil {
		t.Fatal(err)
	}
	if len(inspectedRows) != 1 {
		t.Fatalf("expected one inspection: %s", &stdout)
	}
	if inspectedRows[0].Digest != selectedManifest.Digest || inspectedRows[0].Architecture != platforms.DefaultSpec().Architecture {
		t.Fatalf("retained index inspection lost selected child: %s", &stdout)
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

func TestImageListingWaitsForNativeMaintenance(t *testing.T) {
	options := buildah.StoreOptions{GraphRoot: t.TempDir(), RunRoot: t.TempDir(), ImageStore: t.TempDir(), GraphDriverName: "vfs"}
	for _, root := range buildah.ActivityRoots(options, "") {
		exclusive, err := storeactivity.AcquireExclusive(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		command := newImagesCommand()
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		command.SetContext(context.WithValue(ctx, storageSelectionKey{}, storageSelection{store: options}))
		readErr := command.RunE(command, nil)
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
		{"image prune count", []string{"image", "prune", "--dry-run"}, 0},
		{"system prune count", []string{"system", "prune", "--dry-run"}, 0},
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

func TestImagesReportIncludesDanglingAndHidesPrivateRecords(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	var visibleID, danglingID string
	if err := buildah.WithStore(options, func(backend storage.Store) error {
		for _, fixture := range []struct {
			name, label  string
			privateAlias bool
		}{
			{"visible", "visible", false}, {"", "dangling", false}, {"", "digest-only", false},
			{"coopr.internal/cache:private", "cache", false}, {"", "selected", true},
		} {
			layout, descriptor, id := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: "amd64"}, fixture.label)
			if _, err := imagestore.FromStore(backend).WriteLayout(context.Background(), layout, descriptor, fixture.name); err != nil {
				return err
			}
			if fixture.label == "digest-only" {
				if err := backend.AddNames(id.Encoded(), []string{"registry.example/team/pinned@" + descriptor.Digest.String()}); err != nil {
					return err
				}
			}
			if fixture.privateAlias {
				if _, err := oci.SelectedStoredImage(context.Background(), backend, id.Encoded(), descriptor.Digest); err != nil {
					return err
				}
				if _, err := backend.DeleteImage(id.Encoded(), true); err != nil {
					return err
				}
			}
			if fixture.label == "visible" {
				visibleID = id.Encoded()
			}
			if fixture.label == "dangling" {
				danglingID = id.Encoded()
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "images"}, &stdout, &stderr); status != 0 {
		t.Fatalf("images status=%d stderr=%s", status, &stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 4 || !strings.Contains(stdout.String(), "registry.example/team/pinned") || strings.Join(strings.Fields(lines[0]), " ") != "REPOSITORY TAG IMAGE ID CREATED SIZE" || !strings.Contains(stdout.String(), visibleID[:12]) || !strings.Contains(stdout.String(), danglingID[:12]) || !strings.Contains(stdout.String(), "<none>") || strings.Contains(stdout.String(), "coopr.internal") {
		t.Fatalf("unexpected image table: %s", &stdout)
	}
}

func TestImageRemoveRetainsOtherNamesAndCacheIdentity(t *testing.T) {
	for _, remainingName := range []string{"localhost/other:latest", "coopr.internal/cache:retained"} {
		t.Run(remainingName, func(t *testing.T) {
			options := maintenanceStoreOptions(t.TempDir())
			var imageID string
			if err := buildah.WithStore(options, func(backend storage.Store) error {
				layout, descriptor, id := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: "amd64"}, "shared")
				imageID = id.Encoded()
				if _, err := imagestore.FromStore(backend).WriteLayout(context.Background(), layout, descriptor, "shared"); err != nil {
					return err
				}
				return backend.AddNames(imageID, []string{remainingName})
			}); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image", "rm", "shared"}, &stdout, &stderr); status != 0 {
				t.Fatalf("rm status=%d stderr=%s", status, &stderr)
			}
			if stdout.String() != "Untagged: localhost/shared:latest\n" {
				t.Fatalf("untag claimed deletion: %s", &stdout)
			}
			if err := buildah.WithStore(options, func(backend storage.Store) error {
				image, err := backend.Image(remainingName)
				if err != nil {
					return err
				}
				if image.ID != imageID {
					t.Fatalf("remaining name identity changed")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestImageRemoveIndexPreservesInstances(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	ctx := context.Background()
	var children []string
	if err := buildah.WithStore(options, func(backend storage.Store) error {
		var variants []oci.ImageVariant
		for _, arch := range []string{"amd64", "arm64"} {
			platform := v1.Platform{OS: "linux", Architecture: arch}
			layout, descriptor, id := maintenanceImageLayout(t, platform, arch)
			children = append(children, id.Encoded())
			variants = append(variants, oci.ImageVariant{Layout: layout, Manifest: descriptor, Platform: platform})
		}
		layout := filepath.Join(t.TempDir(), "index")
		index, _, err := oci.AssembleImageIndex(ctx, layout, variants, "oci")
		if err != nil {
			return err
		}
		_, err = imagestore.FromStore(backend).WriteIndexLayout(ctx, layout, index, "multi")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image", "rm", "multi"}, &stdout, &stderr); status != 0 {
		t.Fatalf("index rm status=%d stderr=%s", status, &stderr)
	}
	if !strings.Contains(stdout.String(), "Untagged: localhost/multi:latest") || !strings.Contains(stdout.String(), "Deleted:") {
		t.Fatalf("missing index reports: %s", &stdout)
	}
	if err := buildah.WithStore(options, func(backend storage.Store) error {
		for _, id := range children {
			if _, err := backend.Image(id); err != nil {
				return err
			}
		}
		if _, err := backend.Image("localhost/multi:latest"); !errors.Is(err, storage.ErrImageUnknown) {
			t.Fatalf("index name remains: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestImageRemoveRefusesContainerUsedImage(t *testing.T) {
	if testing.Short() {
		t.Skip("native container creation requires the rootless test runtime")
	}
	options := maintenanceStoreOptions(t.TempDir())
	var imageID string
	if err := buildah.WithStore(options, func(backend storage.Store) error {
		layout, descriptor, id := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: "amd64"}, "in-use")
		imageID = id.Encoded()
		if _, err := imagestore.FromStore(backend).WriteLayout(context.Background(), layout, descriptor, "in-use"); err != nil {
			return err
		}
		_, err := backend.CreateContainer("", nil, imageID, "", "", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image", "rm", "in-use"}, &stdout, &stderr); status != 2 {
		t.Fatalf("in-use removal status=%d stdout=%q stderr=%q", status, &stdout, &stderr)
	}
	if strings.Contains(stdout.String(), "Deleted:") {
		t.Fatalf("reported deletion of in-use image: %s", &stdout)
	}
	if err := buildah.WithStore(options, func(backend storage.Store) error {
		image, err := backend.Image("localhost/in-use:latest")
		if err == nil && image.ID != imageID {
			t.Fatal("in-use image identity changed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestImageRemovePulledInstanceWithIndexOrigin(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	ctx := context.Background()
	var imageID string
	if err := buildah.WithStore(options, func(backend storage.Store) error {
		platform := v1.Platform{OS: "linux", Architecture: "amd64"}
		layout, descriptor, id := maintenanceImageLayout(t, platform, "pulled-instance")
		imageID = id.Encoded()
		if _, err := imagestore.FromStore(backend).WriteLayout(ctx, layout, descriptor, "pulled-instance"); err != nil {
			return err
		}
		_, foreign, _ := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: "arm64"}, "absent-instance")
		root, data, err := oci.ImageIndexDescriptor([]oci.ImageVariant{{Manifest: descriptor, Platform: platform}, {Manifest: foreign, Platform: v1.Platform{OS: "linux", Architecture: "arm64"}}}, "oci")
		if err != nil {
			return err
		}
		if err := backend.SetImageBigData(imageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+root.Digest.String(), data, func(data []byte) (digest.Digest, error) { return digest.FromBytes(data), nil }); err != nil {
			return err
		}
		return oci.RecordStoredOrigin(ctx, backend, "localhost/pulled-instance:latest", oci.StoredSelection{Root: root, Manifest: descriptor, ImageID: imageID})
	}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image", "rm", "pulled-instance"}, &stdout, &stderr); status != 0 {
		t.Fatalf("pulled-instance rm status=%d stderr=%s", status, &stderr)
	}
	if !strings.Contains(stdout.String(), "Deleted: "+imageID) {
		t.Fatalf("instance not deleted: %s", &stdout)
	}
}

func TestImageRemoveDoesNotRequireHealthyConfiguration(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	ctx := context.Background()
	var imageID string
	if err := buildah.WithStore(options, func(backend storage.Store) error {
		layout, descriptor, id := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: "amd64"}, "bad-config")
		imageID = id.Encoded()
		if _, err := imagestore.FromStore(backend).WriteLayout(ctx, layout, descriptor, "bad-config"); err != nil {
			return err
		}
		return backend.SetImageBigData(imageID, id.String(), []byte("broken config"), nil)
	}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image", "rm", "bad-config"}, &stdout, &stderr); status != 0 {
		t.Fatalf("damaged config blocked rm: status=%d stderr=%s", status, &stderr)
	}
	if !strings.Contains(stdout.String(), "Deleted: "+imageID) {
		t.Fatalf("missing native deletion: %s", &stdout)
	}
}

func TestImageRemoveBatchesRepeatedNamesAndIDs(t *testing.T) {
	for _, byID := range []bool{false, true} {
		t.Run(map[bool]string{false: "repeated-name", true: "id-and-name"}[byID], func(t *testing.T) {
			options := maintenanceStoreOptions(t.TempDir())
			layout, descriptor, id := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: "amd64"}, "duplicate")
			if err := buildah.WithStore(options, func(backend storage.Store) error {
				_, err := imagestore.FromStore(backend).WriteLayout(context.Background(), layout, descriptor, "duplicate")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			names := []string{"duplicate", "duplicate"}
			if byID {
				names[0] = id.Encoded()
			}
			var stdout, stderr bytes.Buffer
			args := append([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image", "rm"}, names...)
			if status := run(args, &stdout, &stderr); status != 0 {
				t.Fatalf("duplicate removal status=%d stderr=%s", status, &stderr)
			}
			if strings.Count(stdout.String(), "Deleted: "+id.Encoded()) != 1 {
				t.Fatalf("duplicate deletion reports: %s", &stdout)
			}
		})
	}
}

func TestImageRemoveMissingManifestUsesNativeRecovery(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	id := digest.FromString("missing-manifest-rm").Encoded()
	if err := buildah.WithStore(options, func(backend storage.Store) error {
		_, err := backend.CreateImage(id, []string{"localhost/broken:latest"}, "", "", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image", "rm", "broken"}, &stdout, &stderr); status != 0 {
		t.Fatalf("missing manifest blocked native removal: status=%d stderr=%s", status, &stderr)
	}
	if !strings.Contains(stdout.String(), "Deleted: "+id) {
		t.Fatalf("missing native deletion: %s", &stdout)
	}
}

func TestPruneConfirmationPreventsMutation(t *testing.T) {
	for _, system := range []bool{false, true} {
		for _, input := range []string{"n\n", "n", "\n", ""} {
			t.Run(fmt.Sprintf("system=%t/input=%q", system, input), func(t *testing.T) {
				command := newPruneCommand(system)
				var out bytes.Buffer
				command.SetIn(strings.NewReader(input))
				command.SetOut(&out)
				command.SetContext(context.WithValue(context.Background(), storageSelectionKey{}, storageSelection{store: maintenanceStoreOptions(t.TempDir())}))
				// A declined prompt must return before touching storage.
				err := command.RunE(command, nil)
				if input == "" {
					if !errors.Is(err, io.EOF) {
						t.Fatalf("expected EOF without mutation, got %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(out.String(), "[y/N]") {
					t.Fatalf("missing confirmation: %q", out.String())
				}
				if system && !strings.Contains(out.String(), "component") {
					t.Fatalf("system warning omits component content: %q", out.String())
				}
			})
		}
	}
}

func TestPruneConfirmationErrors(t *testing.T) {
	want := errors.New("confirmation stream failed")
	for _, mode := range []string{"read", "partial-read", "write"} {
		command := newPruneCommand(false)
		command.SetContext(context.Background())
		command.SetIn(iotest.ErrReader(want))
		command.SetOut(&bytes.Buffer{})
		if mode == "partial-read" {
			command.SetIn(io.MultiReader(strings.NewReader("y"), iotest.ErrReader(want)))
		}
		if mode == "write" {
			command.SetOut(&maintenanceFailingWriter{err: want})
		}
		if err := command.RunE(command, nil); !errors.Is(err, want) {
			t.Fatalf("confirmation stream error: got %v", err)
		}
	}
}

func TestImageInspectIndexUnavailableHost(t *testing.T) {
	for _, missingMember := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing-member=%t", missingMember), func(t *testing.T) {
			options := maintenanceStoreOptions(t.TempDir())
			platform := platforms.DefaultSpec()
			if !missingMember {
				platform.Architecture = "arm64"
				if platform.Architecture == platforms.DefaultSpec().Architecture {
					platform.Architecture = "amd64"
				}
			}
			layout, descriptor, imageID := maintenanceImageLayout(t, platform, "unavailable")
			indexLayout := filepath.Join(t.TempDir(), "index")
			index, _, err := oci.AssembleImageIndex(context.Background(), indexLayout, []oci.ImageVariant{{Layout: layout, Manifest: descriptor, Platform: platform}}, "oci")
			if err != nil {
				t.Fatal(err)
			}
			if err := buildah.WithStore(options, func(backend storage.Store) error {
				if _, err := imagestore.FromStore(backend).WriteIndexLayout(context.Background(), indexLayout, index, "unavailable"); err != nil {
					return err
				}
				if missingMember {
					_, err := backend.DeleteImage(imageID.Encoded(), true)
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image", "inspect", "unavailable"}, &stdout, &stderr)
			if status == 0 || stdout.Len() != 0 {
				t.Fatalf("unavailable index unexpectedly inspected: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
			}
			expected := "no image found"
			if missingMember {
				expected = "image not known"
			}
			if !strings.Contains(stderr.String(), expected) {
				t.Fatalf("missing selection error: %s", &stderr)
			}
			if !missingMember {
				// A foreign single image is still inspectable; only index selection requires host matching.
				status = run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image", "inspect", imageID.Encoded()}, &stdout, &stderr)
				if status != 0 {
					t.Fatalf("foreign single inspection failed: %s", &stderr)
				}
			}
		})
	}
}

func TestPruneConfirmationExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("supervised pruning requires rootless runtime")
	}
	for _, system := range []bool{false, true} {
		for _, mode := range []string{"yes", "yes-eof", "force", "dry-run", "decline", "decline-eof", "eof"} {
			t.Run(fmt.Sprintf("system=%t/%s", system, mode), func(t *testing.T) {
				t.Setenv("XDG_DATA_HOME", t.TempDir())
				options := maintenanceStoreOptions(t.TempDir())
				layout, descriptor, imageID := maintenanceImageLayout(t, platforms.DefaultSpec(), "confirmed")
				if err := buildah.WithStore(options, func(backend storage.Store) error {
					_, err := imagestore.FromStore(backend).WriteLayout(context.Background(), layout, descriptor, "confirmed")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				command := newPruneCommand(system)
				var stdout bytes.Buffer
				command.SetOut(&stdout)
				command.SetErr(&bytes.Buffer{})
				command.SetIn(strings.NewReader("Y\n"))
				command.SetContext(context.WithValue(context.Background(), storageSelectionKey{}, storageSelection{store: options}))
				for _, flag := range []string{"all", map[string]string{"yes": "all", "yes-eof": "all", "force": "force", "dry-run": "dry-run", "decline": "all", "decline-eof": "all", "eof": "all"}[mode]} {
					if err := command.Flags().Set(flag, "true"); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "decline" {
					command.SetIn(strings.NewReader("n\n"))
				}
				if mode == "yes-eof" {
					command.SetIn(strings.NewReader("y"))
				}
				if mode == "decline-eof" {
					command.SetIn(strings.NewReader("n"))
				}
				if mode == "eof" {
					command.SetIn(strings.NewReader(""))
				}
				if mode == "force" || mode == "dry-run" {
					command.SetIn(iotest.ErrReader(errors.New("must not read input")))
				}
				if err := command.RunE(command, nil); (mode == "eof" && !errors.Is(err, io.EOF)) || (mode != "eof" && err != nil) {
					t.Fatal(err)
				}
				if strings.Contains(stdout.String(), "[y/N]") != (mode == "yes" || mode == "yes-eof" || mode == "decline" || mode == "decline-eof" || mode == "eof") {
					t.Fatalf("unexpected confirmation output: %s", &stdout)
				}
				if err := buildah.WithStore(options, func(backend storage.Store) error {
					_, err := backend.Image(imageID.Encoded())
					if (mode == "dry-run" || mode == "decline" || mode == "decline-eof" || mode == "eof") && err != nil {
						t.Fatalf("dry run deleted image: %v", err)
					}
					if (mode == "yes" || mode == "yes-eof" || mode == "force") && !errors.Is(err, storage.ErrImageUnknown) {
						t.Fatalf("accepted prune did not delete image: %v", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestPruneConfirmationCanceled(t *testing.T) {
	command := newPruneCommand(false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	command.SetContext(ctx)
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetIn(strings.NewReader("y\n"))
	if err := command.RunE(command, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled prompt: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("canceled prompt wrote output: %s", &stdout)
	}
}

func TestImageInspectIndexUsesNativeSelection(t *testing.T) {
	for _, malformedSize := range []bool{false, true} {
		t.Run(fmt.Sprintf("malformed-size=%t", malformedSize), func(t *testing.T) {
			options := maintenanceStoreOptions(t.TempDir())
			platform := platforms.DefaultSpec()
			var expectedID string
			if err := buildah.WithStore(options, func(backend storage.Store) error {
				var children []v1.Descriptor
				for _, name := range []string{"first", "second"} {
					layout, descriptor, _ := maintenanceImageLayout(t, platform, name)
					if _, err := imagestore.FromStore(backend).WriteLayout(context.Background(), layout, descriptor, name); err != nil {
						return err
					}
					descriptor.Platform = &platform
					children = append(children, descriptor)
				}
				native, err := libimage.RuntimeFromStore(backend, nil)
				if err != nil {
					return err
				}
				list, err := native.CreateManifestList("choices")
				if err != nil {
					return err
				}
				for _, child := range children {
					ref, err := manifestMemberReference(context.Background(), backend, child.Digest.String())
					if err != nil {
						return err
					}
					if _, err := list.Add(context.Background(), ref, nil); err != nil {
						return err
					}
				}
				if malformedSize {
					children[0].Size++
					data, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: children})
					if err != nil {
						return err
					}
					if err := backend.SetImageBigData(list.ID(), storage.ImageDigestBigDataKey, data, func(data []byte) (digest.Digest, error) { return digest.FromBytes(data), nil }); err != nil {
						return err
					}
				}
				image, _, err := native.LookupImage("choices", nil)
				if err == nil {
					expectedID = image.ID()
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image", "inspect", "choices"}, &stdout, &stderr)
			if malformedSize {
				if status == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "differs from its index") {
					t.Fatalf("mismatched selected descriptor accepted: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
				}
				return
			}
			var inspected []libimage.ImageData
			if err := json.Unmarshal(stdout.Bytes(), &inspected); status != 0 || err != nil || len(inspected) != 1 || inspected[0].ID != expectedID {
				t.Fatalf("selection differs from native libimage: status=%d stdout=%q stderr=%q err=%v want ID=%s", status, stdout.String(), stderr.String(), err, expectedID)
			}
		})
	}
}

func TestImageInspectEncryptedIndexUsesPlaintextNativeImage(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	platform := platforms.DefaultSpec()
	layout, plaintext, imageID := maintenanceImageLayout(t, platform, "decrypted")
	if err := buildah.WithStore(options, func(backend storage.Store) error {
		if _, err := imagestore.FromStore(backend).WriteLayout(context.Background(), layout, plaintext, "plaintext"); err != nil {
			return err
		}
		raw, err := os.ReadFile(filepath.Join(layout, "blobs", "sha256", plaintext.Digest.Encoded()))
		if err != nil {
			return err
		}
		var encrypted v1.Manifest
		if err := json.Unmarshal(raw, &encrypted); err != nil {
			return err
		}
		encrypted.Layers = []v1.Descriptor{{MediaType: v1.MediaTypeImageLayerGzip + "+encrypted", Digest: digest.FromString("ciphertext"), Size: 10}}
		raw, err = json.Marshal(encrypted)
		if err != nil {
			return err
		}
		original := oci.Descriptor(v1.MediaTypeImageManifest, raw)
		original.Platform = &platform
		if err := backend.SetImageBigData(imageID.Encoded(), storage.ImageDigestManifestBigDataNamePrefix+"-"+original.Digest.String(), raw, func(data []byte) (digest.Digest, error) { return digest.FromBytes(data), nil }); err != nil {
			return err
		}
		indexData, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{original}})
		if err != nil {
			return err
		}
		index := oci.Descriptor(v1.MediaTypeImageIndex, indexData)
		_, err = backend.CreateImage(index.Digest.Encoded(), []string{"localhost/encrypted-list:latest"}, "", "", &storage.ImageOptions{BigData: []storage.ImageBigDataOption{{Key: storage.ImageDigestBigDataKey, Data: indexData, Digest: index.Digest}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image", "inspect", "encrypted-list"}, &stdout, &stderr)
	var inspected []libimage.ImageData
	if err := json.Unmarshal(stdout.Bytes(), &inspected); status != 0 || err != nil || len(inspected) != 1 || inspected[0].ID != imageID.Encoded() || inspected[0].Digest != plaintext.Digest {
		t.Fatalf("encrypted origin failed native inspection: status=%d stdout=%q stderr=%q err=%v", status, stdout.String(), stderr.String(), err)
	}
	t.Run("save and push", func(t *testing.T) {
		if testing.Short() {
			t.Skip("native image export requires user namespaces")
		}
		archivePath := filepath.Join(t.TempDir(), "plaintext.tar")
		stdout.Reset()
		stderr.Reset()
		if status := run(imageIOArgs(options, "save", "--format", "oci-archive", "--output", archivePath, "encrypted-list"), &stdout, &stderr); status != 0 {
			t.Fatalf("decrypted-origin save status=%d: %s", status, &stderr)
		}
		archive, err := orasoci.NewFromTar(context.Background(), archivePath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Resolve(context.Background(), plaintext.Digest.String()); err != nil {
			t.Fatalf("decrypted save did not retain plaintext root: %v", err)
		}
		server := httptest.NewServer(registry.New())
		defer server.Close()
		remote := strings.TrimPrefix(server.URL, "http://") + "/test/decrypted:latest"
		stdout.Reset()
		stderr.Reset()
		if status := run(imageIOArgs(options, "push", "--tls-verify=false", "encrypted-list", remote), &stdout, &stderr); status != 0 {
			t.Fatalf("decrypted-origin push status=%d: %s", status, &stderr)
		}
		resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false)})
		if err != nil {
			t.Fatal(err)
		}
		raw, mediaType, err := resolver.RemoteImageManifest(context.Background(), remote)
		if err != nil || mediaType != plaintext.MediaType || digest.FromBytes(raw) != plaintext.Digest {
			t.Fatalf("decrypted push changed runnable root: type=%s digest=%s err=%v", mediaType, digest.FromBytes(raw), err)
		}
	})
}

func TestImageInspectForeignSingleWithIndexOrigin(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	platform := platforms.DefaultSpec()
	platform.Architecture = "arm64"
	if platform.Architecture == platforms.DefaultSpec().Architecture {
		platform.Architecture = "amd64"
	}
	layout, child, imageID := maintenanceImageLayout(t, platform, "foreign-origin")
	if err := buildah.WithStore(options, func(backend storage.Store) error {
		if _, err := imagestore.FromStore(backend).WriteLayout(context.Background(), layout, child, "foreign-origin"); err != nil {
			return err
		}
		indexedChild := child
		indexedChild.Platform = &platform
		data, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{indexedChild}})
		if err != nil {
			return err
		}
		root := oci.Descriptor(v1.MediaTypeImageIndex, data)
		if err := backend.SetImageBigData(imageID.Encoded(), storage.ImageDigestManifestBigDataNamePrefix+"-"+root.Digest.String(), data, func(data []byte) (digest.Digest, error) { return digest.FromBytes(data), nil }); err != nil {
			return err
		}
		return oci.RecordStoredOrigin(context.Background(), backend, "foreign-origin", oci.StoredSelection{Root: root, Manifest: child, ImageID: imageID.Encoded()})
	}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image", "inspect", "foreign-origin"}, &stdout, &stderr)
	var inspected []libimage.ImageData
	if err := json.Unmarshal(stdout.Bytes(), &inspected); status != 0 || err != nil || len(inspected) != 1 || inspected[0].Digest != child.Digest || inspected[0].Architecture != platform.Architecture {
		t.Fatalf("foreign native image incorrectly selected from remote index: status=%d stdout=%q stderr=%q err=%v", status, stdout.String(), stderr.String(), err)
	}
}

func TestComponentRemoveAcceptsLocalPrefix(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	directory, err := componentstore.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	layout, descriptor, _ := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: "amd64"}, "component-remove")
	source, err := orasoci.New(layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := localstore.Put(context.Background(), directory, source, descriptor, "remove", "retain"); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if status := run([]string{"component", "rm", "local:remove"}, &stdout, &stderr); status != 0 || stdout.String() != "remove\n" {
		t.Fatalf("rm local reference: %d %q %q", status, &stdout, &stderr)
	}
	if _, found, err := localstore.Inspect(context.Background(), directory, "retain"); err != nil || !found {
		t.Fatalf("remaining name lost: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if status := run([]string{"component", "rm", "local:" + descriptor.Digest.String()}, &stdout, &stderr); status == 0 {
		t.Fatalf("component rm accepted immutable digest: %q", &stdout)
	}
}
