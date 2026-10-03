package build

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"coopr/internal/oci"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

const exampleBase = "docker.io/library/alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"

// This is an opt-in system test: it compiles the actual published gofmt
// component, removes both publisher checkouts, and runs the final image through
// rootless Podman after an OCI registry round trip.
func TestPublishedExamplesSurvivePublisherRemovalAndRun(t *testing.T) {
	if os.Getenv("COOPR_TEST_EXAMPLES") != "1" {
		t.Skip("set COOPR_TEST_EXAMPLES=1 for the costly published-example acceptance")
	}
	if os.Getenv("COOPR_TEST_BUILDAH_REGISTRY") == "" {
		t.Skip("set COOPR_TEST_BUILDAH_REGISTRY for live Buildah registry tests")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Fatalf("rootless Podman is required for the example acceptance: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	server := httptest.NewServer(registry.New())
	defer server.Close()
	registryHost := strings.TrimPrefix(server.URL, "http://")

	companyDir := t.TempDir()
	copyExampleFile(t, filepath.Join("..", "..", "examples", "published-components", "components", "company-config", "component.coopr"), filepath.Join(companyDir, "component.coopr"))
	copyExampleFile(t, filepath.Join("..", "..", "examples", "published-components", "components", "company-config", "defaults.json"), filepath.Join(companyDir, "defaults.json"))
	company, err := PublishComponent(ctx, PublishOptions{
		File: filepath.Join(companyDir, "component.coopr"), Reference: registryHost + "/coopr/company:debug",
		Target: "debug", Platform: "linux/amd64", PlainHTTP: true,
	})
	if err != nil {
		t.Fatalf("publish company-config example: %v", err)
	}
	if !strings.Contains(company, "@sha256:") {
		t.Fatalf("company publication is mutable: %q", company)
	}

	gofmtDir := t.TempDir()
	copyExampleFile(t, filepath.Join("..", "..", "examples", "published-components", "components", "gofmt", "component.coopr"), filepath.Join(gofmtDir, "component.coopr"))
	gofmt, err := PublishComponent(ctx, PublishOptions{
		File: filepath.Join(gofmtDir, "component.coopr"), Reference: registryHost + "/coopr/gofmt:stable",
		Platform: "linux/amd64", PlainHTTP: true,
	})
	if err != nil {
		t.Fatalf("publish gofmt example: %v", err)
	}
	if !strings.Contains(gofmt, "@sha256:") {
		t.Fatalf("gofmt publication is mutable: %q", gofmt)
	}
	// Invocation must use only the immutable OCI artifacts. Remove the full
	// publisher trees, including definitions and source payload.
	if err := os.RemoveAll(companyDir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(gofmtDir); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{companyDir, gofmtDir} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("publisher tree still exists %q: %v", dir, err)
		}
	}

	consumerDir := t.TempDir()
	consumerFile := filepath.Join(consumerDir, "consumer.coopr")
	consumerSource := fmt.Sprintf(`from %q
copy "unformatted.go" "/work/unformatted.go"
component %q channel="preview"
component %q
run "/usr/local/bin/gofmt -w /work/unformatted.go"
workdir "/work"
label coopr_example="published-components"
cmd { exec "/bin/sh" "-c" "cat /etc/company/defaults.json; cat /work/unformatted.go" }
`, exampleBase, company, gofmt)
	if err := os.WriteFile(consumerFile, []byte(consumerSource), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consumerDir, "unformatted.go"), []byte("package main\nfunc main(){println(\"ready\")}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(consumerDir, "consumer.oci.tar")
	if _, err := Run(ctx, Options{File: consumerFile, Tag: "oci-archive:" + archive, Platform: "linux/amd64", PlainHTTP: true}); err != nil {
		t.Fatalf("build consumer after removing publisher trees: %v", err)
	}
	manifest, image := readExampleImage(t, ctx, archive)
	if image.OS != "linux" || image.Architecture != "amd64" || image.Config.WorkingDir != "/work" || image.Config.Labels["coopr_example"] != "published-components" || len(image.Config.Cmd) != 3 || image.Config.Cmd[0] != "/bin/sh" {
		t.Fatalf("final image platform/config changed: %+v", image)
	}
	if !hasEnv(image.Config.Env, "company_channel=preview") || !hasEnv(image.Config.Env, "company_debug=true") {
		t.Fatalf("company debug target/argument missing: %+v", image.Config.Env)
	}
	if len(manifest.Layers) != 5 || len(image.RootFS.DiffIDs) != 5 {
		t.Fatalf("expected base + COPY + two compact components + gofmt RUN, got layers=%d diffIDs=%d, history=%+v, diffIDs=%+v", len(manifest.Layers), len(image.RootFS.DiffIDs), image.History, image.RootFS.DiffIDs)
	}
	for _, id := range image.RootFS.DiffIDs {
		if id.String() == "sha256:5f70bf18a086007016e948b04aed3b82103a36bea41755b6cddfaf10ace3c6ef" {
			t.Fatal("final image retained a new empty layer")
		}
	}
	var nonempty int
	for _, entry := range image.History {
		if !entry.EmptyLayer {
			nonempty++
		}
	}
	if nonempty != len(manifest.Layers) {
		t.Fatalf("history has %d filesystem entries for %d layers: %+v", nonempty, len(manifest.Layers), image.History)
	}
	resolver, err := oci.NewResolver(oci.Options{PlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	base, err := resolver.Resolve(ctx, exampleBase, v1.Platform{OS: "linux", Architecture: "amd64"}, oci.Image)
	if err != nil {
		t.Fatalf("resolve original base: %v", err)
	}
	var baseImage v1.Image
	if err := json.Unmarshal(base.ConfigData, &baseImage); err != nil {
		t.Fatalf("decode original base config: %v", err)
	}
	baseLayerCount := len(baseImage.RootFS.DiffIDs)
	if baseLayerCount == 0 || baseLayerCount != len(base.Layers) || len(image.RootFS.DiffIDs) < baseLayerCount || !slices.Equal(image.RootFS.DiffIDs[:baseLayerCount], baseImage.RootFS.DiffIDs) {
		t.Fatalf("original base filesystem chain changed: got=%v base=%v baseLayers=%d", image.RootFS.DiffIDs, baseImage.RootFS.DiffIDs, len(base.Layers))
	}
	finalRef := registryHost + "/coopr/consumer:acceptance"
	root, err := resolver.PublishImageArchive(ctx, finalRef, archive)
	if err != nil {
		t.Fatalf("publish final OCI image archive: %v", err)
	}
	immutableImage := registryHost + "/coopr/consumer@" + root.Digest.String()
	refExistsErr := exec.CommandContext(ctx, "podman", "image", "exists", immutableImage).Run()
	if refExistsErr == nil {
		t.Fatalf("acceptance image unexpectedly existed before test: %s", immutableImage)
	}
	if exit, ok := refExistsErr.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("check preexisting Podman reference %s: %v", immutableImage, refExistsErr)
	}
	imageID := manifest.Config.Digest.String()
	existsErr := exec.CommandContext(ctx, "podman", "image", "exists", imageID).Run()
	if existsErr != nil {
		if exit, ok := existsErr.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
			t.Fatalf("check preexisting Podman image %s: %v", imageID, existsErr)
		}
	}
	output, err := exec.CommandContext(ctx, "podman", "pull", "--tls-verify=false", immutableImage).CombinedOutput()
	if err != nil {
		t.Fatalf("rootless Podman pull: %v: %s", err, output)
	}
	if existsErr != nil {
		t.Cleanup(func() {
			remove := exec.Command("podman", "rmi", immutableImage)
			if output, err := remove.CombinedOutput(); err != nil {
				t.Errorf("remove owned Podman image %s: %v: %s", immutableImage, err, output)
			}
		})
	}
	run := exec.CommandContext(ctx, "podman", "run", "--rm", "--network=none", "--entrypoint", "/bin/sh", immutableImage,
		"-c", `test "$company_channel" = preview && test "$company_debug" = true && test -x /usr/local/bin/gofmt && test -s /etc/company/defaults.json && test -z "$(/usr/local/bin/gofmt -l /work/unformatted.go)" && grep -q '"format": "json"' /etc/company/defaults.json && cat /work/unformatted.go && printf '\nEXAMPLES_OK\n'`)
	output, err = run.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "func main() {") || !strings.Contains(string(output), "EXAMPLES_OK") {
		t.Fatalf("rootless Podman run failed: %v: %s", err, output)
	}
}

func copyExampleFile(t *testing.T, source, target string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func hasEnv(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}

func readExampleImage(t *testing.T, ctx context.Context, archive string) (v1.Manifest, v1.Image) {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	r := tar.NewReader(f)
	var index v1.Index
	for {
		h, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if h.Name != "index.json" && h.Name != "./index.json" {
			continue
		}
		if err := json.NewDecoder(io.LimitReader(r, h.Size)).Decode(&index); err != nil {
			t.Fatal(err)
		}
		break
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 1 {
		t.Fatalf("expected one final image root, got %d", len(index.Manifests))
	}
	store, err := orasoci.NewFromTar(ctx, archive)
	if err != nil {
		t.Fatal(err)
	}
	read := func(desc v1.Descriptor, dst any) {
		stream, err := store.Fetch(ctx, desc)
		if err != nil {
			t.Fatal(err)
		}
		decodeErr := json.NewDecoder(io.LimitReader(stream, 8<<20)).Decode(dst)
		closeErr := stream.Close()
		if decodeErr != nil || closeErr != nil {
			t.Fatalf("decode OCI metadata %s: %v, close: %v", desc.Digest, decodeErr, closeErr)
		}
	}
	var manifest v1.Manifest
	read(index.Manifests[0], &manifest)
	var image v1.Image
	read(manifest.Config, &image)
	return manifest, image
}
