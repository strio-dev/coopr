package build

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/oci"
	"github.com/google/go-containerregistry/pkg/registry"
)

func TestRunPullRefreshesBaseTagAndPreservesOfflineDigest(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native build integration")
	}
	for _, tc := range []struct {
		name       string
		withLayers bool
	}{
		{name: "config-only base"},
		{name: "layered base", withLayers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRunPullRefresh(t, tc.withLayers)
		})
	}
}

func testRunPullRefresh(t *testing.T, withLayers bool) {
	t.Helper()
	ctx := context.Background()
	server := httptest.NewServer(registry.New())
	defer server.Close()
	ref := strings.TrimPrefix(server.URL, "http://") + "/coopr/base:stable"
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	storeBase := filepath.Join(os.Getenv("XDG_DATA_HOME"), "pull-refresh", strings.ReplaceAll(t.Name(), "/", "-"))
	baseFile := filepath.Join(dir, "base.coopr")
	baseArchive := filepath.Join(dir, "base.oci.tar")
	produceBase := func(revision string) {
		t.Helper()
		definition := "from \"scratch\"\n"
		if withLayers {
			if err := os.WriteFile(filepath.Join(dir, "base-marker"), []byte(revision), 0600); err != nil {
				t.Fatal(err)
			}
			definition += "copy \"base-marker\" \"/base-marker\"\n"
		}
		definition += fmt.Sprintf("label revision=\"%s\"\n", revision)
		if err := os.WriteFile(baseFile, []byte(definition), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Run(ctx, Options{
			File: baseFile, Tag: "oci-archive:" + baseArchive,
			Platform: "linux/amd64", BuildStore: nativeBuildTestStore(filepath.Join(storeBase, "producer-store")),
		}); err != nil {
			t.Fatal(err)
		}
	}
	produceBase("old")
	old, err := resolver.PublishImageArchive(ctx, ref, baseArchive)
	if err != nil {
		t.Fatal(err)
	}

	consumerFile := filepath.Join(dir, "consumer.coopr")
	if withLayers {
		if err := os.WriteFile(filepath.Join(dir, "consumer-marker"), []byte("consumer"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeConsumer := func(from string) {
		t.Helper()
		definition := "from \"" + from + "\"\n"
		if withLayers {
			definition += "copy \"consumer-marker\" \"/consumer-marker\"\n"
		}
		if err := os.WriteFile(consumerFile, []byte(definition), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeConsumer(ref)
	consumerArchive := filepath.Join(dir, "consumer.oci.tar")
	opts := Options{
		File: consumerFile, Tag: "oci-archive:" + consumerArchive,
		Platform: "linux/amd64", TLSVerify: new(false), BuildStore: nativeBuildTestStore(filepath.Join(storeBase, "consumer-store")),
	}
	assertRevision := func(want string) {
		t.Helper()
		if _, err := Run(ctx, opts); err != nil {
			t.Fatal(err)
		}
		_, image := readExampleImage(t, ctx, consumerArchive)
		if got := image.Config.Labels["revision"]; got != want {
			t.Fatalf("base revision = %q, want %q", got, want)
		}
		if withLayers && !containsName(archiveLayerNames(t, consumerArchive), "base-marker") {
			t.Fatal("output omitted the base image layer")
		}
	}
	writeConsumer(strings.TrimSuffix(ref, ":stable") + "@" + old.Digest.String())
	pinned := opts
	pinned.BuildStore = nativeBuildTestStore(filepath.Join(storeBase, "named-digest-store"))
	pinned.Tag = "oci-archive:" + filepath.Join(dir, "pinned-consumer.oci.tar")
	if _, err := Run(ctx, pinned); err != nil {
		t.Fatalf("pull uncached named digest: %v", err)
	}
	writeConsumer(ref)
	assertRevision("old")
	if !withLayers {
		crossFormat := opts
		crossFormat.Format = "docker"
		crossFormat.Tag = "oci-archive:" + filepath.Join(dir, "consumer-docker.oci.tar")
		if _, err := Run(ctx, crossFormat); err != nil {
			t.Fatal(err)
		}
		assertRevision("old") // A same-config-ID Docker alternate must not invalidate the selected OCI manifest.
	}

	produceBase("new")
	if _, err := resolver.PublishImageArchive(ctx, ref, baseArchive); err != nil {
		t.Fatal(err)
	}
	assertRevision("old") // A cached mutable tag is local-first by default.
	opts.PullPolicy = string(oci.PullNewer)
	assertRevision("new") // Different registry digests refresh under newer.
	assertRevision("new") // An unchanged registry digest reuses the local image.
	opts.PullPolicy = string(oci.PullMissing)
	opts.Pull = true
	assertRevision("new") // --pull replaces the cached tag with the registry result.
	opts.Pull = false
	assertRevision("new") // Later default builds use the refreshed local tag.

	server.Close()
	writeConsumer(ref)
	opts.Pull = false
	opts.PullPolicy = string(oci.PullNewer)
	assertRevision("new") // Registry errors under newer retain an available local image.
	opts.PullPolicy = string(oci.PullNever)
	assertRevision("new") // never reuses an available mutable local tag without contacting the registry.
	missing := opts
	missing.BuildStore = nativeBuildTestStore(filepath.Join(storeBase, "pull-never-empty-store"))
	if _, err := Run(ctx, missing); err == nil || !strings.Contains(err.Error(), "pull policy is never") {
		t.Fatalf("missing base under pull policy never = %v", err)
	}

	writeConsumer(old.Digest.String())
	opts.PullPolicy = string(oci.PullNever)
	assertRevision("old") // The retained native digest remains available after its registry tag moves.
}
