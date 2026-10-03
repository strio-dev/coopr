package build

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHTTPDefinitionErrorsRedactCredentialsAcrossBuildModes(t *testing.T) {
	for _, failure := range []string{"download", "parse", "build"} {
		for _, component := range []bool{false, true} {
			t.Run(failure+map[bool]string{false: "/image", true: "/component"}[component], func(t *testing.T) {
				if failure == "build" && os.Getenv("COOPR_TEST_BUILDAH") == "" {
					t.Skip("set COOPR_TEST_BUILDAH for late native failure")
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					switch failure {
					case "download":
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
					case "parse":
						_, _ = io.WriteString(w, "invalid {\n")
					case "build":
						if component {
							_, _ = io.WriteString(w, "package as=\"bundle\"\ncopy \"missing\" \"/missing\"\nextend\ncopy \"/missing\" \"/missing\" from=\"bundle\"\n")
						} else {
							_, _ = io.WriteString(w, "from \"scratch\"\ncopy \"missing\" \"/missing\"\n")
						}
					}
				}))
				defer server.Close()
				source := strings.Replace(server.URL, "http://", "http://user:secret@", 1) + "/definition.coopr"
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				var err error
				if component {
					_, err = BuildComponent(ctx, ComponentOptions{File: source, Context: t.TempDir()})
				} else {
					_, err = Run(ctx, Options{File: source, Context: t.TempDir()})
				}
				if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), definitionDisplayName(source)) {
					t.Fatalf("%s error must identify the redacted definition: %v", failure, err)
				}
			})
		}
	}
}

func TestDefinitionStdinRetainsModeValidation(t *testing.T) {
	_, err := Run(context.Background(), Options{File: "-", Stdin: strings.NewReader("extend\n")})
	if err == nil || !strings.Contains(err.Error(), "requires coopr component build") {
		t.Fatalf("stdin definition did not reach mode validation: %v", err)
	}
}

func TestHTTPDefinitionReportsStatusAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "missing", http.StatusNotFound) }))
	defer server.Close()
	if _, err := readDefinition(context.Background(), server.URL+"/definition.coopr", nil); err == nil || !strings.Contains(err.Error(), "HTTP status 404") {
		t.Fatalf("HTTP definition status error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readDefinition(canceled, server.URL+"/definition.coopr", nil); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("HTTP definition cancellation error = %v", err)
	}
}

func TestHTTPDefinitionSupportsURLBasicAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "user" || password != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "from \"scratch\"\n")
	}))
	defer server.Close()
	source := strings.Replace(server.URL, "http://", "http://user:secret@", 1) + "/definition.coopr"
	if _, err := readDefinition(context.Background(), source, nil); err != nil {
		t.Fatalf("authenticated HTTP definition: %v", err)
	}
}

func TestHTTPDefinitionDoesNotProbeURLAsLocalPath(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.WriteFile("http:", []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	source := strings.Replace(server.URL, "http://", "http://user:secret@", 1) + "/definition.coopr"
	for _, component := range []bool{false, true} {
		var err error
		if component {
			_, err = BuildComponent(context.Background(), ComponentOptions{File: source, Context: root, Tag: "oci-archive:" + filepath.Join(root, "component.tar")})
		} else {
			_, err = Run(context.Background(), Options{File: source, Context: root, Tag: "oci-archive:" + filepath.Join(root, "image.tar")})
		}
		if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "HTTP status 503") {
			t.Fatalf("remote definition was treated as a local path: %v", err)
		}
	}
}

func TestDefinitionLocalPathWithURLReservedCharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local%definition.coopr")
	if err := os.WriteFile(path, []byte("from \"scratch\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readDefinition(context.Background(), path, nil); err != nil {
		t.Fatalf("local definition parsed as URL: %v", err)
	}
}

func TestDefinitionAndContextCannotBothReadStdin(t *testing.T) {
	_, err := Run(context.Background(), Options{File: "-", Context: "-", Stdin: strings.NewReader("from \"scratch\"\n")})
	if err == nil || !strings.Contains(err.Error(), "both use stdin") {
		t.Fatalf("ambiguous stdin inputs accepted: %v", err)
	}
}

