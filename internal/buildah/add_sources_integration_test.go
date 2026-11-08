package buildah

import (
	"archive/tar"
	"context"
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

	"github.com/opencontainers/go-digest"
	"github.com/strio-dev/coopr/internal/definition"
	storagearchive "go.podman.io/storage/pkg/archive"
)

func TestBuildMultipleCopyAndAddSourcesUseNativeDestinationSemantics(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah COPY/ADD build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(contextDir, name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store := cacheTestStore(root)
	layout := filepath.Join(root, "layout")
	_, err := BuildPlan(ctx, testPlan(t, `from "scratch" {
 workdir "/copy-dot"
 copy "one" "two" "."
 workdir "/add-dot"
 add "one" "two" "."
 workdir "/copy-existing"
 workdir "/"
 copy "one" "two" "/copy-existing"
 workdir "/add-existing"
 workdir "/"
 add "one" "two" "/add-existing"
}`), PlanOptions{
		Store: store, ContextDir: contextDir, Isolation: "rootless",
		Output: Output{Path: layout, Filesystem: FilesystemOutput{Type: "tar", Path: filepath.Join(root, "rootfs.tar")}, Reference: "multi-source-destinations"},
	})
	if err != nil {
		t.Fatal(err)
	}
	layer := filepath.Join(root, "rootfs.tar")
	for _, directory := range []string{"copy-dot", "add-dot", "copy-existing", "add-existing"} {
		for _, name := range []string{"one", "two"} {
			if got := readLayerFile(t, layer, directory+"/"+name); got != name+"\n" {
				t.Fatalf("%s/%s = %q", directory, name, got)
			}
		}
	}

	for _, test := range []struct{ name string }{{"copy"}, {"add"}} {
		t.Run(test.name+" rejects existing file", func(t *testing.T) {
			_, err := BuildPlan(ctx, testPlan(t, fmt.Sprintf(`from "scratch" {
 copy "one" "/target"
 %s "one" "two" "/target"
}`, test.name)), PlanOptions{
				Store: store, ContextDir: contextDir, Isolation: "rootless",
				Output: Output{Path: filepath.Join(root, "invalid-"+test.name), Reference: "invalid-" + test.name},
			})
			if err == nil {
				t.Fatal("native Buildah accepted multiple sources with an existing file destination")
			}
			if strings.Contains(err.Error(), "destination must end with a slash") {
				t.Fatalf("Coopr rejected destination before native type checking: %v", err)
			}
		})
	}
}

func TestBuildSplitMultipleSourcesUseNativeDestinationSemantics(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah mixed COPY/ADD build in short mode")
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(strings.TrimPrefix(request.URL.Path, "/") + "\n"))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "local"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := cacheTestStore(root)
	plan := testPlan(t, fmt.Sprintf(`from "scratch" {
 workdir "/mixed-dot"
 copy "local" "."
 workdir "/remote-dot"
 add %q %q "."
 workdir "/mixed-existing"
 workdir "/"
 copy "local" "/mixed-existing"
 workdir "/remote-existing"
 workdir "/"
 add %q %q "/remote-existing"
 user "1000"
 copy "local" "/owner-default"
 copy "local" "/owner-explicit" chown="12:34"
}`, server.URL+"/remote-one", server.URL+"/remote-two", server.URL+"/remote-one", server.URL+"/remote-two"))
	for index := range plan.Stages[0].Operations {
		operation := &plan.Stages[0].Operations[index]
		if operation.Name == "copy" {
			operation.InlineFiles = []definition.InlineFile{{Path: "inline", Data: "inline\n"}}
		}
	}
	layout := filepath.Join(root, "split-layout")
	_, err := BuildPlan(ctx, plan, PlanOptions{
		Store: store, ContextDir: contextDir, Isolation: "rootless",
		Output: Output{Path: layout, Filesystem: FilesystemOutput{Type: "tar", Path: filepath.Join(root, "rootfs.tar")}, Reference: "split-multi-source-destinations"},
	})
	if err != nil {
		t.Fatal(err)
	}
	layer := filepath.Join(root, "rootfs.tar")
	for path, want := range map[string]string{
		"mixed-dot/local":            "local\n",
		"mixed-dot/inline":           "inline\n",
		"remote-dot/remote-one":      "remote-one\n",
		"remote-dot/remote-two":      "remote-two\n",
		"mixed-existing/local":       "local\n",
		"mixed-existing/inline":      "inline\n",
		"remote-existing/remote-one": "remote-one\n",
		"remote-existing/remote-two": "remote-two\n",
	} {
		if got := readLayerFile(t, layer, path); got != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
	if header := readLayerHeader(t, layer, "owner-default"); header.Uid != 0 || header.Gid != 0 {
		t.Fatalf("implicit destination owner = %d:%d, want 0:0", header.Uid, header.Gid)
	}
	if header := readLayerHeader(t, layer, "owner-explicit"); header.Uid != 12 || header.Gid != 34 {
		t.Fatalf("explicit destination owner = %d:%d, want 12:34", header.Uid, header.Gid)
	}

	for _, test := range []struct{ name string }{{"mixed-copy"}, {"remote-add"}} {
		t.Run(test.name+" rejects existing file", func(t *testing.T) {
			plan := testPlan(t, `from "scratch" {
 copy "local" "/target"
 copy "local" "/target"
}`)
			if test.name == "mixed-copy" {
				plan.Stages[0].Operations[1].InlineFiles = []definition.InlineFile{{Path: "inline", Data: "inline\n"}}
			} else {
				plan = testPlan(t, fmt.Sprintf(`from "scratch" {
 copy "local" "/target"
 add %q %q "/target"
}`, server.URL+"/remote-one", server.URL+"/remote-two"))
			}

			_, err := BuildPlan(ctx, plan, PlanOptions{
				Store: store, ContextDir: contextDir, Isolation: "rootless",
				Output: Output{Path: filepath.Join(root, "split-invalid-"+test.name), Reference: "split-invalid-" + test.name},
			})
			if err == nil {
				t.Fatal("accepted split multiple sources with an existing file destination")
			}
		})
	}
}

func TestBuildPlanUnpacksRemoteArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah remote ADD build in short mode")
	}
	archive := tarBytes(t, "proof", "remote unpack\n")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(archive)
	}))
	defer server.Close()

	plan := testPlan(t, `from "scratch" {
    add "`+server.URL+"/archive.tar\" \"/unpacked/\" unpack=\"true\" checksum=\""+digest.FromBytes(archive).String()+`"
}
`)
	root := t.TempDir()
	layout := filepath.Join(root, "layout")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := BuildPlan(ctx, plan, PlanOptions{
		Store: cacheTestStore(root), ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
	}); err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("layers = %d, want 1", len(manifest.Layers))
	}
	layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	if got := readLayerFile(t, layer, "unpacked/proof"); got != "remote unpack\n" {
		t.Fatalf("unpacked proof = %q", got)
	}
}

