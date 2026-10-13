package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/imagestore"
	"coopr/internal/oci"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func TestManifestMemberTransportRouting(t *testing.T) {
	ctx := context.Background()
	options := maintenanceStoreOptions(t.TempDir())
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	layout, image, _ := maintenanceImageLayout(t, platform, "routing")
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteLayout(ctx, layout, image, "docker:tag"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	err = buildah.WithStore(options, func(backend storage.Store) error {
		for _, name := range []string{
			"docker-daemon:example:latest",
			"docker-archive:" + filepath.Join(t.TempDir(), "image.tar"),
			"dir:" + t.TempDir(),
			"oci-archive:" + filepath.Join(t.TempDir(), "image.oci.tar"),
			"docker://example.com/image:latest",
		} {
			got, err := manifestMemberReference(ctx, backend, name)
			if err != nil || got != name {
				t.Errorf("manifest member %q = %q, %v; want unchanged native transport", name, got, err)
			}
		}
		got, err := manifestMemberReference(ctx, backend, "docker:tag")
		if err != nil || !strings.HasPrefix(got, "containers-storage:") || !strings.Contains(got, image.Digest.String()) {
			t.Errorf("ordinary docker:tag was not pinned to its local image: %q, %v", got, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestManifestFilesystemTransportLifecycle(t *testing.T) {
	ctx := context.Background()
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	layout, _, _ := maintenanceImageLayout(t, platform, "transport-content")
	source, err := alltransports.ParseImageName("oci:" + layout)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := signature.NewPolicyContext(&signature.Policy{Default: []signature.PolicyRequirement{signature.NewPRInsecureAcceptAnything()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := policy.Destroy(); err != nil {
			t.Error(err)
		}
	})
	for _, transport := range []string{"dir", "docker-archive", "oci-archive"} {
		t.Run(transport, func(t *testing.T) {
			name := transport + ":" + filepath.Join(t.TempDir(), "image")
			destination, err := alltransports.ParseImageName(name)
			if err != nil {
				t.Fatal(err)
			}
			system := &types.SystemContext{BigFilesTemporaryDir: t.TempDir()}
			raw, err := imagecopy.Image(ctx, policy, destination, source, &imagecopy.Options{SourceCtx: system, DestinationCtx: system})
			if err != nil {
				t.Fatal(err)
			}
			wantDigest := digest.FromBytes(raw)
			options := maintenanceStoreOptions(t.TempDir())
			var out, errs bytes.Buffer
			execute := func(args ...string) {
				t.Helper()
				out.Reset()
				errs.Reset()
				if status := run(manifestTestArgs(options, args...), &out, &errs); status != 0 {
					t.Fatalf("manifest %v status=%d: %s", args, status, &errs)
				}
			}
			execute("create", "created", name)
			execute("create", "added")
			execute("add", "added", name)
			for _, list := range []string{"created", "added"} {
				execute("annotate", "--annotation", "example.com/transport="+transport, list, name)
				execute("inspect", list)
				var index v1.Index
				if err := json.Unmarshal(out.Bytes(), &index); err != nil {
					t.Fatal(err)
				}
				if len(index.Manifests) != 1 {
					t.Fatalf("manifest list: %s", &out)
				}
				member := index.Manifests[0]
				if member.Digest != wantDigest || member.Platform == nil || member.Platform.OS != platform.OS || member.Platform.Architecture != platform.Architecture || member.Annotations["example.com/transport"] != transport {
					t.Fatalf("manifest member does not preserve source identity/platform and annotation: %+v", member)
				}
			}
		})
	}
}

func TestManifestPushFilesystemTransports(t *testing.T) {
	ctx := context.Background()
	options := maintenanceStoreOptions(t.TempDir())
	native := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	foreign := v1.Platform{OS: "linux", Architecture: "arm64"}
	if native.Architecture == "arm64" {
		foreign.Architecture = "amd64"
	}
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	expected := make(map[digest.Digest]v1.Platform)
	for i, platform := range []v1.Platform{native, foreign} {
		layout, root, _ := maintenanceImageLayout(t, platform, "manifest-push")
		if _, err := store.WriteLayout(ctx, layout, root, []string{"member-native", "member-foreign"}[i]); err != nil {
			t.Fatal(err)
		}
		expected[root.Digest] = platform
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	execute := func(args ...string) int {
		t.Helper()
		out.Reset()
		errs.Reset()
		return run(manifestTestArgs(options, args...), &out, &errs)
	}
	if status := execute("create", "output-list", "member-native", "member-foreign"); status != 0 {
		t.Fatalf("create status=%d: %s", status, &errs)
	}
	for _, transport := range []string{"oci", "dir"} {
		t.Run(transport, func(t *testing.T) {
			target := transport + ":" + filepath.Join(t.TempDir(), "output")
			if transport == "oci" {
				target += ":release"
			}
			digestFile := filepath.Join(t.TempDir(), "digest")
			if status := execute("push", "--quiet", "--digestfile", digestFile, "output-list", target); status != 0 {
				t.Fatalf("push status=%d: %s", status, &errs)
			}
			if out.Len() != 0 || errs.Len() != 0 {
				t.Fatalf("quiet push emitted stdout=%q stderr=%q", &out, &errs)
			}
			reference, err := alltransports.ParseImageName(target)
			if err != nil {
				t.Fatal(err)
			}
			source, err := reference.NewImageSource(ctx, &types.SystemContext{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := source.Close(); err != nil {
					t.Error(err)
				}
			}()
			raw, _, err := source.GetManifest(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			writtenDigest, err := os.ReadFile(digestFile)
			if err != nil {
				t.Fatal(err)
			}
			if string(writtenDigest) != digest.FromBytes(raw).String() {
				t.Fatalf("digestfile=%q does not identify published index", writtenDigest)
			}
			var index v1.Index
			if err := json.Unmarshal(raw, &index); err != nil {
				t.Fatal(err)
			}
			if len(index.Manifests) != len(expected) {
				t.Fatalf("lost children: %s", raw)
			}
			for _, child := range index.Manifests {
				platform, ok := expected[child.Digest]
				if !ok || child.Platform == nil || child.Platform.OS != platform.OS || child.Platform.Architecture != platform.Architecture {
					t.Fatalf("unexpected child descriptor: %+v", child)
				}
				childRaw, _, err := source.GetManifest(ctx, &child.Digest)
				if err != nil {
					t.Fatal(err)
				}
				if digest.FromBytes(childRaw) != child.Digest {
					t.Fatalf("child manifest does not match %s", child.Digest)
				}
				var manifest v1.Manifest
				if err := json.Unmarshal(childRaw, &manifest); err != nil {
					t.Fatal(err)
				}
				blob, _, err := source.GetBlob(ctx, types.BlobInfo{Digest: manifest.Config.Digest}, nil)
				if err != nil {
					t.Fatal(err)
				}
				data, readErr := io.ReadAll(blob)
				closeErr := blob.Close()
				if readErr != nil || closeErr != nil {
					t.Fatalf("config read=%v close=%v", readErr, closeErr)
				}
				if digest.FromBytes(data) != manifest.Config.Digest {
					t.Fatalf("missing or corrupt child config %s", manifest.Config.Digest)
				}
			}
		})
	}

	t.Run("configured-native-store", func(t *testing.T) {
		var original digest.Digest
		if err := buildah.WithStore(options, func(backend storage.Store) error {
			root, _, indexed, err := oci.StoredImageIndex(ctx, backend, "output-list")
			if err != nil {
				return err
			}
			if !indexed {
				t.Fatal("source list is not a native index")
			}
			original = root.Digest
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		// Native containers-storage destinations cannot accept an image index.
		if status := execute("push", "--quiet", "output-list", "containers-storage:localhost/copy:dev"); status == 0 || !strings.Contains(errs.String(), `destination transport "containers-storage" does not support copying multiple images as a group`) {
			t.Fatalf("native manifest destination limitation status=%d: %s", status, &errs)
		}
		if out.Len() != 0 {
			t.Fatalf("failed quiet native push emitted stdout=%q", &out)
		}
		if err := buildah.WithStore(options, func(backend storage.Store) error {
			root, _, indexed, err := oci.StoredImageIndex(ctx, backend, "output-list")
			if err != nil {
				return err
			}
			if !indexed || root.Digest != original {
				t.Fatal("native push modified the original manifest list")
			}
			if _, err := backend.Image("localhost/copy:dev"); !errors.Is(err, storage.ErrImageUnknown) {
				t.Fatalf("failed native push created a destination image: %v", err)
			}
			_, raw, _, err := oci.StoredImageIndex(ctx, backend, "output-list")
			if err != nil {
				return err
			}
			var index v1.Index
			if err := json.Unmarshal(raw, &index); err != nil {
				return err
			}
			if len(index.Manifests) != len(expected) {
				t.Fatalf("failed native push changed source children: %s", raw)
			}
			for _, child := range index.Manifests {
				platform, ok := expected[child.Digest]
				if !ok || child.Platform == nil || child.Platform.OS != platform.OS || child.Platform.Architecture != platform.Architecture {
					t.Fatalf("failed native push changed source child: %+v", child)
				}
				selected, err := oci.ResolveStoredImage(ctx, backend, "output-list", platform)
				if err != nil {
					return err
				}
				if selected.Selected.Digest != child.Digest {
					t.Fatalf("configured native store cannot resolve original child %s", child.Digest)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	for _, transport := range []string{"dir", "oci", "docker-archive"} {
		for _, root := range []string{options.GraphRoot, options.RunRoot} {
			target := transport + ":" + filepath.Join(root, "output-"+transport)
			status := execute("push", "--quiet", "output-list", target)
			if transport == "docker-archive" {
				if status == 0 || !strings.Contains(errs.String(), "does not support copying multiple images as a group") {
					t.Fatalf("native archive limitation %q status=%d: %s", target, status, &errs)
				}
				continue
			}
			if status != 0 || out.Len() != 0 || errs.Len() != 0 {
				t.Fatalf("owned-store output %q status=%d stdout=%q stderr=%q", target, status, &out, &errs)
			}
			reference, err := alltransports.ParseImageName(target)
			if err != nil {
				t.Fatal(err)
			}
			source, err := reference.NewImageSource(ctx, &types.SystemContext{})
			if err != nil {
				t.Fatal(err)
			}
			raw, _, readErr := source.GetManifest(ctx, nil)
			closeErr := source.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("read owned-store output %q: %v, %v", target, readErr, closeErr)
			}
			var index v1.Index
			if err := json.Unmarshal(raw, &index); err != nil {
				t.Fatal(err)
			}
			if len(index.Manifests) != len(expected) {
				t.Fatalf("owned-store output lost children: %s", raw)
			}
			for _, child := range index.Manifests {
				if _, ok := expected[child.Digest]; !ok {
					t.Fatalf("owned-store output changed child: %+v", child)
				}
			}
		}
	}
	if status := execute("exists", "output-list"); status != 0 {
		t.Fatalf("filesystem exports damaged original list: %s", &errs)
	}
	target := "docker-archive:" + filepath.Join(t.TempDir(), "output.tar")
	if status := execute("push", "--quiet", "output-list", target); status == 0 || !strings.Contains(errs.String(), "does not support copying multiple images as a group") {
		t.Fatalf("Docker archive index limitation status=%d: %s", status, &errs)
	}
}
