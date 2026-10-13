package transfer

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestDestinationString(t *testing.T) {
	for _, test := range []struct {
		destination Destination
		want        string
	}{
		{Destination{Transport: "local", Name: "app:latest"}, "app:latest"},
		{Destination{Transport: "local", Name: "local:app"}, "local:app"},
		{Destination{Transport: "registry", Name: "example.com/app:latest"}, "docker://example.com/app:latest"},
		{Destination{Transport: "docker", Name: "app:dev"}, "docker-daemon:app:dev"},
		{Destination{Transport: "oci-archive", Name: "app.tar"}, "oci-archive:app.tar"},
	} {
		if got := test.destination.String(); got != test.want {
			t.Fatalf("destination string = %q, want %q", got, test.want)
		}
	}
}

func TestBuildTagsUseOnlyOrdinaryNames(t *testing.T) {
	for _, kind := range []oci.Kind{oci.Image, oci.Component} {
		for _, name := range []string{"app", "app:dev", "registry:app", "docker:app"} {
			destination, err := ParseTag(name, kind)
			if err != nil || destination.Transport != "local" {
				t.Fatalf("tag %s = %+v, %v", name, destination, err)
			}
		}
		for _, name := range []string{"docker://example.com/app:dev", "docker-daemon:app:dev", "oci-archive:/tmp/app.tar"} {
			if _, err := ParseTag(name, kind); err == nil {
				t.Fatalf("transport accepted as build tag: %s", name)
			}
		}
	}
}

func TestArchiveDestinationReference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.tar")
	for _, kind := range []oci.Kind{oci.Image, oci.Component} {
		value := "oci-archive:" + path + ":app"
		destination, err := ParseDestination(value, kind)
		if err != nil || destination.Name != path || destination.ArchiveReference != "app" || destination.String() != value {
			t.Fatalf("archive destination = %+v, %v", destination, err)
		}
		for _, invalid := range []string{"oci-archive:" + path + ":bad name", "oci-archive:" + path + ":app?tag"} {
			if _, err := ParseDestination(invalid, kind); err == nil {
				t.Fatalf("invalid archive reference accepted: %s", invalid)
			}
		}
	}
}

func TestDestinationTransports(t *testing.T) {
	for _, test := range []struct {
		value, transport, name string
	}{
		{"app:dev", "local", "app:dev"},
		{"local:app", "local", "local:app"},
		{"registry:app", "local", "registry:app"}, {"docker:app", "local", "docker:app"},
		{"podman:5000/team/app:dev", "local", "podman:5000/team/app:dev"},
		{"docker-daemon:app:dev", "docker", "app:dev"},
		{"docker://example.com/team/app:dev", "registry", "example.com/team/app:dev"},
		{"oci-archive:app.tar", "oci-archive", "app.tar"},
	} {
		got, err := ParseDestination(test.value, oci.Image)
		if err != nil || got != (Destination{Transport: test.transport, Name: test.name}) {
			t.Fatalf("destination %q = %+v, %v", test.value, got, err)
		}
	}
	for _, value := range []string{"docker-daemon:app:dev"} {
		if _, err := ParseDestination(value, oci.Component); err == nil || !strings.Contains(err.Error(), "cannot store Coopr component") {
			t.Fatalf("component destination %q accepted: %v", value, err)
		}
	}
	for _, value := range []string{"docker-daemon:app:dev", "oci-archive:app.tar"} {
		if _, err := ParsePushDestination(value, oci.Image); err != nil {
			t.Fatalf("push target %q rejected: %v", value, err)
		}
	}
	if destination, err := ParsePushDestination("localhost:5000/team/app:dev", oci.Image); err != nil || destination.Transport != "registry" {
		t.Fatalf("registry with port rejected: %+v, %v", destination, err)
	}
}

func TestPodmanImageNameUsesOrdinaryTagSyntax(t *testing.T) {
	for _, value := range []string{"podman:dev"} {
		got, err := ParseDestination(value, oci.Image)
		if err != nil || got != (Destination{Transport: "local", Name: "podman:dev"}) {
			t.Fatalf("destination %q = %+v, %v; want an ordinary local tag", value, got, err)
		}
	}
	got, err := ParsePushDestination("podman:dev", oci.Image)
	if err != nil || got != (Destination{Transport: "registry", Name: "podman:dev"}) {
		t.Fatalf("push destination = %+v, %v; want an ordinary registry reference", got, err)
	}
	if _, err := ParseDestination("podman:app:dev", oci.Image); err == nil {
		t.Fatal("invalid multi-colon image name accepted")
	}
}

