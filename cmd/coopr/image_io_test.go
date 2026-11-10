package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/planner"
	"github.com/strio-dev/coopr/internal/testutil/nativeimage"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage"
	"go.podman.io/storage"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestImageIOCommandFlags(t *testing.T) {
	for _, test := range []struct {
		name  string
		flags []string
	}{
		{"pull", []string{"policy", "platform", "authfile", "tls-verify", "quiet", "all-tags"}},
		{"push", []string{"platform", "authfile", "tls-verify", "digestfile", "quiet"}},
		{"save", []string{"output", "format", "platform", "quiet", "multi-image-archive", "compress", "uncompressed"}},
		{"load", []string{"input", "quiet", "signature-policy"}},
		{"history", []string{"format", "quiet", "no-trunc", "platform", "human"}},
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

func TestImagePushNativeDestinationsWriteDigest(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping supervised native transfers in short mode")
	}
	ctx := context.Background()
	options := maintenanceStoreOptions(t.TempDir())
	layout, root, _ := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "native-push")
	maintenanceWriteLayout(t, options, layout, root, "source")
	for _, transport := range []string{"dir", "oci", "docker-archive", "oci-archive", "containers-storage"} {
		t.Run(transport, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "image@artifact")
			loaded := maintenanceStoreOptions(t.TempDir())
			destination := transport + ":" + output
			if transport == "oci" || transport == "docker-archive" {
				destination += ":exported:dev"
			}
			if transport == "containers-storage" {
				destination = "containers-storage:[vfs@" + loaded.GraphRoot + "+" + loaded.RunRoot + "]localhost/exported:dev"
			}
			digestFile := filepath.Join(t.TempDir(), "digest")
			var out, errs bytes.Buffer
			maintenanceRun(t, imageIOArgs(options, "push", "--quiet", "--digestfile", digestFile, "source", destination), &out, &errs)
			if out.Len() != 0 || errs.Len() != 0 {
				t.Fatalf("quiet push printed stdout=%q stderr=%q", &out, &errs)
			}
			data, err := os.ReadFile(digestFile)
			if err != nil {
				t.Fatal(err)
			}
			if err := digest.Digest(data).Validate(); err != nil {
				t.Fatalf("invalid pushed digest %q: %v", data, err)
			}
			if transport != "containers-storage" {
				maintenanceRun(t, imageIOArgs(loaded, "load", "--quiet", "--input", output), &out, &errs)
			}
			err = buildah.WithStore(loaded, func(backend storage.Store) error {
				selected, err := oci.ResolveStoredImage(ctx, backend, string(data), v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
				if err != nil {
					return err
				}
				var config v1.Image
				if err := json.Unmarshal(selected.ConfigData, &config); err != nil {
					return err
				}
				if config.Config.Labels["test"] != "native-push" {
					t.Fatalf("loaded output lost configuration: %+v", config.Config)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestImageIOExistsTagAndArchiveRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping supervised native tag and archive operations in short mode")
	}
	ctx := context.Background()
	options := maintenanceStoreOptions(t.TempDir())
	layout, root, _ := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "image-io")
	maintenanceWriteLayout(t, options, layout, root, "source")
	var out, errs bytes.Buffer
	for _, test := range []struct {
		args   []string
		status int
	}{
		{[]string{"exists", "source"}, 0}, {[]string{"exists", "missing"}, 1}, {[]string{"tag", "source", "tagged"}, 0}, {[]string{"exists", "tagged"}, 0},
		{[]string{"exists", "local:tagged"}, 1}, {[]string{"tag", "source", "local:tagged"}, 0},
		{[]string{"exists", "local:tagged"}, 0}, {[]string{"copy", "local:tagged", "copied", "also-copied"}, 0}, {[]string{"exists", "copied"}, 0}, {[]string{"exists", "also-copied"}, 0},
		{[]string{"tag", "source", "docker-daemon:dev"}, 0}, {[]string{"copy", "docker-daemon:dev", "daemon-name-alias"}, 0},
		{[]string{"exists", "daemon-name-alias"}, 0},
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
	pushed := filepath.Join(t.TempDir(), "pushed.oci.tar")
	out.Reset()
	errs.Reset()
	maintenanceRun(t, imageIOArgs(options, "push", "--quiet", "source", "oci-archive:"+pushed+":pushed-image"), &out, &errs)
	archivedRoot, err := oci.ArchiveRoot(pushed)
	if err != nil || archivedRoot.Annotations[v1.AnnotationRefName] != "pushed-image" {
		t.Fatalf("image archive suffix annotation = %v, %v", archivedRoot.Annotations, err)
	}
	loaded := maintenanceStoreOptions(t.TempDir())
	maintenanceRun(t, imageIOArgs(loaded, "load", "--quiet", "--input", pushed), &out, &errs)
	maintenanceRun(t, imageIOArgs(loaded, "exists", "pushed-image"), &out, &errs)
	if info, err := os.Stat(pushed); err != nil || info.Size() == 0 {
		t.Fatalf("image push archive missing or empty: %v", err)
	}
	for _, format := range []string{"oci-archive", "docker-archive", "oci-dir", "docker-dir"} {
		t.Run(format, func(t *testing.T) {
			if format == "oci-archive" && testing.Short() {
				t.Skip("skipping load OCI archives in the native user namespace in short mode")
			}
			archive := filepath.Join(t.TempDir(), format+".tar")
			out.Reset()
			errs.Reset()
			maintenanceRun(t, imageIOArgs(options, "save", "--quiet", "--format", format, "--output", archive, "tagged"), &out, &errs)
			destination := maintenanceStoreOptions(t.TempDir())
			out.Reset()
			errs.Reset()
			maintenanceRun(t, imageIOArgs(destination, "load", "--quiet", "--input", archive), &out, &errs)
			if format == "oci-archive" {
				server := httptest.NewServer(http.FileServer(http.Dir(filepath.Dir(archive))))
				defer server.Close()
				urlDestination := maintenanceStoreOptions(t.TempDir())
				maintenanceRun(t, imageIOArgs(urlDestination, "load", "--quiet", "--input", server.URL+"/"+filepath.Base(archive)), &out, &errs)
			}
			err := buildah.WithStore(destination, func(backend storage.Store) error {
				if _, err := oci.StoredImageName(backend, "tagged"); err != nil && !strings.HasSuffix(format, "-dir") {
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
	maintenanceWriteLayout(t, options, layout, root, "source")
	server := httptest.NewServer(registry.New())
	defer server.Close()
	reference := strings.TrimPrefix(server.URL, "http://") + "/coopr/image-io:test"
	digestFile := filepath.Join(t.TempDir(), "digest")
	var out, errs bytes.Buffer
	maintenanceRun(t, imageIOArgs(options, "push", "--tls-verify=false", "--digestfile", digestFile, "source", reference), &out, &errs)
	if out.Len() != 0 || !strings.Contains(errs.String(), "Writing manifest") {
		t.Fatalf("push stdout=%q stderr=%q", &out, &errs)
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
	maintenanceRun(t, imageIOArgs(destination, "pull", "--tls-verify=false", reference), &out, &errs)
	if !strings.Contains(errs.String(), "Writing manifest") || len(strings.TrimSpace(out.String())) != 64 {
		t.Fatalf("pull stdout=%q stderr=%q", &out, &errs)
	}
	out.Reset()
	errs.Reset()
	if status := run(imageIOArgs(options, "push", "-q", "--tls-verify=false", "source", reference), &out, &errs); status != 0 || out.Len() != 0 || errs.Len() != 0 {
		t.Fatalf("quiet push status=%d stdout=%q stderr=%q", status, &out, &errs)
	}
	out.Reset()
	errs.Reset()
	if status := run(imageIOArgs(maintenanceStoreOptions(t.TempDir()), "pull", "-q", "--tls-verify=false", reference), &out, &errs); status != 0 || len(strings.TrimSpace(out.String())) != 64 || errs.Len() != 0 {
		t.Fatalf("quiet pull status=%d stdout=%q stderr=%q", status, &out, &errs)
	}
	t.Run("partial pull", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		missing := strings.TrimSuffix(reference, ":test") + ":missing"
		if status := run(imageIOArgs(maintenanceStoreOptions(t.TempDir()), "pull", "--quiet", "--retry=0", "--tls-verify=false", missing, reference), &stdout, &stderr); status == 0 || len(strings.TrimSpace(stdout.String())) != 64 || !strings.Contains(stderr.String(), "missing") {
			t.Fatalf("partial pull status=%d out=%q err=%q", status, &stdout, &stderr)
		}
	})
	t.Run("all tags", func(t *testing.T) {
		second := strings.TrimSuffix(reference, ":test") + ":second"
		var stdout, stderr bytes.Buffer
		maintenanceRun(t, imageIOArgs(options, "push", "--quiet", "--tls-verify=false", "source", second), &stdout, &stderr)
		target := maintenanceStoreOptions(t.TempDir())
		maintenanceRun(t, imageIOArgs(target, "pull", "--all-tags", "--quiet", "--tls-verify=false", strings.TrimSuffix(reference, ":test")), &stdout, &stderr)
		if len(strings.Fields(stdout.String())) != 2 {
			t.Fatalf("all-tags IDs=%q", &stdout)
		}
		for _, name := range []string{reference, second} {
			exists, err := buildah.ImageExists(ctx, target, name)
			if err != nil || !exists {
				t.Fatalf("missing tag %s: %v", name, err)
			}
		}
	})
	t.Run("converted manifest digest", func(t *testing.T) {
		converted := strings.TrimSuffix(reference, ":test") + ":docker"
		var stdout, stderr bytes.Buffer
		maintenanceRun(t, imageIOArgs(options, "push", "--quiet", "--format=v2s2", "--tls-verify=false", "--digestfile", digestFile, "source", converted), &stdout, &stderr)
		recorded, err := os.ReadFile(digestFile)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.Get(server.URL + "/v2/coopr/image-io/manifests/docker")
		if err != nil {
			t.Fatal(err)
		}
		actual, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(recorded) != digest.FromBytes(actual).String() || string(recorded) == root.Digest.String() {
			t.Fatalf("converted digestfile=%s actual=%s original=%s", recorded, digest.FromBytes(actual), root.Digest)
		}
	})

	outputErr := errors.New("pull stdout unavailable")
	errs.Reset()
	if status := run(imageIOArgs(destination, "pull", "--policy", "never", reference), &maintenanceFailingWriter{err: outputErr}, &errs); status == 0 || !strings.Contains(errs.String(), outputErr.Error()) {
		t.Fatalf("pull writer failure status=%d stderr=%q", status, &errs)
	}
	errs.Reset()
	if status := run(imageIOArgs(options, "push", "-q", "--tls-verify=false", "--digestfile", t.TempDir(), "source", reference), &out, &errs); status == 0 || !strings.Contains(errs.String(), "is a directory") {
		t.Fatalf("push digestfile failure status=%d stderr=%q", status, &errs)
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

func TestImagePushReportsFailureOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping supervised native registry transfer in short mode")
	}
	options := maintenanceStoreOptions(t.TempDir())
	layout, root, _ := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "failed-push")
	maintenanceWriteLayout(t, options, layout, root, "failed-push")
	server := httptest.NewServer(registry.New())
	destination := strings.TrimPrefix(server.URL, "http://") + "/coopr/failure:test"
	server.Close()
	for _, quiet := range []bool{false, true} {
		t.Run(fmt.Sprintf("quiet=%t", quiet), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := imageIOArgs(options, "push", "--tls-verify=false", "--retry=0", fmt.Sprintf("--quiet=%t", quiet), "failed-push", destination)
			if status := run(args, &stdout, &stderr); status == 0 || stdout.Len() != 0 {
				t.Fatalf("status=%d stdout=%q stderr=%q", status, &stdout, &stderr)
			}
			if strings.Count(stderr.String(), "connection refused") != 1 || strings.Count(stderr.String(), "Error: ") != 1 {
				t.Fatalf("push failure must be reported once: %q", &stderr)
			}
		})
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
	maintenanceRun(t, imageIOArgs(options, "history", "--format", "json", "history"), &out, &errs)
	var actual []libimage.ImageHistory
	if err := json.Unmarshal(out.Bytes(), &actual); err != nil {
		t.Fatal(err)
	}
	if len(actual) != 2 || actual[0].CreatedBy != history[1].CreatedBy || actual[0].Comment != history[1].Comment || actual[0].Size != 0 || actual[1].CreatedBy != history[0].CreatedBy {
		t.Fatalf("history=%+v", actual)
	}
}

func TestImageIOLoadPreservesNativeIndexes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping native indexed archive loading in short mode")
	}
	for _, mode := range []string{"multi-platform", "foreign-only", "duplicate-platform"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			native := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
			foreign := v1.Platform{OS: "linux", Architecture: "arm64"}
			if native.Architecture == foreign.Architecture {
				foreign.Architecture = "amd64"
			}
			platforms := []v1.Platform{native, foreign}
			switch mode {
			case "foreign-only":
				if foreign.Architecture == "arm64" {
					foreign.Variant = "v8"
				}
				platforms = []v1.Platform{foreign}
			case "duplicate-platform":
				native.OSFeatures = []string{"z", "a"}
				platforms = []v1.Platform{native, native}
			}
			var variants []oci.ImageVariant
			var members []v1.Descriptor
			for i, platform := range platforms {
				label := fmt.Sprintf("member-%d", i)
				layout, child, _ := maintenanceImageLayout(t, platform, label)
				child.Platform = &platform
				if mode == "duplicate-platform" {
					child.Annotations = map[string]string{"example.com/member": label}
				}
				members = append(members, child)
				variants = append(variants, oci.ImageVariant{Layout: layout, Manifest: child, Platform: platform})
			}
			indexLayout := filepath.Join(t.TempDir(), "index")
			var root v1.Descriptor
			var data []byte
			if mode == "multi-platform" {
				var err error
				root, data, err = oci.ImageIndexDescriptor(variants, "oci")
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var err error
				data, err = json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: members})
				if err != nil {
					t.Fatal(err)
				}
				root = oci.Descriptor(v1.MediaTypeImageIndex, data)
			}
			writeImageIndexFixture(t, ctx, indexLayout, root, data, variants)
			source := maintenanceStoreOptions(t.TempDir())
			if err := buildah.WithStore(source, func(backend storage.Store) error {
				_, err := nativeimage.BorrowStore(backend).SeedIndex(ctx, indexLayout, root, "multi")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var out, errs bytes.Buffer
			for round := 0; round < 2; round++ {
				archive := filepath.Join(t.TempDir(), "index.tar")
				out.Reset()
				errs.Reset()
				maintenanceRun(t, imageIOArgs(source, "save", "--format=oci-archive", "--output", archive, "multi"), &out, &errs)
				if !strings.Contains(errs.String(), "Writing manifest") {
					t.Fatalf("missing real OCI save progress: %s", &errs)
				}
				destination := maintenanceStoreOptions(t.TempDir())
				maintenanceRun(t, imageIOArgs(destination, "load", "--input", archive), &out, &errs)
				if err := buildah.WithStore(destination, func(backend storage.Store) error {
					for _, reference := range []string{"multi", root.Digest.String()} {
						loaded, _, selections, err := oci.StoredImageSelections(ctx, backend, reference)
						if err != nil {
							return err
						}
						if loaded.Digest != root.Digest || len(selections) != len(members) {
							t.Fatalf("round %d: loaded root=%s members=%d want %s and %d", round, loaded.Digest, len(selections), root.Digest, len(members))
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				source = destination
				if mode != "duplicate-platform" {
					break
				}
			}
			if mode == "duplicate-platform" {
				server := httptest.NewServer(registry.New())
				defer server.Close()
				remote := strings.TrimPrefix(server.URL, "http://") + "/duplicate:test"
				maintenanceRun(t, imageIOArgs(source, "push", "--quiet", "--tls-verify=false", "multi", remote), &out, &errs)
				response, err := http.Get(server.URL + "/v2/duplicate/manifests/test")
				if err != nil {
					t.Fatal(err)
				}
				data, readErr := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if readErr != nil {
					t.Fatal(readErr)
				}
				if digest.FromBytes(data) != root.Digest {
					t.Fatalf("pushed duplicate index changed %s to %s", root.Digest, digest.FromBytes(data))
				}
			}
		})
	}
}

func TestImageIOArchiveStandardStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping native OCI archive loading in short mode")
	}
	for _, format := range []string{"oci-archive", "docker-archive"} {
		t.Run(format, func(t *testing.T) {
			ctx := context.Background()
			options := maintenanceStoreOptions(t.TempDir())
			layout, root, config := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "streams")
			maintenanceWriteLayout(t, options, layout, root, "streams")
			var archive, errs bytes.Buffer
			if status := run(imageIOArgs(options, "save", "--format", format, "streams"), &archive, &errs); status != 0 {
				t.Fatalf("stdout save status=%d: %s", status, &errs)
			}

			outputErr := errors.New("archive stdout unavailable")
			if status := run(imageIOArgs(options, "save", "--format", format, "streams"), &maintenanceFailingWriter{err: outputErr}, &errs); status == 0 || !strings.Contains(errs.String(), outputErr.Error()) {
				t.Fatalf("save writer failure status=%d stderr=%q", status, &errs)
			}
			errs.Reset()
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

			command = newRootCommand()
			command.SetArgs(imageIOArgs(destination, "load", "--quiet"))
			command.SetIn(bytes.NewReader(archive.Bytes()))
			command.SetOut(&maintenanceFailingWriter{err: outputErr})
			command.SetErr(&errs)
			if err := command.ExecuteContext(ctx); !errors.Is(err, outputErr) {
				t.Fatalf("load writer error = %v", err)
			}

			command = newRootCommand()
			command.SetArgs(imageIOArgs(destination, "load", "--quiet"))
			command.SetIn(strings.NewReader("not an OCI archive"))
			command.SetOut(&out)
			command.SetErr(&errs)
			if err := command.ExecuteContext(ctx); err == nil {
				t.Fatal("malformed archive accepted by public load")
			}
			err := buildah.WithStore(destination, func(backend storage.Store) error {
				selector := root.Digest.String()
				if format == "docker-archive" {
					selector = "streams"
				}
				selected, err := oci.ResolveStoredImage(ctx, backend, selector, v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
				if err == nil && format == "oci-archive" && selected.Selected.Digest != root.Digest {
					t.Errorf("stream archive changed exact manifest")
				}
				if err == nil && selected.Config.Digest != config {
					t.Errorf("stream archive changed image configuration")
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestImageIOExistsLocalStatus(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	layout, root, _ := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "exists")
	maintenanceWriteLayout(t, options, layout, root, "exists-local")
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
			maintenanceWriteLayout(t, source, layout, root, "archive-source")
			for _, selector := range []string{root.Digest.String(), "archive-source"} {
				archive := filepath.Join(t.TempDir(), "image.tar")
				var out, errs bytes.Buffer
				maintenanceRun(t, imageIOArgs(source, "save", "--quiet", "--output", archive, selector), &out, &errs)
				destination := maintenanceStoreOptions(t.TempDir())
				out.Reset()
				errs.Reset()
				maintenanceRun(t, imageIOArgs(destination, "load", "--quiet", "--input", archive), &out, &errs)
				err := buildah.WithStore(destination, func(backend storage.Store) error {
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
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	layout, first, id := maintenanceImageLayout(t, platform, "shared-config")
	source := maintenanceStoreOptions(t.TempDir())
	maintenanceWriteLayout(t, source, layout, first, "shared")
	var second v1.Descriptor
	var secondData []byte
	err := buildah.WithStore(source, func(backend storage.Store) error {
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
	maintenanceRun(t, imageIOArgs(source, "save", "--quiet", "--multi-image-archive", "--output", archive, first.Digest.String(), second.Digest.String()), &out, &errs)
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
	maintenanceRun(t, imageIOArgs(destination, "load", "--quiet", "--input", archive), &out, &errs)
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

func TestImageIOSaveExplicitUncompressed(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native layer import")
	}
	ctx := context.Background()
	root := t.TempDir()
	store := maintenanceStoreOptions(filepath.Join(root, "store"))
	if err := os.WriteFile(filepath.Join(root, "payload"), bytes.Repeat([]byte("compressible payload\n"), 100), 0600); err != nil {
		t.Fatal(err)
	}
	definition, err := definition.Parse(strings.NewReader(`from "scratch" {
    copy "payload" "/payload"
}
`))
	if err != nil {
		t.Fatal(err)
	}
	built, err := buildah.BuildDefinitionSupervised(ctx, definition, planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH}, buildah.SupervisedPlanOptions{Store: store, ContextDir: root, Isolation: "rootless", Output: buildah.Output{Path: filepath.Join(root, "source"), DisableCompression: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, uncompressed := range []bool{true, false} {
		target := filepath.Join(t.TempDir(), "saved")
		var stdout, stderr bytes.Buffer
		maintenanceRun(t, imageIOArgs(store, "save", "--format=oci-dir", "--output", target, fmt.Sprintf("--uncompressed=%t", uncompressed), built.ImageID), &stdout, &stderr)
		top, err := oci.LayoutRoot(target)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(target, "blobs", "sha256", top.Digest.Encoded()))
		if err != nil {
			t.Fatal(err)
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatal(err)
		}
		want := v1.MediaTypeImageLayerGzip
		if uncompressed {
			want = v1.MediaTypeImageLayer
		}
		if len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != want {
			t.Fatalf("uncompressed=%t layers=%+v", uncompressed, manifest.Layers)
		}
		if uncompressed && top.Digest.String() != built.ManifestDigest {
			t.Fatalf("uncompressed save changed manifest %s to %s", built.ManifestDigest, top.Digest)
		}
	}
}

func writeImageIndexFixture(t *testing.T, ctx context.Context, path string, root v1.Descriptor, data []byte, variants []oci.ImageVariant) {
	t.Helper()
	target, err := orasoci.NewWithContext(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := orasoci.NewStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range variants {
		source, err := orasoci.NewWithContext(ctx, variant.Layout)
		if err != nil {
			t.Fatal(err)
		}
		if err := oras.CopyGraph(ctx, source, blobs, variant.Manifest, oras.CopyGraphOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := target.Push(ctx, root, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := target.Tag(ctx, root, root.Digest.String()); err != nil {
		t.Fatal(err)
	}
}
