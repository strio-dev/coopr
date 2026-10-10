package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/imagestore"
	"coopr/internal/oci"
	"coopr/internal/transfer"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/manifest"
)

func TestManifestInlineOverridesAndNamedMember(t *testing.T) {
	ctx := context.Background()
	options := maintenanceStoreOptions(t.TempDir())
	layout, child, _ := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "overrides")
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteLayout(ctx, layout, child, "member"); err != nil {
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
			t.Fatalf("%v status=%d: %s", args, status, &errs)
		}
	}
	execute("create", "--annotation", "example.com/index=one", "list")
	execute("add", "--arch", "arm64", "--variant", "v8", "--os-version", "version", "--annotation", "example.com/member=two", "list", "member")
	execute("annotate", "--annotation", "example.com/named=three", "list", "member")
	execute("inspect", "list")
	var data v1.Index
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data.Annotations["example.com/index"] != "one" || len(data.Manifests) != 1 {
		t.Fatalf("index=%s", &out)
	}
	m := data.Manifests[0]
	if m.Digest != child.Digest || m.Platform.Architecture != "arm64" || m.Platform.Variant != "v8" || m.Platform.OSVersion != "version" || m.Annotations["example.com/member"] != "two" || m.Annotations["example.com/named"] != "three" {
		t.Fatalf("member=%+v", m)
	}
	execute("rm", "--ignore", "missing", "list")
	execute("create", "list")
	out.Reset()
	errs.Reset()
	if status := run(manifestTestArgs(options, "rm", "missing", "list"), &out, &errs); status != 1 || !strings.Contains(out.String(), "Deleted:") || !strings.Contains(errs.String(), "missing") {
		t.Fatalf("partial rm status=%d out=%s err=%s", status, &out, &errs)
	}
}

func TestManifestRemoteInspectAndPushRemoval(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(registry.New())
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	options := maintenanceStoreOptions(t.TempDir())
	layout, child, _ := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "remote-inspect")
	indexLayout := filepath.Join(t.TempDir(), "layout")
	index, _, err := oci.AssembleImageIndex(ctx, indexLayout, []oci.ImageVariant{{Layout: layout, Manifest: child, Platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH}}}, "oci")
	if err != nil {
		t.Fatal(err)
	}
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteIndexLayout(ctx, indexLayout, index, "list"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	remote := host + "/test/index:latest"
	if status := run(manifestTestArgs(options, "push", "--tls-verify=false", "list", remote), &out, &errs); status != 0 {
		t.Fatalf("push status=%d %s", status, &errs)
	}
	out.Reset()
	errs.Reset()
	if status := run(manifestTestArgs(options, "inspect", "--tls-verify=false", remote), &out, &errs); status != 0 {
		t.Fatalf("inspect status=%d %s", status, &errs)
	}
	var inspected v1.Index
	if err := json.Unmarshal(out.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if len(inspected.Manifests) != 1 || inspected.Manifests[0].Digest != child.Digest {
		t.Fatalf("remote index=%s", &out)
	}
	// Native push controls must reach the list copier, including the resulting digest.
	digestFile := filepath.Join(t.TempDir(), "pushed.digest")
	out.Reset()
	errs.Reset()
	converted := host + "/test/converted:latest"
	if status := run(manifestTestArgs(options, "push", "--format=v2s2", "--remove-signatures", "--digestfile", digestFile, "--tls-verify=false", "list", converted), &out, &errs); status != 0 {
		t.Fatalf("converted push status=%d %s", status, &errs)
	}
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	raw, mediaType, err := resolver.RemoteImageManifest(ctx, converted)
	if err != nil {
		t.Fatal(err)
	}
	writtenDigest, err := os.ReadFile(digestFile)
	if err != nil {
		t.Fatal(err)
	}
	if mediaType != manifest.DockerV2ListMediaType || string(writtenDigest) != digest.FromBytes(raw).String() {
		t.Fatalf("push ignored format or wrote old digest: type=%s digest=%s manifest=%s", mediaType, writtenDigest, raw)
	}
	// A failed push must keep the local list.
	out.Reset()
	errs.Reset()
	if status := run(manifestTestArgs(options, "push", "--rm", "list", "bad name"), &out, &errs); status == 0 {
		t.Fatal("invalid push succeeded")
	}
	out.Reset()
	errs.Reset()
	if status := run(manifestTestArgs(options, "exists", "list"), &out, &errs); status != 0 {
		t.Fatalf("failed push removed list: %s", &errs)
	}
	// Ordinary local images must not mask remote indexes of the same name.
	if _, err := transfer.CopyRoot(ctx, oci.Image, layout, child, transfer.Destination{Transport: "local", Name: remote}, transfer.Options{BuildStore: options}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errs.Reset()
	if status := run(manifestTestArgs(options, "inspect", "--tls-verify=false", remote), &out, &errs); status != 0 {
		t.Fatalf("local ordinary-image fallback status=%d %s", status, &errs)
	}
	out.Reset()
	errs.Reset()
	if status := run(manifestTestArgs(options, "push", "--rm", "--tls-verify=false", "list", host+"/test/removed:latest"), &out, &errs); status != 0 {
		t.Fatalf("push rm status=%d %s", status, &errs)
	}
	out.Reset()
	errs.Reset()
	if status := run(manifestTestArgs(options, "exists", "list"), &out, &errs); status != 1 {
		t.Fatalf("successful push retained list status=%d %s", status, &errs)
	}
}

func TestManifestRemoteInspectSingleOCIImage(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(registry.New())
	defer server.Close()
	remote := strings.TrimPrefix(server.URL, "http://") + "/test/single:latest"
	layout, child, imageID := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "single-inspect")
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.PublishLayout(ctx, remote, layout, child); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if status := run(manifestTestArgs(maintenanceStoreOptions(t.TempDir()), "inspect", "--tls-verify=false", remote), &out, &errs); status != 0 {
		t.Fatalf("inspect status=%d %s", status, &errs)
	}
	var inspected v1.Manifest
	if err := json.Unmarshal(out.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.MediaType != v1.MediaTypeImageManifest || inspected.Config.Digest != imageID || len(inspected.Layers) != 0 {
		t.Fatalf("remote manifest=%s", &out)
	}
	if !strings.Contains(errs.String(), "Warning: The manifest type "+v1.MediaTypeImageManifest+" is not a manifest list but a single image.") {
		t.Fatalf("missing single-image warning: %s", &errs)
	}
}

func TestManifestPushUsesNativeListOptionSurface(t *testing.T) {
	cmd := newManifestPushCommand()
	for _, option := range []string{"format", "compression-format", "compression-level", "force-compression", "add-compression", "remove-signatures", "rm"} {
		if cmd.Flags().Lookup(option) == nil {
			t.Errorf("missing native list option --%s", option)
		}
	}
	for _, option := range []string{"encryption-key", "encrypt-layer"} {
		if cmd.Flags().Lookup(option) != nil {
			t.Errorf("native list copier cannot implement --%s", option)
		}
	}
}
