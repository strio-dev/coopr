package oci

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

	"coopr/internal/componentstore"
	"coopr/internal/definition"
	"coopr/internal/planner"
	"github.com/google/go-containerregistry/pkg/registry"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func publicationMetadata(t *testing.T, data []byte, stage bool) (ComponentMetadata, map[string]string) {
	t.Helper()
	source := "extend as=\"base\"\nenv GREETING=\"hello\"\n"
	if stage {
		source = "package as=\"pkg\"\nextend as=\"base\"\ncopy \"/src\" \"/dst\" from=\"pkg\"\n"
	}
	def, err := definition.Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	publication, err := planner.Create(def, planner.Options{Mode: planner.Publish, Platform: "linux/amd64"})
	if err != nil {
		t.Fatal(err)
	}
	meta := ComponentMetadata{Version: ComponentVersion, Platform: platform, Component: *publication.Component}
	paths := map[string]string{}
	if stage {
		desc := Descriptor(ComponentPackageType, data)
		meta.Packages = []Package{{Stage: "pkg", Descriptor: desc, Config: packageImageConfig(t, platform, data)}}
		path := filepath.Join(t.TempDir(), "package.tar")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		paths["pkg"] = path
	}
	return meta, paths
}

func TestPublishComponentRoundTripAndFailedValidationKeepsTag(t *testing.T) {
	_, _, resolver, name := testRepository(t, registry.New())
	ctx := context.Background()
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	meta, paths := publicationMetadata(t, packageTar(t, "first"), true)
	root, err := resolver.PublishComponent(ctx, name+":stable", meta, paths)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolver.Resolve(ctx, name+":stable", platform, Component)
	if err != nil || got.Root.Digest != root.Digest || len(got.Component.Packages) != 1 {
		t.Fatalf("published component did not resolve: %+v, %v", got, err)
	}
	file := filepath.Join(t.TempDir(), "downloaded.tar")
	if err := resolver.Download(ctx, got, got.Component.Packages[0].Descriptor, file); err != nil {
		t.Fatal(err)
	}
	read, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(read, packageTar(t, "first")) {
		t.Fatalf("package roundtrip failed: %v", err)
	}
	if _, err := planner.Instantiate(&got.Component.Component, planner.Options{Platform: "linux/amd64"}); err != nil {
		t.Fatalf("published component cannot be invoked: %v", err)
	}
	if _, err := resolver.PublishComponent(ctx, name+"@"+digest.FromString("wrong").String(), meta, paths); err == nil {
		t.Fatal("mismatched digest target accepted")
	}
	badCases := []struct {
		name string
		edit func(*ComponentMetadata, map[string]string)
	}{
		{"corrupt package", func(_ *ComponentMetadata, paths map[string]string) {
			_ = os.WriteFile(paths["pkg"], []byte("wrong"), 0600)
		}},
		{"missing package", func(_ *ComponentMetadata, paths map[string]string) { _ = os.Remove(paths["pkg"]) }},
		{"wrong metadata", func(m *ComponentMetadata, _ map[string]string) { m.Version = "invalid" }},
		{"wrong package config", func(m *ComponentMetadata, _ map[string]string) {
			m.Packages[0].Config = json.RawMessage(`{"os":"linux","architecture":"arm64"}`)
		}},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			candidate, candidatePaths := publicationMetadata(t, packageTar(t, "replacement"), true)
			tc.edit(&candidate, candidatePaths)
			if _, err := resolver.PublishComponent(ctx, name+":stable", candidate, candidatePaths); err == nil {
				t.Fatal("invalid publication succeeded")
			}
			current, err := resolver.Resolve(ctx, name+":stable", platform, Component)
			if err != nil || current.Root.Digest != root.Digest {
				t.Fatalf("failed publication moved tag: %+v, %v", current, err)
			}
		})
	}
}