func TestComponentDestinationsUseOrdinaryReferences(t *testing.T) {
	for _, test := range []struct{ value, name string }{
		{"app", "app:latest"}, {"app:dev", "app:dev"},
		{"local:app", "local:app"}, {"docker.io/library/app", "app:latest"},
	} {
		destination, err := ParseDestination(test.value, oci.Component)
		if err != nil || destination != (Destination{Transport: "local", Name: test.name}) {
			t.Fatalf("component destination %q = %+v, %v", test.value, destination, err)
		}
		selector, err := localSelector(test.value, oci.Component)
		if err != nil || selector != test.name {
			t.Fatalf("component selector %q = %q, %v", test.value, selector, err)
		}
	}
	pinned := digest.FromString("component").String()
	selector, err := localSelector("app@"+pinned, oci.Component)
	if err != nil || selector != pinned {
		t.Fatalf("named component digest selector = %q, %v", selector, err)
	}
	for _, kind := range []oci.Kind{oci.Image, oci.Component} {
		if _, err := ParseDestination("local:app:dev", kind); err == nil {
			t.Fatalf("invalid multi-colon name accepted for %s", kind)
		}
		destination, err := ParsePushDestination("local:app", kind)
		if err != nil || destination != (Destination{Transport: "registry", Name: "local:app"}) {
			t.Fatalf("ordinary local:app push = %+v, %v", destination, err)
		}
	}
	selector, err = localSelector("local:app", oci.Image)
	if err != nil || selector != "local:app" {
		t.Fatalf("image selector stripped ordinary name: %q, %v", selector, err)
	}
}

func TestCopyRootArchiveAndRegistryWithoutRebuild(t *testing.T) {
	ctx := context.Background()
	options := nativeTestStore(t.TempDir())
	source, root := imageFixture(t, ctx)
	layout := filepath.Join(t.TempDir(), "layout")
	if err := localstore.Put(ctx, layout, source, root); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "image.oci.tar")
	result, err := CopyRoot(ctx, oci.Image, layout, root, Destination{Transport: "oci-archive", Name: archive}, Options{BuildStore: options})
	if err != nil || result != "oci-archive:"+archive+"@"+root.Digest.String() {
		t.Fatalf("copy archive = %q, %v", result, err)
	}
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	var index v1.Index
	for {
		header, err := reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != "index.json" {
			continue
		}
		if err := json.NewDecoder(reader).Decode(&index); err != nil {
			t.Fatal(err)
		}
		break
	}
	if len(index.Manifests) != 1 || index.Manifests[0].Digest != root.Digest {
		t.Fatalf("archive root = %+v, want %s", index.Manifests, root.Digest)
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	registryRef := strings.TrimPrefix(server.URL, "http://") + "/coopr/copy:dev"
	result, err = CopyRoot(ctx, oci.Image, layout, root, Destination{Transport: "registry", Name: registryRef}, Options{BuildStore: options, TLSVerify: new(false)})
	if err != nil || result != strings.TrimSuffix(registryRef, ":dev")+"@"+root.Digest.String() {
		t.Fatalf("copy registry = %q, %v", result, err)
	}
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(ctx, registryRef, v1.Platform{OS: "linux", Architecture: "amd64"}, oci.Image)
	if err != nil || resolved.Root.Digest != root.Digest {
		t.Fatalf("registry graph differs: %+v, %v", resolved, err)
	}
}

func TestCopyRootRegistryUsesCredentialsAndDirectCertLeaf(t *testing.T) {
	ctx := context.Background()
	storeDir := filepath.Join(t.TempDir(), "images")
	source, root := imageFixture(t, ctx)
	if err := localstore.Put(ctx, storeDir, source, root); err != nil {
		t.Fatal(err)
	}
	registryHandler := registry.New()
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "coopr" || password != "secret" {
			writer.Header().Set("WWW-Authenticate", `Basic realm="coopr"`)
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		registryHandler.ServeHTTP(writer, request)
	}))
	defer server.Close()
	certDir := t.TempDir()
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(certDir, "ca.crt"), certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	registryRef := strings.TrimPrefix(server.URL, "https://") + "/coopr/private:test"
	result, err := CopyRoot(ctx, oci.Image, storeDir, root, Destination{Transport: "registry", Name: registryRef}, Options{
		Credentials: "coopr:secret", CertDir: certDir, Retry: 0, RetrySet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSuffix(registryRef, ":test") + "@" + root.Digest.String(); result != want {
		t.Fatalf("private registry result = %q, want %q", result, want)
	}
}

func TestArchiveDestinationInsideStoreUsesNativePolicy(t *testing.T) {
	ctx := context.Background()
	storeDir := filepath.Join(t.TempDir(), "images")
	source, root := imageFixture(t, ctx)
	if err := localstore.Put(ctx, storeDir, source, root); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(storeDir, "index.json")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "store-alias")
	if err := os.Symlink(storeDir, alias); err != nil {
		t.Fatal(err)
	}
	destination, err := ParseDestination("oci-archive:"+filepath.Join(alias, "export.tar")+":app", oci.Image)
	if err != nil {
		t.Fatal(err)
	}
	_, err = CopyRoot(ctx, oci.Image, storeDir, root, destination, Options{})
	if err != nil {
		t.Fatalf("archive export within store = %v", err)
	}
	after, err := os.ReadFile(index)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("store index changed during archive export: %v", err)
	}
}