func TestBuildPlanAddsAuthenticatedRemoteFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah remote ADD build in short mode")
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer build-token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = writer.Write([]byte("authenticated payload\n"))
	}))
	defer server.Close()
	root := t.TempDir()
	secretFile := filepath.Join(root, "token")
	if err := os.WriteFile(secretFile, []byte("build-token"), 0600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, `from "scratch" {
    add "`+server.URL+`/payload" "/payload"
}
`)
	layout := filepath.Join(root, "layout")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store: cacheTestStore(root), ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
		Secrets: []string{"id=HTTP_AUTH_TOKEN_127.0.0.1,src=" + secretFile},
	}); err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("layers = %d, want 1", len(manifest.Layers))
	}
	layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	if got := readLayerFile(t, layer, "payload"); got != "authenticated payload\n" {
		t.Fatalf("authenticated ADD payload = %q", got)
	}
}

func TestBuildPlanAddsGitDefaultRefChecksumAndMetadata(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah Git ADD build in short mode")
	}
	source, commit := gitProtocolFixture(t)
	plan := testPlan(t, `
from "scratch" {
    add "`+source+"\" \"/without/\" checksum=\""+commit[:12]+`"
    add "`+source+"\" \"/with/\" checksum=\""+commit[:12]+`" keep-git-dir="true"
}
`)
	root := t.TempDir()
	layout := filepath.Join(root, "layout")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := BuildPlan(ctx, plan, PlanOptions{
		Store: cacheTestStore(root), ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
	}); err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 2 {
		t.Fatalf("layers = %d, want 2", len(manifest.Layers))
	}
	without := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	if got := readLayerFile(t, without, "without/proof"); got != "default branch\n" {
		t.Fatalf("default-ref proof = %q", got)
	}
	if layerContainsPath(t, without, "without/.git/HEAD") {
		t.Fatal("default Git ADD retained .git metadata")
	}
	with := filepath.Join(layout, "blobs", "sha256", manifest.Layers[1].Digest.Encoded())
	if got := readLayerFile(t, with, "with/proof"); got != "default branch\n" {
		t.Fatalf("keep-git-dir proof = %q", got)
	}
	if !layerContainsPath(t, with, "with/.git/HEAD") {
		t.Fatal("keep-git-dir Git ADD removed .git metadata")
	}
}