func TestLocalComponentTagMovesWhileDigestPinsRemainResolvable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	resolver, err := NewResolver(Options{ComponentStoreDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	var roots []v1.Descriptor
	for _, value := range []string{"first", "second"} {
		meta, paths := publicationMetadata(t, packageTar(t, value), true)
		layout := t.TempDir()
		root, err := WriteComponentLayout(ctx, layout, meta, paths)
		if err != nil {
			t.Fatal(err)
		}
		source, err := orasoci.NewWithContext(ctx, layout)
		if err != nil {
			t.Fatal(err)
		}
		if err := componentstore.Put(ctx, dir, source, root, "stable"); err != nil {
			t.Fatal(err)
		}
		roots = append(roots, root)
	}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	latest, err := resolver.Resolve(ctx, "local:stable", platform, Component)
	if err != nil || latest.Root.Digest != roots[1].Digest {
		t.Fatalf("local tag did not move: %+v, %v", latest, err)
	}
	ref := roots[0].Digest.String()
	pinned, err := resolver.Resolve(ctx, ref, platform, Component)
	if err != nil || pinned.Root.Digest != roots[0].Digest {
		t.Fatalf("old local digest %q did not resolve: %+v, %v", ref, pinned, err)
	}
	path := filepath.Join(t.TempDir(), "package.tar")
	if err := resolver.Download(ctx, pinned, pinned.Layers[0], path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, packageTar(t, "first")) {
		t.Fatalf("old local package bytes changed: %v", err)
	}
}

func TestPublishComponentEmptyPackageAndNoPackage(t *testing.T) {
	_, _, resolver, name := testRepository(t, registry.New())
	for _, tc := range []struct {
		name  string
		stage bool
	}{{"empty package", true}, {"no package", false}} {
		t.Run(tc.name, func(t *testing.T) {
			meta, paths := publicationMetadata(t, nil, tc.stage)
			ref := name + ":" + strings.ReplaceAll(tc.name, " ", "-")
			root, err := resolver.PublishComponent(context.Background(), ref, meta, paths)
			if err != nil {
				t.Fatal(err)
			}
			got, err := resolver.Resolve(context.Background(), ref, meta.Platform, Component)
			if err != nil || got.Root.Digest != root.Digest || len(got.Layers) != len(meta.Packages) {
				t.Fatalf("roundtrip failed: %+v, %v", got, err)
			}
			if tc.stage {
				path := filepath.Join(t.TempDir(), "empty.tar")
				if err := resolver.Download(context.Background(), got, got.Layers[0], path); err != nil {
					t.Fatal(err)
				}
				if info, err := os.Stat(path); err != nil || info.Size() != 0 {
					t.Fatalf("empty package changed: %v, %v", info, err)
				}
			}
		})
	}
}

func imageArchive(t *testing.T, corrupt bool) (string, v1.Descriptor, v1.Descriptor) {
	t.Helper()
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	layerBytes := []byte("base layer bytes")
	layer := Descriptor(v1.MediaTypeImageLayerGzip, layerBytes)
	configBytes, err := json.Marshal(v1.Image{Platform: platform, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{digest.FromString("uncompressed layer")}}, Config: v1.ImageConfig{Cmd: []string{"cat", "/message"}}})
	if err != nil {
		t.Fatal(err)
	}
	config := Descriptor(v1.MediaTypeImageConfig, configBytes)
	manifestBytes, err := json.Marshal(VersionedManifest(config, []v1.Descriptor{layer}, ""))
	if err != nil {
		t.Fatal(err)
	}
	root := Descriptor(v1.MediaTypeImageManifest, manifestBytes)
	indexBytes, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []v1.Descriptor{root}})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"oci-layout":                              []byte(`{"imageLayoutVersion":"1.0.0"}`),
		"index.json":                              indexBytes,
		"blobs/sha256/" + layer.Digest.Encoded():  layerBytes,
		"blobs/sha256/" + config.Digest.Encoded(): configBytes,
		"blobs/sha256/" + root.Digest.Encoded():   manifestBytes,
	}
	if corrupt {
		files["blobs/sha256/"+layer.Digest.Encoded()] = []byte("corrupt layer")
	}
	path := filepath.Join(t.TempDir(), "image.oci.tar")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := tar.NewWriter(file)
	for name, data := range files {
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path, root, layer
}

