package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/imagestore"
	"coopr/internal/oci"
	"coopr/internal/transfer"

	"github.com/containerd/platforms"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestManifestCommandShape(t *testing.T) {
	for _, name := range []string{"create", "add", "annotate", "inspect", "push", "remove", "rm", "exists"} {
		command, _, err := newRootCommand().Find([]string{"manifest", name})
		if err != nil || command.Name() != name {
			t.Fatalf("missing manifest %s: %v", name, err)
		}
	}
	command, _, _ := newRootCommand().Find([]string{"manifest", "push"})
	flag := command.Flags().Lookup("all")
	if flag == nil || flag.DefValue != "true" {
		t.Fatalf("manifest push must copy all images by default: %v", flag)
	}
}

func TestManifestCommandsRejectMissingArguments(t *testing.T) {
	for _, name := range []string{"create", "add", "annotate", "inspect", "push", "remove", "rm", "exists"} {
		var out, errs bytes.Buffer
		if code := run([]string{"manifest", name}, &out, &errs); code == 0 {
			t.Errorf("manifest %s accepted no arguments", name)
		}
	}
}

func manifestTestArgs(options buildah.StoreOptions, args ...string) []string {
	return append([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "manifest"}, args...)
}

func TestManifestNativeLifecycleAndCooprMutationVisibility(t *testing.T) {
	ctx := context.Background()
	options := maintenanceStoreOptions(t.TempDir())
	native := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	layout, child, _ := maintenanceImageLayout(t, native, "manifest")
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteLayout(ctx, layout, child, "child"); err != nil {
		t.Fatal(err)
	}
	indexLayout := filepath.Join(t.TempDir(), "index")
	original, _, err := oci.AssembleImageIndex(ctx, indexLayout, []oci.ImageVariant{{Layout: layout, Manifest: child, Platform: native}}, "oci")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteIndexLayout(ctx, indexLayout, original, "original"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	execute := func(args ...string) {
		t.Helper()
		out.Reset()
		errs.Reset()
		if status := run(manifestTestArgs(options, args...), &out, &errs); status != 0 {
			t.Fatalf("manifest %v status=%d stderr=%s", args, status, &errs)
		}
	}
	execute("create", "empty")
	execute("inspect", "empty")
	var data v1.Index
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Manifests) != 0 {
		t.Fatalf("empty list=%s", &out)
	}
	execute("exists", "empty")
	execute("annotate", "--index", "--annotation", "example.com/index=empty", "empty")
	execute("inspect", "empty")
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data.Annotations["example.com/index"] != "empty" || len(data.Manifests) != 0 {
		t.Fatalf("empty index annotation=%s", &out)
	}
	var childID string
	err = buildah.WithStore(options, func(backend storage.Store) error {
		selected, err := oci.ResolveStoredImage(ctx, backend, "child", native)
		if err == nil {
			childID = selected.StorageImageID
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	execute("add", "empty", childID)
	execute("create", "digest-member", child.Digest.String())
	execute("inspect", "digest-member")
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Manifests) != 1 || data.Manifests[0].Digest != child.Digest {
		t.Fatalf("digest member=%s", &out)
	}
	execute("inspect", "empty")
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Manifests) != 1 || data.Manifests[0].Digest != child.Digest {
		t.Fatalf("added list=%s", &out)
	}
	execute("annotate", "--annotation", "example.com/test=value", "original", child.Digest.String())
	var current digest.Digest
	err = buildah.WithStore(options, func(backend storage.Store) error {
		root, raw, indexed, err := oci.StoredImageIndex(ctx, backend, "original")
		if err != nil {
			return err
		}
		if !indexed || root.Digest == original.Digest {
			t.Fatal("Coopr reads stale original index after native annotation")
		}
		if err := json.Unmarshal(raw, &data); err != nil {
			return err
		}
		if len(data.Manifests) != 1 || data.Manifests[0].Annotations["example.com/test"] != "value" {
			t.Fatalf("Coopr index=%s", raw)
		}
		current = root.Digest
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !testing.Short() {
		copiedArchive := filepath.Join(t.TempDir(), "mutated.tar")
		out.Reset()
		errs.Reset()
		if status := run(imageIOArgs(options, "save", "--format", "oci-archive", "--output", copiedArchive, "original"), &out, &errs); status != 0 {
			t.Fatalf("save mutated list status=%d stderr=%s", status, &errs)
		}
		archive, err := orasoci.NewFromTar(ctx, copiedArchive)
		if err != nil {
			t.Fatal(err)
		}
		root, err := archive.Resolve(ctx, current.String())
		if err != nil {
			t.Fatalf("copied stale native list: %v", err)
		}
		if root.Digest != current {
			t.Fatalf("copied digest=%s want %s", root.Digest, current)
		}
	}

	execute("remove", "original", child.Digest.String())
	execute("inspect", "original")
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Manifests) != 0 {
		t.Fatalf("removed list=%s", &out)
	}
	execute("rm", "empty", "original")
	out.Reset()
	errs.Reset()
	if status := run(manifestTestArgs(options, "exists", "empty"), &out, &errs); status != 1 || out.Len() != 0 || errs.Len() != 0 {
		t.Fatalf("missing exists status=%d out=%s err=%s", status, &out, &errs)
	}
}

func TestManifestPushFetchesRemoteMembersAndCopiesAllByDefault(t *testing.T) {
	ctx := context.Background()
	options := maintenanceStoreOptions(t.TempDir())
	handler := registry.New()
	var recording atomic.Bool
	var childCopies atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if recording.Load() && (r.Method == http.MethodPut || r.Method == http.MethodPost || r.Method == http.MethodPatch) && (strings.Contains(r.URL.Path, "/blobs/") || strings.Contains(r.URL.Path, "/manifests/sha256:")) {
			childCopies.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	memberA := host + "/source/a:test"
	memberB := host + "/source/b:test"
	native := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	foreign := v1.Platform{OS: "linux", Architecture: "arm64"}
	if native.Architecture == "arm64" {
		foreign.Architecture = "amd64"
	}
	sourceStore := maintenanceStoreOptions(t.TempDir())
	for _, test := range []struct {
		name     string
		platform v1.Platform
	}{{memberA, native}, {memberB, foreign}} {
		layout, root, _ := maintenanceImageLayout(t, test.platform, test.name)
		if _, err := transfer.CopyRoot(ctx, oci.Image, layout, root, transfer.Destination{Transport: "registry", Name: test.name}, transfer.Options{BuildStore: sourceStore, TLSVerify: new(false)}); err != nil {
			t.Fatal(err)
		}
	}
	var out, errs bytes.Buffer
	if status := run(manifestTestArgs(options, "create", "--tls-verify=false", "remote", "docker://"+memberA, memberB), &out, &errs); status != 0 {
		t.Fatalf("create remote list status=%d: %s", status, &errs)
	}
	err := buildah.WithStore(options, func(backend storage.Store) error {
		images, err := backend.Images()
		if err != nil {
			return err
		}
		if len(images) != 1 {
			t.Fatalf("manifest add unexpectedly pulled %d native records", len(images))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	destination := host + "/destination/all:test"
	out.Reset()
	errs.Reset()
	if status := run(manifestTestArgs(options, "push", "--tls-verify=false", "remote", destination), &out, &errs); status != 0 {
		t.Fatalf("push remote list status=%d: %s", status, &errs)
	}
	indexOnly := host + "/destination/all:index-only"
	recording.Store(true)
	out.Reset()
	errs.Reset()
	if status := run(manifestTestArgs(options, "push", "--tls-verify=false", "--all=false", "remote", indexOnly), &out, &errs); status != 0 {
		t.Fatalf("push index only status=%d: %s", status, &errs)
	}
	response, err := http.Get(server.URL + "/v2/destination/all/manifests/index-only")
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read index only: %v %v", readErr, closeErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("index only status=%d body=%s", response.StatusCode, raw)
	}
	var index v1.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 2 {
		t.Fatalf("index-only root=%s", raw)
	}
	for _, child := range index.Manifests {
		response, err := http.Get(server.URL + "/v2/destination/all/manifests/" + child.Digest.String())
		if err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("pre-existing child %s missing status=%d", child.Digest, response.StatusCode)
		}
	}
	recording.Store(false)
	if childCopies.Load() != 0 {
		t.Fatalf("--all=false uploaded %d child manifests/blobs", childCopies.Load())
	}
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	for _, platform := range []v1.Platform{native, foreign} {
		resolved, err := resolver.ResolveRemoteImage(ctx, destination, platform)
		if err != nil {
			t.Fatalf("default --all omitted %s: %v", platforms.Format(platform), err)
		}
		if resolved.Platform.Architecture != platform.Architecture {
			t.Fatalf("pushed wrong platform %+v", resolved.Platform)
		}
	}
}

func TestManifestExistsBrokenStoreIsOperationalFailure(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	if err := os.WriteFile(options.GraphRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if status := run(manifestTestArgs(options, "exists", "missing"), &out, &errs); status != 125 || errs.Len() == 0 {
		t.Fatalf("broken-store exists status=%d out=%q err=%q", status, &out, &errs)
	}
}
