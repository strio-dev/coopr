package build

import (
	"archive/tar"
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/oci"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestPublishComponentServiceFromSourceAndConsume(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH_REGISTRY") == "" {
		t.Skip("set COOPR_TEST_BUILDAH_REGISTRY for live Buildah registry tests")
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	ref := strings.TrimPrefix(server.URL, "http://") + "/coopr/service:stable"
	publisher := t.TempDir()
	component := filepath.Join(publisher, "component.coopr")
	if err := os.WriteFile(component, []byte("package as=\"pkg\"\ncopy \"publisher-source\" \"/artifact\"\nextend as=\"base\"\ncopy \"/artifact\" \"/installed\" from=\"pkg\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(publisher, "publisher-source")
	if err := os.WriteFile(source, []byte("published from source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	immutable, err := PublishComponent(context.Background(), PublishOptions{
		File: component, Reference: ref, Platform: "linux/amd64", TLSVerify: new(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(publisher, "coopr.lock")); !os.IsNotExist(err) {
		t.Fatalf("publication created a project lockfile: %v", err)
	}
	if !strings.HasPrefix(immutable, strings.TrimSuffix(ref, ":stable")+"@sha256:") {
		t.Fatalf("publication returned nonimmutable reference %q", immutable)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	consumer := t.TempDir()
	app := filepath.Join(consumer, "app.coopr")
	if err := os.WriteFile(app, []byte("from \"scratch\"\ncomponent \""+immutable+"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(consumer, "app.oci.tar")
	if _, err := Run(context.Background(), Options{File: app, Tag: "oci-archive:" + archive, Platform: "linux/amd64", TLSVerify: new(false)}); err != nil {
		t.Fatal(err)
	}
	if !containsName(archiveLayerNames(t, archive), "installed") {
		t.Fatal("consumer image lacks published package file")
	}
}

func TestBuildComponentDefaultLocalStoreThenInvoke(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for live Buildah tests")
	}
	publisher := t.TempDir()
	component := filepath.Join(publisher, "component.coopr")
	if err := os.WriteFile(component, []byte("package as=\"pkg\"\ncopy \"payload\" \"/artifact\"\nextend as=\"base\"\ncopy \"/artifact\" \"/installed\" from=\"pkg\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(publisher, "payload"), []byte("local component\n"), 0600); err != nil {
		t.Fatal(err)
	}
	local, err := BuildComponent(context.Background(), ComponentOptions{
		File: component, Platform: "linux/amd64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(local, "sha256:") {
		t.Fatalf("unexpected local identity %q", local)
	}
	consumer := t.TempDir()
	app := filepath.Join(consumer, "app.coopr")
	if err := os.WriteFile(app, []byte("from \"scratch\"\ncomponent \""+local+"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(consumer, "app.oci.tar")
	if _, err := Run(context.Background(), Options{File: app, Tag: "oci-archive:" + archive, Platform: "linux/amd64"}); err != nil {
		t.Fatal(err)
	}
	if !containsName(archiveLayerNames(t, archive), "installed") {
		t.Fatal("image built from local component lacks installed artifact")
	}
}

func TestPublishComponentDoesNotRunInvocationSteps(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH_REGISTRY") == "" {
		t.Skip("set COOPR_TEST_BUILDAH_REGISTRY for live Buildah registry tests")
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	ref := strings.TrimPrefix(server.URL, "http://") + "/coopr/poison:stable"
	dir := t.TempDir()
	component := filepath.Join(dir, "component.coopr")
	if err := os.WriteFile(component, []byte("extend as=\"base\"\nrun \"exit 79\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	immutable, err := PublishComponent(context.Background(), PublishOptions{
		File: component, Reference: ref, Platform: "linux/amd64", TLSVerify: new(false),
	})
	if err != nil {
		t.Fatalf("publication executed invocation RUN: %v", err)
	}
	if !strings.Contains(immutable, "@sha256:") {
		t.Fatalf("missing immutable reference: %q", immutable)
	}
}

func TestPublishIndependentPackages(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH_REGISTRY") == "" {
		t.Skip("set COOPR_TEST_BUILDAH_REGISTRY for live Buildah registry tests")
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	ref := strings.TrimPrefix(server.URL, "http://") + "/coopr/two-packages:stable"
	dir := t.TempDir()
	file := filepath.Join(dir, "component.coopr")
	content := "package as=\"first\"\ncopy \"first-source\" \"/first-source\"\npackage as=\"second\"\ncopy \"second-source\" \"/second-source\"\nextend as=\"base\"\ncopy \"/first-source\" \"/first\" from=\"first\"\ncopy \"/second-source\" \"/second\" from=\"second\"\n"
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first-source", "second-source"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	immutable, err := PublishComponent(context.Background(), PublishOptions{File: file, Reference: ref, Platform: "linux/amd64", TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first-source", "second-source"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	consumer := t.TempDir()
	app := filepath.Join(consumer, "app.coopr")
	if err := os.WriteFile(app, []byte("from \"scratch\"\ncomponent \""+immutable+"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(consumer, "app.oci.tar")
	if _, err := Run(context.Background(), Options{File: app, Tag: "oci-archive:" + archive, Platform: "linux/amd64", TLSVerify: new(false)}); err != nil {
		t.Fatal(err)
	}
	names := archiveLayerNames(t, archive)
	if !containsName(names, "first") || !containsName(names, "second") {
		t.Fatalf("independent packages absent from consumer: %v", names)
	}
}

func TestPublishCopyDotExcludesPackageStagingInsideContext(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH_REGISTRY") == "" {
		t.Skip("set COOPR_TEST_BUILDAH_REGISTRY for live Buildah registry tests")
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	ref := strings.TrimPrefix(server.URL, "http://") + "/coopr/context:stable"
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	component := filepath.Join(dir, "component.coopr")
	content := "package as=\"first\"\ncopy \".\" \"/captured-first/\"\npackage as=\"second\"\ncopy \".\" \"/captured-second/\"\nextend as=\"base\"\ncopy \"/captured-first/marker\" \"/first\" from=\"first\"\ncopy \"/captured-second/marker\" \"/second\" from=\"second\"\n"
	if err := os.WriteFile(component, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("authored\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishComponent(context.Background(), PublishOptions{File: component, Reference: ref, Platform: "linux/amd64", TLSVerify: new(false)}); err != nil {
		t.Fatal(err)
	}
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(context.Background(), ref, v1.Platform{OS: "linux", Architecture: "amd64"}, oci.Component)
	if err != nil || resolved.Component == nil || len(resolved.Component.Packages) != 2 {
		t.Fatalf("expected two published packages: %+v, %v", resolved, err)
	}
	for _, pkg := range resolved.Component.Packages {
		path := filepath.Join(t.TempDir(), "package.tar")
		if err := resolver.Download(context.Background(), resolved, pkg.Descriptor, path); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		reader := tar.NewReader(file)
		marker := false
		for {
			header, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			name := strings.TrimPrefix(header.Name, "./")
			if strings.HasSuffix(name, "/marker") {
				marker = true
			}
			if strings.Contains(name, ".coopr-stage-") || strings.Contains(name, ".coopr-build-worker-") {
				_ = file.Close()
				t.Fatalf("package %s captured Coopr-owned context artifact %q", pkg.Stage, name)
			}
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if !marker {
			t.Fatalf("package %s lost authored marker", pkg.Stage)
		}
	}
}
