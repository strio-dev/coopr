package buildah

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coopr/internal/definition"
	"github.com/opencontainers/go-digest"
	storagearchive "go.podman.io/storage/pkg/archive"
)

func TestBuildMultipleCopyAndAddSourcesUseNativeDestinationSemantics(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah COPY/ADD build")
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
	_, err := Build(ctx, Request{
		Store: store, Base: "scratch", ContextDir: contextDir, Isolation: "rootless",
		Operations: []Operation{
			WorkDir("/copy-dot"),
			Copy{Sources: []string{"one", "two"}, Destination: "."},
			WorkDir("/add-dot"),
			Add{Sources: []string{"one", "two"}, Destination: "."},
			WorkDir("/copy-existing"), WorkDir("/"),
			Copy{Sources: []string{"one", "two"}, Destination: "/copy-existing"},
			WorkDir("/add-existing"), WorkDir("/"),
			Add{Sources: []string{"one", "two"}, Destination: "/add-existing"},
		},
		Output: Output{Path: layout, Reference: "multi-source-destinations"},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("layers = %d, want 1", len(manifest.Layers))
	}
	layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	for _, directory := range []string{"copy-dot", "add-dot", "copy-existing", "add-existing"} {
		for _, name := range []string{"one", "two"} {
			if got := readLayerFile(t, layer, directory+"/"+name); got != name+"\n" {
				t.Fatalf("%s/%s = %q", directory, name, got)
			}
		}
	}

	for _, test := range []struct {
		name      string
		operation Operation
	}{
		{name: "copy", operation: Copy{Sources: []string{"one", "two"}, Destination: "/target"}},
		{name: "add", operation: Add{Sources: []string{"one", "two"}, Destination: "/target"}},
	} {
		t.Run(test.name+" rejects existing file", func(t *testing.T) {
			_, err := Build(ctx, Request{
				Store: store, Base: "scratch", ContextDir: contextDir, Isolation: "rootless",
				Operations: []Operation{
					Copy{Sources: []string{"one"}, Destination: "/target"},
					test.operation,
				},
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
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah mixed COPY/ADD build")
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
	remoteSources := []string{server.URL + "/remote-one", server.URL + "/remote-two"}
	mixedCopy := func(destination string) Copy {
		return Copy{
			Sources: []string{"local"}, Destination: destination,
			InlineFiles: []definition.InlineFile{{Path: "inline", Data: "inline\n"}},
		}
	}
	layout := filepath.Join(root, "split-layout")
	_, err := Build(ctx, Request{
		Store: store, Base: "scratch", ContextDir: contextDir, Isolation: "rootless",
		Operations: []Operation{
			WorkDir("/mixed-dot"), mixedCopy("."),
			WorkDir("/remote-dot"), Add{Sources: remoteSources, Destination: "."},
			WorkDir("/mixed-existing"), WorkDir("/"), mixedCopy("/mixed-existing"),
			WorkDir("/remote-existing"), WorkDir("/"), Add{Sources: remoteSources, Destination: "/remote-existing"},
			User("1000"), mixedCopy("/owner-default"),
			Copy{
				Sources: []string{"local"}, Destination: "/owner-explicit", Chown: "12:34",
				InlineFiles: []definition.InlineFile{{Path: "inline", Data: "inline\n"}},
			},
		},
		Output: Output{Path: layout, Reference: "split-multi-source-destinations"},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
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

	for _, test := range []struct {
		name      string
		operation Operation
	}{
		{name: "mixed-copy", operation: mixedCopy("/target")},
		{name: "remote-add", operation: Add{Sources: remoteSources, Destination: "/target"}},
	} {
		t.Run(test.name+" rejects existing file", func(t *testing.T) {
			_, err := Build(ctx, Request{
				Store: store, Base: "scratch", ContextDir: contextDir, Isolation: "rootless",
				Operations: []Operation{
					Copy{Sources: []string{"local"}, Destination: "/target"},
					test.operation,
				},
				Output: Output{Path: filepath.Join(root, "split-invalid-"+test.name), Reference: "split-invalid-" + test.name},
			})
			if err == nil {
				t.Fatal("accepted split multiple sources with an existing file destination")
			}
		})
	}
}

func TestBuildPlanUnpacksRemoteArchive(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah remote ADD build")
	}
	archive := tarBytes(t, "proof", "remote unpack\n")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(archive)
	}))
	defer server.Close()

	plan := testPlan(t, "from \"scratch\"\nadd \""+server.URL+"/archive.tar\" \"/unpacked/\" unpack=\"true\" checksum=\""+digest.FromBytes(archive).String()+"\"\n")
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
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah remote ADD build")
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
	plan := testPlan(t, "from \"scratch\"\nadd \""+server.URL+"/payload\" \"/payload\"\n")
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
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah Git ADD build")
	}
	source, commit := gitProtocolFixture(t)
	plan := testPlan(t, `
from "scratch"
add "`+source+`" "/without/" checksum="`+commit[:12]+`"
add "`+source+`" "/with/" checksum="`+commit[:12]+`" keep-git-dir="true"
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
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah Git submodule ADD build")
	}
	source, _ := gitHTTPSubmoduleFixture(t, "")
	plan := testPlan(t, "from \"scratch\"\nadd \""+source+"\" \"/source/\"\n")
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
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for isolated Buildah root URL destinations")
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
			operations := []Operation{}
			if tc.directory {
				operations = append(operations, WorkDir("/directory"), WorkDir("/"))
			}
			if tc.existingFile {
				if err := os.WriteFile(filepath.Join(contextDir, "initial"), []byte("before"), 0600); err != nil {
					t.Fatal(err)
				}
				operations = append(operations, Copy{Sources: []string{"initial"}, Destination: tc.destination})
			}
			if tc.workdir != "" {
				operations = append(operations, WorkDir(tc.workdir))
			}
			operations = append(operations, Add{Sources: []string{server.URL + "/"}, Destination: tc.destination})
			layout := filepath.Join(root, "layout")
			_, err := Build(ctx, Request{Store: cacheTestStore(root), Base: "scratch", ContextDir: contextDir, Isolation: "rootless", Operations: operations, Output: Output{Path: layout, Reference: "root-url"}})
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
			layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
			if got := readLayerFile(t, layer, tc.want); got != "download\n" {
				t.Fatalf("payload=%q", got)
			}
		})
	}
}

func TestBuildRemoteAddMissingLastModifiedCopiesEpoch(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for copied HTTP metadata")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("download\n")) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	layout := filepath.Join(root, "layout")
	_, err := Build(ctx, Request{Store: cacheTestStore(root), Base: "scratch", ContextDir: root, Isolation: "rootless", Operations: []Operation{Add{Sources: []string{server.URL + "/payload"}, Destination: "/payload"}}, Output: Output{Path: layout, Reference: "epoch"}})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	file, err := os.Open(filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close layer: %v", err)
		}
	}()
	reader, err := storagearchive.DecompressStream(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Errorf("close layer reader: %v", err)
		}
	}()
	archive := tar.NewReader(reader)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			t.Fatal("copied payload absent")
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimPrefix(header.Name, "./") == "payload" {
			if !header.ModTime.Equal(time.Unix(0, 0)) {
				t.Fatalf("copied mtime=%v", header.ModTime)
			}
			return
		}
	}
}
