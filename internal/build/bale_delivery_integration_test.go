package build

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"coopr/internal/componentstore"
	"coopr/internal/oci"
	"coopr/internal/transfer"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	"github.com/strio-dev/bale"
)

func TestBuildConsumesBaleDeliveryWithoutSourceRegistry(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native build integration")
	}
	ctx := t.Context()
	work := t.TempDir()
	platform := "linux/" + runtime.GOARCH
	publisher := filepath.Join(work, "publisher")
	if err := os.Mkdir(publisher, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) string {
		t.Helper()
		path := filepath.Join(publisher, name)
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	source := httptest.NewServer(registry.New())
	t.Cleanup(source.Close)
	sourceHost := strings.TrimPrefix(source.URL, "http://")
	imageRef := sourceHost + "/toolchain:stable"
	childRef := sourceHost + "/child:stable"
	parentRef := sourceHost + "/parent:stable"
	write("base-marker", "delivered image payload\n")
	write("artifact", "delivered component payload\n")
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatal("live Buildah tests require the Nix dev shell's static busybox binary")
	}
	binary, err := os.ReadFile(busybox)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(publisher, "busybox"), binary, 0700); err != nil {
		t.Fatal(err)
	}
	imageDefinition := write("image.coopr", `from "scratch" {
    copy "base-marker" "/base-marker"
    copy "busybox" "/busybox" chmod="0755"
    env BASE="delivered"
}
`)
	publisherStore := nativeBuildTestStore(filepath.Join(work, "publisher-native"))
	if _, err := Run(ctx, Options{File: imageDefinition, Tag: imageRef, Push: true, Platform: platform, TLSVerify: new(false), BuildStore: publisherStore}); err != nil {
		t.Fatal(err)
	}
	childDefinition := write("child.coopr", fmt.Sprintf(`package as="payload" {
    copy "artifact" "/artifact"
}
extend {
    from %q {
        copy "/artifact" "/installed" from="payload"
        copy "/base-marker" "/copied-image" from=%q
        run network="none" {
            mount "bind" from=%q source="/" target="/delivered-input" readonly=#true
            exec "/busybox" "sh" "-ec" "/busybox cat /delivered-input/base-marker > /mounted-image"
        }
        env CHILD="delivered"
    }
}
`, imageRef, imageRef, imageRef))
	parentDefinition := write("parent.coopr", fmt.Sprintf(`extend {
    component %q
    env PARENT="delivered"
}
`, childRef))
	componentDigests := make(map[string]digest.Digest)
	for _, item := range []struct{ file, reference string }{{childDefinition, childRef}, {parentDefinition, parentRef}} {
		published, err := BuildComponent(ctx, ComponentOptions{File: item.file, Tag: item.reference, Push: true, Platform: platform, TLSVerify: new(false), BuildStore: publisherStore, StoreDir: filepath.Join(work, "publisher-components")})
		if err != nil {
			t.Fatal(err)
		}
		repository, value, ok := strings.Cut(published, "@")
		if !ok {
			t.Fatalf("published component has no immutable identity: %q", published)
		}
		componentDigests[repository], err = digest.Parse(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{sourceHost}})
	if err != nil {
		t.Fatal(err)
	}
	bundle := sourceHost + "/bundle:captured"
	created, err := client.Create(ctx, bale.Recipe{Include: []bale.Selection{{Ref: parentRef}}}, bundle, bale.CreateOptions{})
	if err != nil || !created.Published {
		t.Fatalf("capture recursive component and image dependencies: %+v, %v", created, err)
	}
	layout := filepath.Join(work, "bundle")
	if _, err := client.Pull(ctx, bundle, layout, bale.PullLocked); err != nil {
		t.Fatal(err)
	}
	destination := httptest.NewServer(registry.New())
	t.Cleanup(destination.Close)
	destinationHost := strings.TrimPrefix(destination.URL, "http://")
	client, err = bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{destinationHost}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := client.PlanPush(ctx, layout, destinationHost+"/bundle:delivered", bale.PushOptions{RepositoryMap: map[string]string{
		sourceHost + "/parent":    "components/parent",
		sourceHost + "/child":     "components/child",
		sourceHost + "/toolchain": "images/toolchain",
	}})
	if err != nil {
		t.Fatal(err)
	}
	source.Close()
	if err := os.RemoveAll(publisher); err != nil {
		t.Fatal(err)
	}
	delivered, err := client.Push(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	locations := make(map[string]string)
	for repository, identity := range componentDigests {
		relocation, ok := delivered.Relocations.Lookup(bale.ArtifactID{Repository: repository, Digest: identity})
		if !ok {
			t.Fatalf("delivery omitted component root %s@%s", repository, identity)
		}
		locations[repository] = relocation.Destination.Repository + "@" + relocation.Destination.Digest.String()
	}
	parent := locations[sourceHost+"/parent"]
	child := locations[sourceHost+"/child"]
	if parent == "" || child == "" {
		t.Fatalf("delivery omitted an application dependency: %+v", delivered.Relocations)
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(work, "consumer-data"))
	consumer := filepath.Join(work, "consumer.coopr")
	writeConsumer := func(t *testing.T, reference string) {
		t.Helper()
		if err := os.WriteFile(consumer, []byte(fmt.Sprintf("from \"scratch\" {\n    component %q\n}\n", reference)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeConsumer(t, parent)
	consumerStore := nativeBuildTestStore(filepath.Join(work, "consumer-native"))
	cache := filepath.Join(work, "consumer-cache")
	buildAndCheck := func(t *testing.T, name, pullPolicy string) string {
		t.Helper()
		archive := filepath.Join(work, name+".tar")
		var output bytes.Buffer
		if _, err := buildArchive(t, archive, ctx, Options{File: consumer, Platform: platform, TLSVerify: new(false), BuildStore: consumerStore, CacheLocalDir: cache, PullPolicy: pullPolicy, Stderr: &output}); err != nil {
			t.Fatalf("consume delivered component: %v\n%s", err, &output)
		}
		for _, name := range []string{"installed", "base-marker", "copied-image", "mounted-image"} {
			if !containsName(archiveLayerNames(t, archive), name) {
				t.Fatalf("delivered component/image payload %q is absent", name)
			}
		}
		config := archiveImageConfig(t, archive)
		for _, variable := range []string{"BASE=delivered", "CHILD=delivered", "PARENT=delivered"} {
			if !slices.Contains(config.Config.Env, variable) {
				t.Fatalf("delivered configuration lacks %s: %v", variable, config.Config.Env)
			}
		}
		return output.String()
	}
	t.Run("remote", func(t *testing.T) {
		buildAndCheck(t, "remote-first", "")
		if output := buildAndCheck(t, "remote-cached", ""); !strings.Contains(output, "Using component cache") {
			t.Fatalf("second build did not reuse a delivered component result:\n%s", output)
		}
	})
	if t.Failed() {
		return
	}
	t.Run("local-archive-offline", func(t *testing.T) {
		localDir := filepath.Join(work, "pulled-components")
		resolver, err := oci.NewResolver(oci.Options{ComponentStoreDir: localDir, TLSVerify: new(false)})
		if err != nil {
			t.Fatal(err)
		}
		archives := make(map[string]string)
		for name, reference := range map[string]string{"parent": parent, "child": child} {
			if _, err := resolver.PullComponent(ctx, reference, name); err != nil {
				t.Fatal(err)
			}
			alias := name + "-copy"
			if _, err := transfer.Copy(ctx, oci.Component, name, transfer.Destination{Transport: "local", Name: alias}, transfer.Options{ComponentStoreDir: localDir}); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(work, name+"-component.tar")
			if _, err := transfer.Copy(ctx, oci.Component, alias, transfer.Destination{Transport: "oci-archive", Name: archive}, transfer.Options{ComponentStoreDir: localDir}); err != nil {
				t.Fatal(err)
			}
			archives[name] = archive
		}
		destination.Close()
		importDir, err := componentstore.DefaultDir()
		if err != nil {
			t.Fatal(err)
		}
		resolver, err = oci.NewResolver(oci.Options{ComponentStoreDir: importDir})
		if err != nil {
			t.Fatal(err)
		}
		for name, archive := range archives {
			if _, err := resolver.LoadComponent(ctx, archive, "loaded-"+name); err != nil {
				t.Fatal(err)
			}
		}
		writeConsumer(t, "loaded-parent")
		buildAndCheck(t, "offline-loaded", "never")
	})
}