func TestComponentArchiveProtectsStore(t *testing.T) {
	ctx := context.Background()
	source, root := componentExportFixture(t, ctx)
	blob := filepath.Join("blobs", root.Digest.Algorithm().String(), root.Digest.Encoded())
	for _, reserved := range []string{".", "index.json", "oci-layout", ".coopr-store.lock", ".coopr-activity.lock", "blobs", blob, filepath.Join("blobs", "new", "export.tar")} {
		for _, aliased := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/alias=%t", reserved, aliased), func(t *testing.T) {
				storeDir := filepath.Join(t.TempDir(), "components")
				if err := localstore.Put(ctx, storeDir, source, root, "example:latest"); err != nil {
					t.Fatal(err)
				}
				outputRoot := storeDir
				if aliased {
					outputRoot = filepath.Join(t.TempDir(), "store-alias")
					if err := os.Symlink(storeDir, outputRoot); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(storeDir, ".coopr-activity.lock"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				before := map[string][]byte{}
				for _, name := range []string{"index.json", "oci-layout", ".coopr-store.lock", ".coopr-activity.lock", blob} {
					data, err := os.ReadFile(filepath.Join(storeDir, name))
					if err != nil {
						t.Fatal(err)
					}
					before[name] = data
				}
				destination := Destination{Transport: "oci-archive", Name: filepath.Join(outputRoot, reserved)}
				if _, err := CopyRoot(ctx, oci.Component, storeDir, root, destination, Options{ComponentStoreDir: outputRoot}); err == nil || !strings.Contains(err.Error(), "component store") {
					t.Fatalf("reserved output accepted or wrong error: %v", err)
				}
				for name, data := range before {
					after, err := os.ReadFile(filepath.Join(storeDir, name))
					if err != nil || !bytes.Equal(data, after) {
						t.Fatalf("store file %s changed: %v", name, err)
					}
				}
				store, err := localstore.Open(ctx, storeDir)
				if err != nil {
					t.Fatal(err)
				}
				selected, err := store.Resolve(ctx, "example:latest")
				if err != nil || selected.Digest != root.Digest {
					t.Fatalf("component no longer resolves: %v", err)
				}
				resolver, err := oci.NewResolver(oci.Options{ComponentStoreDir: storeDir})
				if err != nil {
					t.Fatal(err)
				}
				resolved, err := resolver.Resolve(ctx, "example:latest", v1.Platform{OS: "linux", Architecture: "amd64"}, oci.Component)
				if err != nil || resolved.Component == nil {
					t.Fatalf("component metadata no longer readable: %v", err)
				}
			})
		}
	}
}

func TestComponentArchiveExportPlacement(t *testing.T) {
	ctx := context.Background()
	source, root := componentExportFixture(t, ctx)
	storeDir := filepath.Join(t.TempDir(), "components")
	if err := localstore.Put(ctx, storeDir, source, root, "example:latest"); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(storeDir, "index.json")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	for _, transport := range []string{"oci-archive", "oci-dir"} {
		for _, output := range []string{filepath.Join(t.TempDir(), "component.tar"), filepath.Join(storeDir, "exports", transport+".tar")} {
			if _, err := CopyRoot(ctx, oci.Component, storeDir, root, Destination{Transport: transport, Name: output}, Options{ComponentStoreDir: storeDir}); err != nil {
				t.Fatalf("valid export %s: %v", output, err)
			}
			file, err := os.Open(output)
			if err != nil {
				t.Fatal(err)
			}
			header, readErr := tar.NewReader(file).Next()
			closeErr := file.Close()
			if readErr != nil || closeErr != nil || header == nil {
				t.Fatalf("export not an archive: %v, %v", readErr, closeErr)
			}
		}
	}
	// Atomic archive installation replaces this link itself, not its target.
	leaf := filepath.Join(t.TempDir(), "export.tar")
	if err := os.Symlink(index, leaf); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyRoot(ctx, oci.Component, storeDir, root, Destination{Transport: "oci-archive", Name: leaf}, Options{ComponentStoreDir: storeDir}); err != nil {
		t.Fatalf("leaf symlink export: %v", err)
	}
	after, err := os.ReadFile(index)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("leaf symlink target changed: %v", err)
	}
	// A temporary source must not be able to overwrite the persistent store.
	sourcePath := t.TempDir()
	if err := localstore.Put(ctx, sourcePath, source, root); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyRoot(ctx, oci.Component, sourcePath, root, Destination{Transport: "oci-archive", Name: index}, Options{ComponentStoreDir: storeDir}); err == nil || !strings.Contains(err.Error(), "component store") {
		t.Fatalf("persistent store overwrite: %v", err)
	}
}

