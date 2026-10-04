package oci

import (
	"archive/tar"
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
	"strings"
	"testing"
	"time"

	"coopr/internal/definition"
	"coopr/internal/planner"
	"github.com/google/go-containerregistry/pkg/registry"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

func packageTar(t *testing.T, contents string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	header := &tar.Header{Name: "src", Mode: 0644, Size: int64(len(contents)), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg}
	if err := w.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, contents); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func packageImageConfig(t *testing.T, platform v1.Platform, snapshot []byte) json.RawMessage {
	t.Helper()
	image := v1.Image{Platform: platform, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{digest.FromBytes(snapshot)}}, Config: v1.ImageConfig{Env: []string{"GREETING=hello"}, WorkingDir: "/opt"}}
	data, err := json.Marshal(image)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["futureField"] = json.RawMessage(`{"retain":true}`)
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertPackageSnapshot(t *testing.T, snapshot []byte, contents string, config json.RawMessage, platform v1.Platform) {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(snapshot))
	header, err := reader.Next()
	if err != nil || header.Name != "src" || header.Mode != 0644 || header.Size != int64(len(contents)) {
		t.Fatalf("invalid package tar header: %+v, %v", header, err)
	}
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != contents {
		t.Fatalf("invalid package file %q: %v", data, err)
	}
	if _, err := reader.Next(); err != io.EOF {
		t.Fatalf("unexpected package tar entry: %v", err)
	}
	var image v1.Image
	if err := json.Unmarshal(config, &image); err != nil {
		t.Fatal(err)
	}
	if image.OS != platform.OS || image.Architecture != platform.Architecture || image.RootFS.Type != "layers" || len(image.RootFS.DiffIDs) != 1 || image.RootFS.DiffIDs[0] != digest.FromBytes(snapshot) || image.Config.WorkingDir != "/opt" {
		t.Fatalf("package image config does not describe snapshot/platform: %+v", image)
	}
}

func testRepository(t *testing.T, handler http.Handler) (*httptest.Server, *remote.Repository, *Resolver, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	name := strings.TrimPrefix(server.URL, "http://") + "/coopr/test"
	repo, err := remote.NewRepository(name)
	if err != nil {
		t.Fatal(err)
	}
	repo.PlainHTTP = true
	resolver, err := NewResolver(Options{PlainHTTP: true, ImageStoreDir: filepath.Join(t.TempDir(), "images")})
	if err != nil {
		t.Fatal(err)
	}
	return server, repo, resolver, name
}

func TestResolveRemoteImageFromRegistry(t *testing.T) {
	_, repo, _, name := testRepository(t, registry.New())
	ctx := context.Background()
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	root, _ := imageManifest(t, repo, platform, "remote layer")
	if err := repo.Tag(ctx, root, "remote"); err != nil {
		t.Fatal(err)
	}
	imageStoreDir := filepath.Join(t.TempDir(), "images")
	resolver, err := NewResolver(Options{PlainHTTP: true, ImageStoreDir: imageStoreDir})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(ctx, name+":remote", platform, Image)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Root.Digest != root.Digest || resolved.Repository == "local" {
		t.Fatalf("unexpected registry fallback resolution: %+v", resolved)
	}
}