func TestPublishImageArchivePreservesGraphAndFailedCopyKeepsTag(t *testing.T) {
	_, _, resolver, name := testRepository(t, registry.New())
	ctx := context.Background()
	path, root, layer := imageArchive(t, false)
	gotRoot, err := resolver.PublishImageArchive(ctx, name+":image", path)
	if err != nil || gotRoot.Digest != root.Digest {
		t.Fatalf("archive transfer failed: %+v, %v", gotRoot, err)
	}
	resolved, err := resolver.Resolve(ctx, name+":image", v1.Platform{OS: "linux", Architecture: "amd64"}, Image)
	if err != nil || resolved.Root.Digest != root.Digest || resolved.Layers[0].Digest != layer.Digest {
		t.Fatalf("archive descriptors changed: %+v, %v", resolved, err)
	}
	bad, _, _ := imageArchive(t, true)
	if _, err := resolver.PublishImageArchive(ctx, name+":image", bad); err == nil {
		t.Fatal("corrupt archive copied")
	}
	still, err := resolver.Resolve(ctx, name+":image", v1.Platform{OS: "linux", Architecture: "amd64"}, Image)
	if err != nil || still.Root.Digest != root.Digest {
		t.Fatalf("failed copy moved tag: %+v, %v", still, err)
	}
}

func TestPublishComponentUsesDockerCredentials(t *testing.T) {
	base := registry.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		user, pass, ok := req.BasicAuth()
		if !ok || user != "tester" || pass != "secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		base.ServeHTTP(w, req)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(fmt.Sprintf(`{"auths":{"%s":{"auth":"dGVzdGVyOnNlY3JldA=="}}}`, host)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)
	resolver, err := NewResolver(Options{TLSVerify: boolOption(false)})
	if err != nil {
		t.Fatal(err)
	}
	meta, paths := publicationMetadata(t, packageTar(t, "private"), true)
	ref := host + "/coopr/private:component"
	root, err := resolver.PublishComponent(context.Background(), ref, meta, paths)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(context.Background(), host+"/coopr/private@"+root.Digest.String(), meta.Platform, Component)
	if err != nil || resolved.Root.Digest != root.Digest {
		t.Fatalf("private component roundtrip failed: %+v, %v", resolved, err)
	}
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	unauthorized, err := NewResolver(Options{TLSVerify: boolOption(false)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unauthorized.PublishComponent(context.Background(), ref, meta, paths); err == nil {
		t.Fatal("publication without credentials succeeded")
	}
}

func TestRegistryAuthFileAndCustomCertificateDirectory(t *testing.T) {
	base := registry.New()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		user, pass, ok := req.BasicAuth()
		if !ok || user != "tester" || pass != "secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		base.ServeHTTP(w, req)
	}))
	server.StartTLS()
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "https://")

	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, []byte(fmt.Sprintf(`{"auths":{"%s":{"auth":"dGVzdGVyOnNlY3JldA=="}}}`, host)), 0o600); err != nil {
		t.Fatal(err)
	}
	certDir := t.TempDir()
	hostCertDir := filepath.Join(certDir, host)
	if err := os.MkdirAll(hostCertDir, 0o700); err != nil {
		t.Fatal(err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(hostCertDir, "ca.crt"), certificate, 0o600); err != nil {
		t.Fatal(err)
	}

	meta, paths := publicationMetadata(t, packageTar(t, "private-tls"), true)
	target := host + "/coopr/private:component"
	withoutTrust, err := NewResolver(Options{AuthFile: authFile})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withoutTrust.PublishComponent(context.Background(), target, meta, paths); err == nil {
		t.Fatal("custom-CA registry succeeded without its certificate directory")
	}
	resolver, err := NewResolver(Options{AuthFile: authFile, CertDir: hostCertDir})
	if err != nil {
		t.Fatal(err)
	}
	root, err := resolver.PublishComponent(context.Background(), target, meta, paths)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(context.Background(), host+"/coopr/private@"+root.Digest.String(), meta.Platform, Component)
	if err != nil || resolved.Root.Digest != root.Digest {
		t.Fatalf("custom-CA authenticated roundtrip failed: %+v, %v", resolved, err)
	}
}
