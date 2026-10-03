package enginecopy

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/localstore"
	"coopr/internal/oci"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestReadDockerPreservesIndexAndSelectsPlatform(t *testing.T) {
	ctx := context.Background()
	layout, root, children := testIndexLayout(t)
	source, err := localstore.Open(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := localstore.WriteArchiveTo(ctx, source, root, &archive); err != nil {
		t.Fatal(err)
	}
	server := startDockerAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/_ping":
			writeDockerPing(w)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info"):
			writeDockerInfo(w, true)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/images/get"):
			if r.URL.Query().Get("names") != "example:dev" {
				t.Errorf("saved wrong image: %s", r.URL)
			}
			_, _ = w.Write(archive.Bytes())
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	t.Setenv("DOCKER_HOST", server)
	for _, platform := range []v1.Platform{{}, {OS: "linux", Architecture: "arm64"}} {
		destination := filepath.Join(t.TempDir(), "import")
		got, err := Read(ctx, "docker", "example:dev", destination, platform)
		if err != nil {
			t.Fatal(err)
		}
		want := root
		if platform.Architecture != "" {
			want = children[1]
		}
		if !sameContentDescriptor(got, want) {
			t.Fatalf("imported %+v, want %+v", got, want)
		}
		stored, err := oci.LayoutRoot(destination)
		if err != nil || !sameContentDescriptor(stored, want) {
			t.Fatalf("retained descriptor = %+v, %v", stored, err)
		}
		if matches, _ := filepath.Glob(filepath.Join(destination, "*.tar")); len(matches) != 0 {
			t.Fatal("import persisted an archive")
		}
	}
	if _, err := os.Stat(layout); err != nil {
		t.Fatal("engine import modified source")
	}
}