func TestBuildPlanAddsRecursiveGitSubmodulesWithoutMetadata(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah Git submodule ADD build in short mode")
	}
	source, _ := gitHTTPSubmoduleFixture(t, "")
	plan := testPlan(t, `from "scratch" {
    add "`+source+`" "/source/"
}
`)
	root := t.TempDir()
	layout := filepath.Join(root, "layout")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := BuildPlan(ctx, plan, PlanOptions{
		Store: cacheTestStore(root), ContextDir: root, Isolation: "rootless", Output: Output{Path: layout},
	}); err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("layers = %d, want 1", len(manifest.Layers))
	}
	layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	if got := readLayerFile(t, layer, "source/deps/submodule/deps/nested/nested-proof"); got != "nested submodule\n" {
		t.Fatalf("nested submodule proof = %q", got)
	}
	for _, metadata := range []string{"source/.git/HEAD", "source/deps/submodule/.git", "source/deps/submodule/deps/nested/.git"} {
		if layerContainsPath(t, layer, metadata) {
			t.Fatalf("default Git ADD retained metadata %q", metadata)
		}
	}
}

func layerContainsPath(t *testing.T, blobPath, wanted string) bool {
	t.Helper()
	file, err := os.Open(blobPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close layer: %v", err)
		}
	}() //nolint:errcheck
	stream, err := storagearchive.DecompressStream(file)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close() //nolint:errcheck
	reader := tar.NewReader(stream)
	for {
		entry, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimPrefix(entry.Name, "./") == wanted {
			return true
		}
	}
}

func readLayerHeader(t *testing.T, blobPath, wanted string) *tar.Header {
	t.Helper()
	file, err := os.Open(blobPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close layer: %v", err)
		}
	}() //nolint:errcheck
	stream, err := storagearchive.DecompressStream(file)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close() //nolint:errcheck
	reader := tar.NewReader(stream)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			t.Fatalf("layer does not contain %q", wanted)
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSuffix(strings.TrimPrefix(header.Name, "./"), "/") == strings.TrimSuffix(wanted, "/") {
			copy := *header
			return &copy
		}
	}
}

func TestBuildRemoteRootURLUsesNativeDestinationTyping(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping isolated Buildah root URL destinations in short mode")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("download\n")) }))
	defer server.Close()
	for _, tc := range []struct {
		name, destination, workdir string
		directory, existingFile    bool
		want                       string
	}{
		{name: "missing file", destination: "/payload", want: "payload"},
		{name: "existing file", destination: "/payload", existingFile: true, want: "payload"},
		{name: "relative file", destination: "payload", workdir: "/nested", want: "nested/payload"},
		{name: "existing directory", destination: "/directory", directory: true},
		{name: "trailing slash", destination: "/directory/"},
		{name: "workdir", destination: ".", workdir: "/directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			root := t.TempDir()
			contextDir := filepath.Join(root, "context")
			if err := os.Mkdir(contextDir, 0700); err != nil {
				t.Fatal(err)
			}
			source := "from \"scratch\" {\n"
			if tc.directory {
				source += "workdir \"/directory\"\nworkdir \"/\"\n"
			}
			if tc.existingFile {
				if err := os.WriteFile(filepath.Join(contextDir, "initial"), []byte("before"), 0600); err != nil {
					t.Fatal(err)
				}
				source += fmt.Sprintf("copy \"initial\" %q\n", tc.destination)
			}
			if tc.workdir != "" {
				source += fmt.Sprintf("workdir %q\n", tc.workdir)
			}
			layout := filepath.Join(root, "layout")
			_, err := BuildPlan(ctx, testPlan(t, source+fmt.Sprintf("add %q %q\n}\n", server.URL+"/", tc.destination)), PlanOptions{Store: cacheTestStore(root), ContextDir: contextDir, Isolation: "rootless", Output: Output{Path: layout, Reference: "root-url"}})
			if tc.want == "" {
				if err == nil || !strings.Contains(err.Error(), "has no filename") {
					t.Fatalf("directory error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			manifest, _ := readPlanImage(t, layout)
			layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded())
			if got := readLayerFile(t, layer, tc.want); got != "download\n" {
				t.Fatalf("payload=%q", got)
			}
		})
	}
}

func TestBuildRemoteAddMissingLastModifiedCopiesEpoch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping copied HTTP metadata in short mode")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("download\n")) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	layout := filepath.Join(root, "layout")
	_, err := BuildPlan(ctx, testPlan(t, fmt.Sprintf(`from "scratch" { add %q "/payload"; }`, server.URL+"/payload")), PlanOptions{Store: cacheTestStore(root), ContextDir: root, Isolation: "rootless", Output: Output{Path: layout, Reference: "epoch"}})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	header := readLayerHeader(t, filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded()), "payload")
	if !header.ModTime.Equal(time.Unix(0, 0)) {
		t.Fatalf("copied mtime=%v", header.ModTime)
	}
}
