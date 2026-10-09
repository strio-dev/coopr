package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/imagestore"
	"coopr/internal/oci"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage"
	"go.podman.io/storage"
)

func TestImageIOCommandFlags(t *testing.T) {
	for _, test := range []struct {
		name  string
		flags []string
	}{
		{"pull", []string{"policy", "platform", "authfile", "tls-verify"}},
		{"push", []string{"platform", "authfile", "tls-verify", "digestfile", "quiet"}},
		{"save", []string{"output", "format", "platform", "quiet"}},
		{"load", []string{"input", "quiet", "signature-policy"}},
		{"history", []string{"format", "quiet", "no-trunc", "platform"}},
		{"tag", nil}, {"exists", nil},
	} {
		command, _, err := newRootCommand().Find([]string{"image", test.name})
		if err != nil || command.Name() != test.name {
			t.Fatalf("missing image %s: %v", test.name, err)
		}
		for _, flag := range test.flags {
			if command.Flags().Lookup(flag) == nil {
				t.Errorf("image %s lacks --%s", test.name, flag)
			}
		}
	}
}

func TestImageIORejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{{"pull"}, {"push"}, {"tag", "source"}, {"exists"}, {"history"}, {"save"}, {"load", "unexpected"}, {"save", "--format", "unknown", "image"}, {"pull", "--platform", "invalid/platform/extra/more", "example.com/image"}} {
		var out, errs bytes.Buffer
		if code := run(append([]string{"image"}, args...), &out, &errs); code == 0 {
			t.Errorf("%v succeeded", args)
		}
		if strings.Contains(errs.String(), "permission denied") {
			t.Errorf("%v opened native storage before validation: %s", args, &errs)
		}
	}
}

func imageIOArgs(options buildah.StoreOptions, args ...string) []string {
	return append([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "image"}, args...)
}

func TestImageIOExistsTagAndArchiveRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping supervised native tag and archive operations in short mode")
	}
	ctx := context.Background()
	options := maintenanceStoreOptions(t.TempDir())
	layout, root, _ := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "image-io")
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteLayout(ctx, layout, root, "source"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	for _, test := range []struct {
		args   []string
		status int
	}{
		{[]string{"exists", "source"}, 0}, {[]string{"exists", "missing"}, 1}, {[]string{"tag", "source", "tagged"}, 0}, {[]string{"exists", "tagged"}, 0},
	} {
		out.Reset()
		errs.Reset()
		if status := run(imageIOArgs(options, test.args...), &out, &errs); status != test.status {
			t.Fatalf("%v status=%d output=%q stderr=%q", test.args, status, &out, &errs)
		}
		if out.Len() != 0 || errs.Len() != 0 {
			t.Fatalf("%v unexpected output=%q stderr=%q", test.args, &out, &errs)
		}
	}
	for _, format := range []string{"oci-archive", "docker-archive"} {
		t.Run(format, func(t *testing.T) {
			if format == "oci-archive" && testing.Short() {
				t.Skip("skipping load OCI archives in the native user namespace in short mode")
			}
			archive := filepath.Join(t.TempDir(), format+".tar")
			out.Reset()
			errs.Reset()
			if status := run(imageIOArgs(options, "save", "--quiet", "--format", format, "--output", archive, "tagged"), &out, &errs); status != 0 {
				t.Fatalf("save %s status=%d: %s", format, status, &errs)
			}
			destination := maintenanceStoreOptions(t.TempDir())
			out.Reset()
			errs.Reset()
			if status := run(imageIOArgs(destination, "load", "--quiet", "--input", archive), &out, &errs); status != 0 {
				t.Fatalf("load %s status=%d: %s", format, status, &errs)
			}
			err := buildah.WithStore(destination, func(backend storage.Store) error {
				if _, err := oci.StoredImageName(backend, "tagged"); err != nil {
					return fmt.Errorf("archive did not restore local name tagged: %w", err)
				}
				images, err := backend.Images()
				if err != nil {
					return err
				}
				for _, image := range images {
					if image.ID == root.Digest.Encoded() {
						continue
					}
					_, _, selections, err := oci.StoredImageSelections(ctx, backend, image.ID)
					if err != nil {
						return err
					}
					for _, selected := range selections {
						if format == "oci-archive" && selected.Manifest.Digest != root.Digest {
							t.Errorf("OCI archive changed manifest %s to %s", root.Digest, selected.Manifest.Digest)
						}
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestImageIOPullPushWithNativeRegistry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping supervised native registry transfer in short mode")
	}
	ctx := context.Background()
	options := maintenanceStoreOptions(t.TempDir())
	layout, root, _ := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "registry")
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteLayout(ctx, layout, root, "source"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	reference := strings.TrimPrefix(server.URL, "http://") + "/coopr/image-io:test"
	digestFile := filepath.Join(t.TempDir(), "digest")
	var out, errs bytes.Buffer
	if status := run(imageIOArgs(options, "push", "--tls-verify=false", "--digestfile", digestFile, "source", reference), &out, &errs); status != 0 {
		t.Fatalf("push status=%d: %s", status, &errs)
	}
	digestData, err := os.ReadFile(digestFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(digestData) != root.Digest.String() {
		t.Fatalf("pushed digest %q want %s", digestData, root.Digest)
	}
	destination := maintenanceStoreOptions(t.TempDir())
	out.Reset()
	errs.Reset()
	if status := run(imageIOArgs(destination, "pull", "--tls-verify=false", reference), &out, &errs); status != 0 {
		t.Fatalf("pull status=%d: %s", status, &errs)
	}
	err = buildah.WithStore(destination, func(backend storage.Store) error {
		selected, err := oci.ResolveStoredImage(ctx, backend, reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
		if err == nil && selected.Selected.Digest != root.Digest {
			t.Errorf("pulled %s want %s", selected.Selected.Digest, root.Digest)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestImageIOHistoryUsesNativeConfigAndSizes(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	history := []v1.History{{CreatedBy: "ENV FIRST=1", EmptyLayer: true}, {CreatedBy: "LABEL SECOND=2", Comment: "exact config", EmptyLayer: true}}
	configData, err := json.Marshal(v1.Image{Platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, RootFS: v1.RootFS{Type: "layers"}, History: history})
	if err != nil {
		t.Fatal(err)
	}
	config := oci.Descriptor(v1.MediaTypeImageConfig, configData)
	manifestData, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config})
	if err != nil {
		t.Fatal(err)
	}
	root := oci.Descriptor(v1.MediaTypeImageManifest, manifestData)
	err = buildah.WithStore(options, func(backend storage.Store) error {
		_, err := backend.CreateImage(config.Digest.Encoded(), []string{"localhost/history:latest"}, "", "", &storage.ImageOptions{BigData: []storage.ImageBigDataOption{
			{Key: config.Digest.String(), Data: configData, Digest: config.Digest},
			{Key: storage.ImageDigestBigDataKey, Data: manifestData, Digest: root.Digest},
			{Key: storage.ImageDigestManifestBigDataNamePrefix + "-" + root.Digest.String(), Data: manifestData, Digest: root.Digest},
		}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if status := run(imageIOArgs(options, "history", "--format", "json", "history"), &out, &errs); status != 0 {
		t.Fatalf("history status=%d: %s", status, &errs)
	}
	var actual []libimage.ImageHistory
	if err := json.Unmarshal(out.Bytes(), &actual); err != nil {
		t.Fatal(err)
	}
	if len(actual) != 2 || actual[0].CreatedBy != history[1].CreatedBy || actual[0].Comment != history[1].Comment || actual[0].Size != 0 || actual[1].CreatedBy != history[0].CreatedBy {
		t.Fatalf("history=%+v", actual)
	}
}

func TestImageIOLoadRestoresExactMultiPlatformIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping native indexed archive loading in short mode")
	}
	ctx := context.Background()
	options := maintenanceStoreOptions(t.TempDir())
	native := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	foreign := v1.Platform{OS: "linux", Architecture: "arm64"}
	if native.Architecture == "arm64" {
		foreign.Architecture = "amd64"
	}
	layoutA, rootA, _ := maintenanceImageLayout(t, native, "native")
	layoutB, rootB, _ := maintenanceImageLayout(t, foreign, "foreign")
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	indexLayout := filepath.Join(t.TempDir(), "index")
	root, _, err := oci.AssembleImageIndex(ctx, indexLayout, []oci.ImageVariant{{Layout: layoutA, Manifest: rootA, Platform: native}, {Layout: layoutB, Manifest: rootB, Platform: foreign}}, "oci")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.WriteIndexLayout(ctx, indexLayout, root, "multi")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "multi.tar")
	var out, errs bytes.Buffer
	if status := run(imageIOArgs(options, "save", "--format", "oci-archive", "--output", archive, "multi"), &out, &errs); status != 0 {
		t.Fatalf("save index status=%d: %s", status, &errs)
	}
	destination := maintenanceStoreOptions(t.TempDir())
	out.Reset()
	errs.Reset()
	if status := run(imageIOArgs(destination, "load", "--input", archive), &out, &errs); status != 0 {
		t.Fatalf("load index status=%d: %s", status, &errs)
	}
	err = buildah.WithStore(destination, func(backend storage.Store) error {
		loaded, _, selections, err := oci.StoredImageSelections(ctx, backend, root.Digest.String())
		if err != nil {
			return err
		}
		if loaded.Digest != root.Digest || len(selections) != 2 {
			t.Fatalf("loaded root=%s platforms=%d want %s and 2", loaded.Digest, len(selections), root.Digest)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestImageIOArchiveStandardStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping native OCI archive loading in short mode")
	}
	ctx := context.Background()
	options := maintenanceStoreOptions(t.TempDir())
	layout, root, _ := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "streams")
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteLayout(ctx, layout, root, "streams"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var archive, errs bytes.Buffer
	if status := run(imageIOArgs(options, "save", "--quiet", "--format", "oci-archive", "streams"), &archive, &errs); status != 0 {
		t.Fatalf("stdout save status=%d: %s", status, &errs)
	}
	destination := maintenanceStoreOptions(t.TempDir())
	command := newRootCommand()
	command.SetArgs(imageIOArgs(destination, "load", "--quiet"))
	command.SetIn(bytes.NewReader(archive.Bytes()))
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&errs)
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatalf("stdin load: %v stderr=%s", err, &errs)
	}
	if archive.Len() == 0 || !strings.HasPrefix(out.String(), "Loaded image:") {
		t.Fatalf("archive bytes=%d load output=%q", archive.Len(), &out)
	}
	err = buildah.WithStore(destination, func(backend storage.Store) error {
		selected, err := oci.ResolveStoredImage(ctx, backend, root.Digest.String(), v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
		if err == nil && selected.Selected.Digest != root.Digest {
			t.Errorf("stream archive changed exact manifest")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestImageIOLoadForeignOnlyIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping native archive loading in short mode")
	}
	ctx := context.Background()
	foreign := v1.Platform{OS: "linux", Architecture: "arm64"}
	if runtime.GOARCH == "arm64" {
		foreign.Architecture = "amd64"
	}
	layout, child, _ := maintenanceImageLayout(t, foreign, "foreign-only")
	indexLayout := filepath.Join(t.TempDir(), "index")
	root, _, err := oci.AssembleImageIndex(ctx, indexLayout, []oci.ImageVariant{{Layout: layout, Manifest: child, Platform: foreign}}, "oci")
	if err != nil {
		t.Fatal(err)
	}
	source := maintenanceStoreOptions(t.TempDir())
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(source))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteIndexLayout(ctx, indexLayout, root, "foreign-only"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "foreign-only.tar")
	var out, errs bytes.Buffer
	if status := run(imageIOArgs(source, "save", "--format", "oci-archive", "--output", archive, "foreign-only"), &out, &errs); status != 0 {
		t.Fatalf("save foreign-only status=%d: %s", status, &errs)
	}
	destination := maintenanceStoreOptions(t.TempDir())
	out.Reset()
	errs.Reset()
	if status := run(imageIOArgs(destination, "load", "--input", archive), &out, &errs); status != 0 {
		t.Fatalf("load foreign-only status=%d: %s", status, &errs)
	}
	err = buildah.WithStore(destination, func(backend storage.Store) error {
		loaded, _, selections, err := oci.StoredImageSelections(ctx, backend, "foreign-only")
		if err != nil {
			return err
		}
		if loaded.Digest != root.Digest || len(selections) != 1 {
			t.Fatalf("foreign-only root=%s selections=%d", loaded.Digest, len(selections))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestImageIOExistsLocalStatus(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	err := buildah.WithStore(options, func(backend storage.Store) error {
		_, err := backend.CreateImage("", []string{"localhost/exists-local:latest"}, "", "", nil)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		status int
	}{{"exists-local", 0}, {"missing", 1}} {
		var out, errs bytes.Buffer
		if status := run(imageIOArgs(options, "exists", test.name), &out, &errs); status != test.status || out.Len() != 0 || errs.Len() != 0 {
			t.Fatalf("exists %s status=%d out=%q err=%q", test.name, status, &out, &errs)
		}
	}
	for _, args := range [][]string{{"image", "exists", "image"}, {"manifest", "exists", "list"}} {
		var out, errs bytes.Buffer
		failure := errors.New("namespace setup failed")
		if status := runContextWithStorageNamespace(context.Background(), args, &out, &errs, func() error { return failure }); status != 125 || !strings.Contains(errs.String(), failure.Error()) {
			t.Fatalf("%v operational failure status=%d stderr=%s", args, status, &errs)
		}
	}
}

func TestImageIODockerSaveDigestAndForeignSinglePlatform(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping native archive operations in short mode")
	}
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint(foreign), func(t *testing.T) {
			ctx := context.Background()
			platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
			if foreign {
				platform.Architecture = "arm64"
				if runtime.GOARCH == "arm64" {
					platform.Architecture = "amd64"
				}
			}
			layout, root, id := maintenanceImageLayout(t, platform, "docker-save-selector")
			source := maintenanceStoreOptions(t.TempDir())
			store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(source))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.WriteLayout(ctx, layout, root, "archive-source"); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			for _, selector := range []string{root.Digest.String(), "archive-source"} {
				archive := filepath.Join(t.TempDir(), "image.tar")
				var out, errs bytes.Buffer
				if status := run(imageIOArgs(source, "save", "--quiet", "--output", archive, selector), &out, &errs); status != 0 {
					t.Fatalf("Docker save %s %s status=%d: %s", selector, platform.Architecture, status, &errs)
				}
				destination := maintenanceStoreOptions(t.TempDir())
				out.Reset()
				errs.Reset()
				if status := run(imageIOArgs(destination, "load", "--quiet", "--input", archive), &out, &errs); status != 0 {
					t.Fatalf("Docker load status=%d: %s", status, &errs)
				}
				err = buildah.WithStore(destination, func(backend storage.Store) error {
					selected, err := oci.ResolveStoredImage(ctx, backend, id.Encoded(), platform)
					if err != nil {
						return err
					}
					if selected.Config.Digest != id || selected.Platform.Architecture != platform.Architecture {
						t.Fatalf("Docker archive selected config=%s platform=%+v", selected.Config.Digest, selected.Platform)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestImageIODockerSaveSharedConfigSelections(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping native archive operations in short mode")
	}
	ctx := context.Background()
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	layout, first, id := maintenanceImageLayout(t, platform, "shared-config")
	source := maintenanceStoreOptions(t.TempDir())
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(source))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteLayout(ctx, layout, first, "shared"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var second v1.Descriptor
	var secondData []byte
	err = buildah.WithStore(source, func(backend storage.Store) error {
		original, err := backend.ImageBigData(id.Encoded(), storage.ImageDigestBigDataKey)
		if err != nil {
			return err
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(original, &manifest); err != nil {
			return err
		}
		manifest.Annotations = map[string]string{"example.com/variant": "second"}
		secondData, err = json.Marshal(manifest)
		if err != nil {
			return err
		}
		second = oci.Descriptor(v1.MediaTypeImageManifest, secondData)
		if err := backend.SetImageBigData(id.Encoded(), storage.ImageDigestManifestBigDataNamePrefix+"-"+second.Digest.String(), secondData, func(data []byte) (digest.Digest, error) { return digest.FromBytes(data), nil }); err != nil {
			return err
		}
		return backend.SetImageBigData(id.Encoded(), storage.ImageDigestBigDataKey, secondData, func(data []byte) (digest.Digest, error) { return digest.FromBytes(data), nil })
	})
	if err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	archive := filepath.Join(t.TempDir(), "shared.tar")
	if status := run(imageIOArgs(source, "save", "--quiet", "--output", archive, first.Digest.String(), second.Digest.String()), &out, &errs); status != 0 {
		t.Fatalf("shared-config save status=%d: %s", status, &errs)
	}
	err = buildah.WithStore(source, func(backend storage.Store) error {
		actual, err := backend.ImageBigData(id.Encoded(), storage.ImageDigestBigDataKey)
		if err != nil {
			return err
		}
		if !bytes.Equal(actual, secondData) {
			t.Fatal("saving an older shared-config manifest changed the native default")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	destination := maintenanceStoreOptions(t.TempDir())
	out.Reset()
	errs.Reset()
	if status := run(imageIOArgs(destination, "load", "--quiet", "--input", archive), &out, &errs); status != 0 {
		t.Fatalf("shared-config load status=%d: %s", status, &errs)
	}
	err = buildah.WithStore(destination, func(backend storage.Store) error {
		images, err := backend.Images()
		if err != nil {
			return err
		}
		if len(images) != 1 || images[0].ID != id.Encoded() {
			t.Fatalf("native Docker archive should deduplicate shared configs: %+v", images)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