func TestResolveRegistryImageDoesNotCreateAbsentLocalStore(t *testing.T) {
	_, repo, _, name := testRepository(t, registry.New())
	ctx := context.Background()
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	root, _ := imageManifest(t, repo, platform, "remote layer")
	if err := repo.Tag(ctx, root, "remote"); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(t.TempDir(), "missing", "images")
	resolver, err := NewResolver(Options{PlainHTTP: true, ImageStoreDir: storeDir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(ctx, name+":remote", platform, Image); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(storeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("registry resolution created local store %q: %v", storeDir, err)
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (fn transportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestPlainHTTPIsSelectedPerRegistryAndPreservesHTTPSAuth(t *testing.T) {
	const secureHost = "secure-registry.example"
	const insecureHost = "private-http.example:5000"
	configDir := t.TempDir()
	config := `{"auths":{"secure-registry.example":{"auth":"dGVzdGVyOnNlY3JldA=="}}}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", configDir)
	var challenged, authorized, usedHTTP bool
	client := &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		status := http.StatusNotFound
		header := make(http.Header)
		switch req.URL.Host {
		case secureHost:
			if req.URL.Scheme != "https" {
				t.Errorf("secure registry used %s", req.URL.Scheme)
			}
			username, password, ok := req.BasicAuth()
			if !ok {
				challenged = true
				status = http.StatusUnauthorized
				header.Set("WWW-Authenticate", `Basic realm="registry"`)
			} else if username == "tester" && password == "secret" {
				authorized = true
			} else {
				t.Errorf("unexpected HTTPS registry credential")
			}
		case insecureHost:
			if req.URL.Scheme != "http" {
				t.Errorf("explicit HTTP registry used %s", req.URL.Scheme)
			}
			usedHTTP = true
		default:
			t.Errorf("unexpected registry %q", req.URL.Host)
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader("not found")), Request: req}, nil
	})}
	resolver, err := NewResolver(Options{PlainHTTP: true, PlainHTTPRegistries: []string{insecureHost}, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	for _, reference := range []string{secureHost + "/team/image:latest", insecureHost + "/team/image:latest"} {
		if _, err := resolver.Resolve(context.Background(), reference, platform, Component); err == nil {
			t.Fatalf("synthetic missing registry object %q unexpectedly resolved", reference)
		}
	}
	if !challenged || !authorized || !usedHTTP {
		t.Fatalf("mixed registry transport/auth incomplete: challenge=%v authorized=%v HTTP=%v", challenged, authorized, usedHTTP)
	}
}

func TestPlainHTTPRegistryAuthorityValidation(t *testing.T) {
	for _, invalid := range []string{"", "https://registry.example", "registry.example/team", "user@registry.example", "*.example", "registry.example:bad", "registry.example:0", "registry.example:65536", "::1", "::1:5000"} {
		if _, err := NewResolver(Options{PlainHTTPRegistries: []string{invalid}}); err == nil {
			t.Errorf("accepted invalid registry authority %q", invalid)
		}
	}
	for _, valid := range []string{"registry.example", "registry.example:5000", "localhost:5000", "[::1]", "[::1]:5000"} {
		if _, err := NewResolver(Options{PlainHTTPRegistries: []string{valid}}); err != nil {
			t.Errorf("rejected valid registry authority %q: %v", valid, err)
		}
	}
}

func TestPlainHTTPLoopbackAuthorities(t *testing.T) {
	r, err := NewResolver(Options{PlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, authority := range []string{"localhost", "localhost:5000", "registry.localhost:5000", "127.0.0.1:5000", "[::1]", "[::1]:5000"} {
		if !r.usePlainHTTP(authority) {
			t.Errorf("loopback registry %q did not use HTTP", authority)
		}
	}
	for _, authority := range []string{"docker.io", "registry.example:5000", "10.0.0.5:5000"} {
		if r.usePlainHTTP(authority) {
			t.Errorf("nonloopback registry %q used HTTP", authority)
		}
	}
}

func TestValidateComponentReferenceLocalSyntax(t *testing.T) {
	localDigest := digest.FromString("component").String()
	valid := []string{
		"local:stable",
		localDigest,
		"registry.example/team/component:stable",
		"registry.example/team/component@" + localDigest,
	}
	for _, reference := range valid {
		if err := ValidateComponentReference(reference); err != nil {
			t.Errorf("rejected valid component reference %q: %v", reference, err)
		}
	}

	invalid := []string{
		"sha256:not-a-digest",
	}
	for _, reference := range invalid {
		err := ValidateComponentReference(reference)
		if err == nil {
			t.Errorf("accepted invalid component reference %q", reference)
		}
	}
}

func push(t *testing.T, repo *remote.Repository, mediaType string, value []byte) v1.Descriptor {
	t.Helper()
	desc := Descriptor(mediaType, value)
	if err := repo.Push(context.Background(), desc, bytes.NewReader(value)); err != nil {
		t.Fatal(err)
	}
	return desc
}

func pushManifest(t *testing.T, repo *remote.Repository, manifest v1.Manifest, tag string) v1.Descriptor {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	desc := Descriptor(v1.MediaTypeImageManifest, data)
	if err := repo.PushReference(context.Background(), desc, bytes.NewReader(data), tag); err != nil {
		t.Fatal(err)
	}
	return desc
}

func imageManifest(t *testing.T, repo *remote.Repository, platform v1.Platform, payload string) (v1.Descriptor, v1.Descriptor) {
	t.Helper()
	layer := push(t, repo, v1.MediaTypeImageLayer, []byte(payload))
	configBytes, err := json.Marshal(v1.Image{Platform: platform, RootFS: v1.RootFS{Type: "layers"}})
	if err != nil {
		t.Fatal(err)
	}
	config := push(t, repo, v1.MediaTypeImageConfig, configBytes)
	return pushManifest(t, repo, VersionedManifest(config, []v1.Descriptor{layer}, ""), "by-digest"), layer
}

func pushIndex(t *testing.T, repo *remote.Repository, tag string, manifests ...v1.Descriptor) v1.Descriptor {
	t.Helper()
	index := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: manifests}
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	desc := Descriptor(v1.MediaTypeImageIndex, data)
	if err := repo.PushReference(context.Background(), desc, bytes.NewReader(data), tag); err != nil {
		t.Fatal(err)
	}
	return desc
}

func TestResolveIndexByDigestAndDownload(t *testing.T) {
	_, repo, resolver, name := testRepository(t, registry.New())
	amd := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm := v1.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}
	amdManifest, amdLayer := imageManifest(t, repo, amd, "amd64 layer")
	armManifest, _ := imageManifest(t, repo, arm, "arm/v7 layer")
	amdManifest.Platform = &amd
	armManifest.Platform = &arm
	root := pushIndex(t, repo, "multi", amdManifest, armManifest)
	ctx := context.Background()
	resolved, err := resolver.Resolve(ctx, name+":multi", arm, Image)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Root.Digest != root.Digest || resolved.Selected.Digest != armManifest.Digest {
		t.Fatalf("selected wrong descriptors: %+v", resolved)
	}
	newManifest, _ := imageManifest(t, repo, amd, "new tag target")
	newManifest.Platform = &amd
	pushIndex(t, repo, "multi", newManifest)
	pinned, err := resolver.Resolve(ctx, name+"@"+root.Digest.String(), arm, Image)
	if err != nil || pinned.Selected.Digest != armManifest.Digest {
		t.Fatalf("digest resolution changed after tag move: %+v, %v", pinned, err)
	}
	if _, err := resolver.Resolve(ctx, name+":multi", arm, Image); err == nil {
		t.Fatal("tag update did not change platform selection")
	}
	file := filepath.Join(t.TempDir(), "layer")
	amdResolved, err := resolver.Resolve(ctx, name+"@"+amdManifest.Digest.String(), amd, Image)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolver.Download(ctx, amdResolved, amdLayer, file); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "amd64 layer" {
		t.Fatalf("downloaded %q: %v", data, err)
	}
	if err := resolver.Download(ctx, pinned, amdLayer, file); err == nil {
		t.Fatal("accepted unrelated layer")
	}
}

func TestIndexAmbiguityAndWrongConfig(t *testing.T) {
	_, repo, resolver, name := testRepository(t, registry.New())
	amd := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm := v1.Platform{OS: "linux", Architecture: "arm64"}
	manifest, _ := imageManifest(t, repo, arm, "layer")
	manifest.Platform = &amd
	root := pushIndex(t, repo, "wrong-config", manifest)
	_, err := resolver.Resolve(context.Background(), name+"@"+root.Digest.String(), amd, Image)
	if err == nil || !strings.Contains(err.Error(), "config platform") {
		t.Fatalf("wrong image config accepted: %v", err)
	}
	pushIndex(t, repo, "duplicate", manifest, manifest)
	_, err = resolver.Resolve(context.Background(), name+":duplicate", amd, Image)
	if err == nil || !strings.Contains(err.Error(), "2 manifests") {
		t.Fatalf("duplicate platform accepted: %v", err)
	}
	_, err = resolver.Resolve(context.Background(), name+":duplicate", arm, Image)
	if err == nil || !strings.Contains(err.Error(), "0 manifests") {
		t.Fatalf("missing platform accepted: %v", err)
	}
}

func TestComponentArtifactAndBlobVerification(t *testing.T) {
	_, repo, resolver, name := testRepository(t, registry.New())
	def, err := definition.Parse(strings.NewReader("package as=\"pkg\"\nextend as=\"base\"\narg \"channel\"\nenv channel=\"${channel}\"\ncopy \"/src\" \"/dst\" from=\"pkg\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	platforms := []v1.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}}
	var manifests []v1.Descriptor
	for _, platform := range platforms {
		packageData := packageTar(t, "component package "+platform.Architecture)
		packageDesc := push(t, repo, ComponentPackageType, packageData)
		publication, err := planner.Create(def, planner.Options{Mode: planner.Publish, Platform: "linux/" + platform.Architecture})
		if err != nil {
			t.Fatal(err)
		}
		packageConfig := packageImageConfig(t, platform, packageData)
		meta := ComponentMetadata{Version: ComponentVersion, Platform: platform, Component: *publication.Component, Packages: []Package{{Stage: "pkg", Descriptor: packageDesc, Config: packageConfig}}}
		configData, _ := json.Marshal(meta)
		config := push(t, repo, ComponentConfigType, configData)
		manifest := pushManifest(t, repo, VersionedManifest(config, []v1.Descriptor{packageDesc}, ComponentArtifactType), "component-"+platform.Architecture)
		manifest.Platform = &platform
		manifests = append(manifests, manifest)
	}
	root := pushIndex(t, repo, "component-multi", manifests...)
	ref := name + "@" + root.Digest.String()
	for _, platform := range platforms {
		resolved, err := resolver.Resolve(context.Background(), ref, platform, Component)
		if err != nil || resolved.Component == nil || resolved.Component.Component.Output != "1" {
			t.Fatalf("component resolution for %s: %+v, %v", platform.Architecture, resolved, err)
		}
		invoked, err := planner.Instantiate(&resolved.Component.Component, planner.Options{Arguments: map[string]string{"channel": "preview"}})
		if err != nil || len(invoked.Stages) != 2 || invoked.Stages[0].Kind != "package-input" {
			t.Fatalf("published %s artifact cannot invoke: %+v, %v", platform.Architecture, invoked, err)
		}
		if !bytes.Contains(resolved.Component.Packages[0].Config, []byte(`"futureField"`)) {
			t.Fatalf("package config lost unknown field: %s", resolved.Component.Packages[0].Config)
		}
		file := filepath.Join(t.TempDir(), "package")
		if err := resolver.Download(context.Background(), resolved, resolved.Component.Packages[0].Descriptor, file); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		assertPackageSnapshot(t, data, "component package "+platform.Architecture, resolved.Component.Packages[0].Config, platform)
		if platform.Architecture == "arm64" {
			variant := v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
			aliased, err := resolver.Resolve(context.Background(), ref, variant, Component)
			if err != nil || aliased.Selected.Digest != resolved.Selected.Digest {
				t.Fatalf("arm64/v8 alias failed: %+v, %v", aliased, err)
			}
			if _, err := planner.Instantiate(&aliased.Component.Component, planner.Options{Platform: "linux/arm64/v8", Arguments: map[string]string{"channel": "preview"}}); err != nil {
				t.Fatalf("arm64/v8 invocation failed: %v", err)
			}
		}
	}
	if _, err := resolver.Resolve(context.Background(), ref, v1.Platform{OS: "linux", Architecture: "riscv64"}, Component); err == nil {
		t.Fatal("component resolved for unpublished platform")
	}
}

func TestCorruptAndMissingBlob(t *testing.T) {
	base := registry.New()
	corrupt := false
	missing := false
	corruptManifest := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/manifests/sha256:") && corruptManifest {
			w.Header().Set("Content-Type", v1.MediaTypeImageManifest)
			_, _ = io.WriteString(w, `{"schemaVersion":2}`)
			return
		}
		if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/blobs/") {
			if missing {
				http.NotFound(w, req)
				return
			}
			if corrupt {
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = io.WriteString(w, "corrupted")
				return
			}
		}
		base.ServeHTTP(w, req)
	})
	_, repo, resolver, name := testRepository(t, handler)
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	manifest, layer := imageManifest(t, repo, platform, "actual layer")
	file := filepath.Join(t.TempDir(), "layer")
	ref := name + "@" + manifest.Digest.String()
	resolved, err := resolver.Resolve(context.Background(), ref, platform, Image)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	corrupt = true
	if err := resolver.Download(context.Background(), resolved, layer, file); err == nil {
		t.Fatal("corrupt blob accepted")
	}
	if data, _ := os.ReadFile(file); string(data) != "preserve" {
		t.Fatalf("failed download changed destination: %q", data)
	}
	corrupt = false
	missing = true
	if err := resolver.Download(context.Background(), resolved, layer, file); err == nil {
		t.Fatal("missing blob accepted")
	}
	missing = false
	corruptManifest = true
	if _, err := resolver.Resolve(context.Background(), ref, platform, Image); err == nil {
		t.Fatal("corrupt manifest accepted")
	}
}

func TestAuthUsesDockerCredentials(t *testing.T) {
	base := registry.New()
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		username, password, ok := req.BasicAuth()
		if !ok || username != "tester" || password != "secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		base.ServeHTTP(w, req)
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	dir := t.TempDir()
	config := fmt.Sprintf(`{"auths":{"%s":{"auth":"%s"}}}`, host, "dGVzdGVyOnNlY3JldA==")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)
	repo, _ := remote.NewRepository(host + "/coopr/auth")
	repo.PlainHTTP = true
	// Publishing uses the same Docker credential store as resolving.
	store, err := NewResolver(Options{PlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	refForPublish, err := ParseReference(host + "/coopr/auth:private")
	if err != nil {
		t.Fatal(err)
	}
	repo, err = store.repository(refForPublish)
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	manifest, _ := imageManifest(t, repo, platform, "private")
	ref := host + "/coopr/auth@" + manifest.Digest.String()
	resolved, err := store.Resolve(context.Background(), ref, platform, Image)
	if err != nil || resolved == nil {
		t.Fatalf("Docker credential auth failed: %v", err)
	}
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	unauthorized, err := NewResolver(Options{PlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unauthorized.Resolve(context.Background(), ref, platform, Image); err == nil {
		t.Fatal("missing credentials succeeded")
	}
}

func TestImageResolutionUsesExplicitCredentials(t *testing.T) {
	base := registry.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		username, password, ok := req.BasicAuth()
		if !ok || username != "tester" || password != "secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		base.ServeHTTP(w, req)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	repo, err := remote.NewRepository(host + "/coopr/explicit")
	if err != nil {
		t.Fatal(err)
	}
	repo.PlainHTTP = true
	repo.Client = &auth.Client{Credential: auth.StaticCredential(host, auth.Credential{Username: "tester", Password: "secret"})}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	manifest, _ := imageManifest(t, repo, platform, "explicit credentials")
	if err := repo.Tag(context.Background(), manifest, "latest"); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(Options{PlainHTTPRegistries: []string{host}, Credentials: "tester:secret"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(context.Background(), host+"/coopr/explicit:latest", platform, Image)
	if err != nil || resolved.Selected.Digest != manifest.Digest {
		t.Fatalf("explicit credential resolution = %+v, %v", resolved, err)
	}
}

func TestParseReferenceCompatibility(t *testing.T) {
	dgst := digest.FromString("reference compatibility").String()
	for input, want := range map[string]string{
		"shared.coopr":                             "docker.io/library/shared.coopr:latest",
		"shared.coopr:v1":                          "docker.io/library/shared.coopr:v1",
		"shared.coopr@" + dgst:                     "docker.io/library/shared.coopr@" + dgst,
		"team/shared.coopr":                        "docker.io/team/shared.coopr:latest",
		"team/shared.coopr:v1":                     "docker.io/team/shared.coopr:v1",
		"debian":                                   "docker.io/library/debian:latest",
		"debian:bookworm":                          "docker.io/library/debian:bookworm",
		"debian@" + dgst:                           "docker.io/library/debian@" + dgst,
		"docker.io/shared.coopr":                   "docker.io/library/shared.coopr:latest",
		"registry.example/team/shared.coopr:v1":    "registry.example/team/shared.coopr:v1",
		"registry.example:5000/shared.coopr":       "registry.example:5000/shared.coopr:latest",
		"localhost:5000/shared.coopr":              "localhost:5000/shared.coopr:latest",
		"[::1]:5000/team/shared.coopr:v1":          "[::1]:5000/team/shared.coopr:v1",
		"[2001:db8::1]/team/shared.coopr@" + dgst:  "[2001:db8::1]/team/shared.coopr@" + dgst,
		"registry.example/shared.coopr:v1@" + dgst: "registry.example/shared.coopr@" + dgst,
	} {
		t.Run(input, func(t *testing.T) {
			ref, err := ParseReference(input)
			if err != nil || ref.String() != want {
				t.Fatalf("got %q, %v; want %q", ref.String(), err, want)
			}
		})
	}
}

func TestDockerReferenceAndPlatformCompatibility(t *testing.T) {
	for input, want := range map[string]string{
		"debian":           "docker.io/library/debian:latest",
		"debian:bookworm":  "docker.io/library/debian:bookworm",
		"team/app":         "docker.io/team/app:latest",
		"ghcr.io/team/app": "ghcr.io/team/app:latest",
	} {
		ref, err := ParseReference(input)
		if err != nil || ref.String() != want {
			t.Fatalf("%q: got %q, %v; want %q", input, ref.String(), err, want)
		}
	}
	if !platformEqual(v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, v1.Platform{OS: "linux", Architecture: "arm64"}) {
		t.Fatal("arm64/v8 and arm64 should select the same image")
	}
	_, repo, resolver, name := testRepository(t, registry.New())
	platform := v1.Platform{OS: "linux", Architecture: "arm64"}
	layer := push(t, repo, "application/vnd.docker.image.rootfs.diff.tar.gzip", []byte("docker layer"))
	configBytes, _ := json.Marshal(v1.Image{Platform: platform, RootFS: v1.RootFS{Type: "layers"}})
	config := push(t, repo, dockerConfigType, configBytes)
	manifest := VersionedManifest(config, []v1.Descriptor{layer}, "")
	manifest.MediaType = dockerManifestType
	manifestBytes, _ := json.Marshal(manifest)
	manifestDesc := Descriptor(dockerManifestType, manifestBytes)
	if err := repo.PushReference(context.Background(), manifestDesc, bytes.NewReader(manifestBytes), "docker-manifest"); err != nil {
		t.Fatal(err)
	}
	manifestDesc.Platform = &v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	index := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: dockerIndexType, Manifests: []v1.Descriptor{manifestDesc}}
	indexBytes, _ := json.Marshal(index)
	indexDesc := Descriptor(dockerIndexType, indexBytes)
	if err := repo.PushReference(context.Background(), indexDesc, bytes.NewReader(indexBytes), "docker"); err != nil {
		t.Fatal(err)
	}
	result, err := resolver.Resolve(context.Background(), name+":docker", platform, Image)
	if err != nil || result.Selected.Digest != manifestDesc.Digest {
		t.Fatalf("Docker schema2 image resolution: %+v, %v", result, err)
	}
}

func TestNormalizePullPolicy(t *testing.T) {
	for _, test := range []struct {
		policy string
		pull   bool
		want   PullPolicy
	}{
		{"", false, PullMissing},
		{"missing", false, PullMissing},
		{"always", false, PullAlways},
		{"missing", true, PullAlways},
		{"", true, PullAlways},
		{"never", false, PullNever},
		{"newer", false, PullNewer},
		{"ifnewer", false, PullNewer},
	} {
		got, err := NormalizePullPolicy(test.policy, test.pull)
		if err != nil || got != test.want {
			t.Errorf("NormalizePullPolicy(%q, %v) = %q, %v; want %q", test.policy, test.pull, got, err, test.want)
		}
	}
	for _, test := range []struct {
		policy string
		pull   bool
	}{{"sometimes", false}, {"never", true}, {"newer", true}} {
		if _, err := NormalizePullPolicy(test.policy, test.pull); err == nil {
			t.Errorf("NormalizePullPolicy(%q, %v) accepted invalid policy", test.policy, test.pull)
		}
	}
}

func TestRegistryOptionsReturnsIndependentWorkerCopy(t *testing.T) {
	resolver, err := NewResolver(Options{
		PlainHTTPRegistries: []string{"registry.example:5000"}, PullPolicy: "always",
		Credentials: "user:pass", Retry: 0, RetrySet: true, RetryDelay: 2 * time.Second,
		DecryptionKeys: []string{"provider:key"}, SignaturePolicyPath: "/tmp/policy.json",
		ImageStoreDir: t.TempDir(), ComponentStoreDir: t.TempDir(),
		NativeStore: storage.StoreOptions{RunRoot: "/run/coopr", GraphRoot: "/var/lib/coopr", ImageStore: "/var/lib/coopr-images", GraphDriverName: "vfs", GraphDriverOptions: []string{"vfs.ignore_chown_errors=true"}, TransientStore: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	options := resolver.RegistryOptions()
	if !options.RetrySet || options.Retry != 0 || options.PullPolicy != "always" || options.Credentials != "user:pass" || options.SignaturePolicyPath != "/tmp/policy.json" {
		t.Fatalf("worker registry options = %+v", options)
	}
	options.PlainHTTPRegistries[0] = "changed.example"
	options.DecryptionKeys[0] = "changed"
	options.NativeStore.GraphDriverOptions[0] = "changed"
	again := resolver.RegistryOptions()
	if again.PlainHTTPRegistries[0] != "registry.example:5000" || again.DecryptionKeys[0] != "provider:key" || again.NativeStore.GraphDriverOptions[0] != "vfs.ignore_chown_errors=true" {
		t.Fatalf("worker registry options alias resolver state: %+v", again)
	}
	if got := resolver.NativeStoreOptions(); got.GraphRoot != "/var/lib/coopr" || got.ImageStore != "/var/lib/coopr-images" || !got.TransientStore {
		t.Fatalf("native store options = %+v", got)
	}
}

func TestMissingDockerCredentialHelperFailsClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"credsStore":"coopr-test-helper-that-does-not-exist"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)
	resolver, err := NewResolver(Options{PlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(server.URL, "http://")
	if _, err := resolver.Resolve(context.Background(), host+"/coopr/private:latest", v1.Platform{OS: "linux", Architecture: "amd64"}, Image); err == nil {
		t.Fatal("missing credential helper unexpectedly authenticated")
	}
}