func TestPrimaryContextFromArchiveAndStdin(t *testing.T) {
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	content := []byte("archive input\n")
	if err := w.WriteHeader(&tar.Header{Name: "nested/input.txt", Mode: 0640, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "context.tar")
	if err := os.WriteFile(path, archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{path, "-"} {
		t.Run(source, func(t *testing.T) {
			primary, cleanup, err := preparePrimaryContext(context.Background(), "image.coopr", source, nil, nil, bytes.NewReader(archive.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cleanup() }()
			got, err := os.ReadFile(filepath.Join(primary.Path, "nested/input.txt"))
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("extracted context = %q, %v", got, err)
			}
			if err := cleanup(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(primary.Path); !os.IsNotExist(err) {
				t.Fatalf("context remains after cleanup: %v", err)
			}
		})
	}
}

func TestPrimaryContextArchiveRejectsTraversal(t *testing.T) {
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	if err := w.WriteHeader(&tar.Header{Name: "../escape", Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, err := preparePrimaryContext(context.Background(), "image.coopr", "-", nil, nil, bytes.NewReader(archive.Bytes()))
	if err == nil || !strings.Contains(err.Error(), "non-local path") {
		t.Fatalf("archive traversal accepted: %v", err)
	}
}

func TestArchiveContextDefinitionRejectsTraversal(t *testing.T) {
	for _, name := range []string{"image.coopr", "component.coopr"} {
		t.Run(name, func(t *testing.T) {
			outsideDir, err := os.MkdirTemp("", "coopr-outside-definition-")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.RemoveAll(outsideDir) }()
			outsidePath := filepath.Join(outsideDir, name)
			if err := os.WriteFile(outsidePath, []byte("from \"scratch\"\n"), 0600); err != nil {
				t.Fatal(err)
			}

			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			definitionName := filepath.Join("..", filepath.Base(outsideDir), name)
			_, _, _, _, err = prepareDefinitionContext(context.Background(), definitionName, "-", true, nil, nil, bytes.NewReader(archive.Bytes()))
			if err == nil || !strings.Contains(err.Error(), "local relative path") {
				t.Fatalf("archive definition traversal %q accepted: %v", definitionName, err)
			}
		})
	}
}

func TestArchiveContextDefinitionRejectsEscapingSymlink(t *testing.T) {
	for _, name := range []string{"image.coopr", "component.coopr"} {
		t.Run(name, func(t *testing.T) {
			outside := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(outside, []byte("from \"scratch\"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			if err := writer.WriteHeader(&tar.Header{Name: name, Linkname: outside, Typeflag: tar.TypeSymlink, Mode: 0777}); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			_, _, _, _, err := prepareDefinitionContext(context.Background(), name, "-", true, nil, nil, bytes.NewReader(archive.Bytes()))
			if err == nil || !strings.Contains(err.Error(), "outside build context") {
				t.Fatalf("archive definition symlink to %q accepted: %v", outside, err)
			}
		})
	}
}

func TestContextIgnoreFileSelection(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"image.coopr.dockerignore", "image.coopr.containerignore", ".cooprignore", ".containerignore", ".dockerignore"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	explicit := filepath.Join(root, "image.coopr.dockerignore")
	got, err := selectIgnoreFile(root, explicit)
	if err != nil || got != explicit {
		t.Fatalf("explicit ignore = %q, %v", got, err)
	}
	for _, name := range []string{".cooprignore", ".containerignore", ".dockerignore"} {
		want := filepath.Join(root, name)
		got, err := selectIgnoreFile(root, "")
		if err != nil || got != want {
			t.Fatalf("context ignore = %q, %v; want %q", got, err, want)
		}
		if err := os.Remove(want); err != nil {
			t.Fatal(err)
		}
	}
	got, err = selectIgnoreFile(root, "")
	if err != nil || got != "" {
		t.Fatalf("legacy ignore selected = %q, %v", got, err)
	}
	if _, err := selectIgnoreFile(root, filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing explicit ignore accepted")
	}
}