func TestComponentArchiveProtectsSymlinkedStoreData(t *testing.T) {
	ctx := context.Background()
	source, root := componentExportFixture(t, ctx)
	storeDir := filepath.Join(t.TempDir(), "components")
	if err := localstore.Put(ctx, storeDir, source, root, "example:latest"); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	for _, name := range []string{"index.json", "blobs"} {
		original, moved := filepath.Join(storeDir, name), filepath.Join(external, name)
		if err := os.Rename(original, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, original); err != nil {
			t.Fatal(err)
		}
	}
	for _, output := range []string{filepath.Join(external, "index.json"), filepath.Join(external, "blobs", root.Digest.Algorithm().String(), root.Digest.Encoded()), filepath.Join(external, "blobs", "new", "export.tar")} {
		if _, err := CopyRoot(ctx, oci.Component, storeDir, root, Destination{Transport: "oci-archive", Name: output}, Options{ComponentStoreDir: storeDir}); err == nil || !strings.Contains(err.Error(), "component store") {
			t.Fatalf("symlinked store data overwrite at %s: %v", output, err)
		}
	}
	resolver, err := oci.NewResolver(oci.Options{ComponentStoreDir: storeDir})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(ctx, "example:latest", v1.Platform{OS: "linux", Architecture: "amd64"}, oci.Component)
	if err != nil || resolved.Component == nil {
		t.Fatalf("symlinked store unreadable: %v", err)
	}
}

func TestComponentArchiveProtectsStoreAfterSymlinkDotDot(t *testing.T) {
	ctx := context.Background()
	source, root := componentExportFixture(t, ctx)
	base := t.TempDir()
	storeDir := filepath.Join(base, "components")
	if err := localstore.Put(ctx, storeDir, source, root, "example:latest"); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(storeDir, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(child, alias); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(storeDir, "index.json")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	// Join/Abs would clean away .. before the kernel resolves the symlink.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, alias)
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{alias, relative} {
		output := prefix + "/../index.json"
		if _, err := CopyRoot(ctx, oci.Component, storeDir, root, Destination{Transport: "oci-archive", Name: output}, Options{ComponentStoreDir: storeDir}); err == nil || !strings.Contains(err.Error(), "component store") {
			t.Fatalf("symlink-dotdot store overwrite: %v", err)
		}
		after, err := os.ReadFile(index)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("component index changed: %v", err)
		}
	}
	// A safe sibling still follows the same raw path semantics as Rename.
	output := alias + "/../export.tar"
	if _, err := CopyRoot(ctx, oci.Component, storeDir, root, Destination{Transport: "oci-archive", Name: output}, Options{ComponentStoreDir: storeDir}); err != nil {
		t.Fatalf("safe symlink-dotdot export: %v", err)
	}
	if _, err := os.Stat(filepath.Join(storeDir, "export.tar")); err != nil {
		t.Fatalf("export path semantics changed: %v", err)
	}
}

func componentExportFixture(t *testing.T, ctx context.Context) (*orasoci.Store, v1.Descriptor) {
	t.Helper()
	def, err := definition.Parse(strings.NewReader("extend { env GREETING=hello }"))
	if err != nil {
		t.Fatal(err)
	}
	publication, err := planner.Create(def, planner.Options{Mode: planner.Publish, Platform: "linux/amd64"})
	if err != nil {
		t.Fatal(err)
	}
	layout := t.TempDir()
	root, err := oci.WriteComponentLayout(ctx, layout, oci.ComponentMetadata{Version: oci.ComponentVersion, Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Component: *publication.Component}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	return store, root
}

func imageFixture(t *testing.T, ctx context.Context) (*orasoci.Store, v1.Descriptor) {
	t.Helper()
	store, err := orasoci.NewWithContext(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configBytes := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	config := v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: digest.FromBytes(configBytes), Size: int64(len(configBytes))}
	if err := store.Push(ctx, config, bytes.NewReader(configBytes)); err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config})
	if err != nil {
		t.Fatal(err)
	}
	root := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromBytes(manifestBytes), Size: int64(len(manifestBytes))}
	if err := store.Push(ctx, root, bytes.NewReader(manifestBytes)); err != nil {
		t.Fatal(err)
	}
	return store, root
}
