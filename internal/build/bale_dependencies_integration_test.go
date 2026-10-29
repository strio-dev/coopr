package build

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/componentstore"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
)

func TestBuildComponentPublishesBaleDependenciesAndConsumes(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native build integration")
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	child := host + "/coopr/live-child:stable"
	parent := host + "/coopr/live-parent:stable"
	work := t.TempDir()
	publisher := filepath.Join(work, "publisher")
	if err := os.Mkdir(publisher, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"payload": "hello from a nested component\n",
		"child.coopr": `package as="pkg" {
    copy "payload" "/artifact"
}
extend {
    copy "/artifact" "/installed" from="pkg"
    env CHILD="applied"
}
`,
		"parent.coopr": fmt.Sprintf("arg \"dependency\" %q\nextend {\n    arg \"dependency\"\n    component \"${dependency}\"\n    env PARENT=\"applied\"\n}\n", child),
	} {
		if err := os.WriteFile(filepath.Join(publisher, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	storeDir := filepath.Join(work, "published-components")
	store := nativeBuildTestStore(filepath.Join(work, "native"))
	var parentImmutable string
	for _, item := range []struct{ file, reference string }{{"child.coopr", child}, {"parent.coopr", parent}} {
		var stderr bytes.Buffer
		published, err := BuildComponent(t.Context(), ComponentOptions{
			File: filepath.Join(publisher, item.file), Tag: item.reference, Push: true,
			Platform: "linux/amd64", TLSVerify: new(false), BuildStore: store, StoreDir: storeDir,
			Stderr: &stderr,
		})
		if err != nil {
			t.Fatal(err)
		}
		if item.reference == parent {
			if !strings.Contains(stderr.String(), "runtime-selected dependencies explicitly") {
				t.Fatalf("component build push omitted dependency discovery warning: %q", &stderr)
			}
			if !strings.HasPrefix(published, host+"/coopr/live-parent@sha256:") || strings.ContainsAny(published, "\n \t") {
				t.Fatalf("build result contains more than the immutable reference: %q", published)
			}
			parentImmutable = published
		}
	}
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := client.Resolve(t.Context(), parentImmutable)
	if err != nil {
		t.Fatal(err)
	}
	local, err := componentstore.Open(t.Context(), storeDir)
	if err != nil {
		t.Fatal(err)
	}
	root, err := local.Resolve(t.Context(), remote.Descriptor.Digest.String())
	if err != nil || !content.Equal(root, remote.Descriptor) {
		t.Fatalf("registry attachment changed the built component digest: %+v, %+v, %v", root, remote.Descriptor, err)
	}
	declarations, err := client.InspectDependencies(t.Context(), parentImmutable)
	if err != nil || len(declarations) != 1 || !content.Equal(declarations[0].Subject, root) || len(declarations[0].Declaration.Dependencies) != 1 || declarations[0].Declaration.Dependencies[0].Ref != child {
		t.Fatalf("built parent dependency declaration: %+v, %v", declarations, err)
	}
	if err := os.RemoveAll(publisher); err != nil {
		t.Fatal(err)
	}
	consumer := filepath.Join(work, "image.coopr")
	if err := os.WriteFile(consumer, []byte(fmt.Sprintf("from \"scratch\" {\n    component %q\n}\n", parentImmutable)), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(work, "image.oci.tar")
	if _, err := buildArchive(t, archive, t.Context(), Options{File: consumer, Platform: "linux/amd64", TLSVerify: new(false), BuildStore: store}); err != nil {
		t.Fatal(err)
	}
	if !containsName(archiveLayerNames(t, archive), "installed") {
		t.Fatal("consumer image lacks the nested component's packaged artifact")
	}
	config := archiveImageConfig(t, archive)
	if !slices.Contains(config.Config.Env, "CHILD=applied") || !slices.Contains(config.Config.Env, "PARENT=applied") {
		t.Fatalf("nested component configuration was not inherited: %v", config.Config.Env)
	}
}
