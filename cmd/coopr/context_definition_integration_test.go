package main

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildCommandsReadExplicitHTTPDefinitionWithLocalAndRemoteContexts(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for remote definitions")
	}
	root := t.TempDir()
	localContext := filepath.Join(root, "context")
	if err := os.Mkdir(localContext, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localContext, "proof"), []byte("remote definition\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var contextArchive bytes.Buffer
	writer := tar.NewWriter(&contextArchive)
	proof := []byte("remote definition\n")
	if err := writer.WriteHeader(&tar.Header{Name: "proof", Size: int64(len(proof)), Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(proof); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	definitions := map[string]string{
		"/image.coopr":     "from \"scratch\"\ncopy \"proof\" \"/proof\"\n",
		"/component.coopr": "package as=\"bundle\"\ncopy \"proof\" \"/proof\"\nextend\ncopy \"/proof\" \"/proof\" from=\"bundle\"\n",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if source, ok := definitions[request.URL.Path]; ok {
			_, _ = io.WriteString(w, source)
			return
		}
		if request.URL.Path == "/context.tar" {
			_, _ = w.Write(contextArchive.Bytes())
			return
		}
		http.NotFound(w, request)
	}))
	defer server.Close()

	for _, contextSource := range []string{localContext, server.URL + "/context.tar"} {
		name := "local"
		if strings.HasPrefix(contextSource, "http") {
			name = "remote"
		}
		for _, component := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "-image", true: "-component"}[component], func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				output := filepath.Join(t.TempDir(), "output")
				args := []string{"build", contextSource, "--file", server.URL + "/image.coopr", "--quiet", "--output", output}
				if component {
					args = []string{"component", "build", contextSource, "--file", server.URL + "/component.coopr", "--quiet", "--tag", "oci-archive:" + output}
				}
				if code := run(args, &stdout, &stderr); code != 0 {
					t.Fatalf("HTTP definition build failed: %s", stderr.String())
				}
				if component {
					if info, err := os.Stat(output); err != nil || info.Size() == 0 {
						t.Fatalf("component archive missing: %v", err)
					}
					return
				}
				got, err := os.ReadFile(filepath.Join(output, "proof"))
				if err != nil || !bytes.Equal(got, proof) {
					t.Fatalf("image proof = %q, %v", got, err)
				}
			})
		}
	}
}

func TestBuildCommandsReadExplicitDefinitionInsideExtractedContext(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for context-contained definitions")
	}
	root := t.TempDir()
	files := map[string]string{
		"image.coopr":     "from \"scratch\"\ncopy \"proof\" \"/proof\"\n",
		"component.coopr": "package as=\"bundle\"\ncopy \"proof\" \"/proof\"\nextend\ncopy \"/proof\" \"/proof\" from=\"bundle\"\n",
		"proof":           "context definition\n",
	}
	files["custom-image.coopr"] = files["image.coopr"]
	var data bytes.Buffer
	archive := tar.NewWriter(&data)
	working := filepath.Join(root, "working")
	if err := os.Mkdir(working, 0700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range files {
		if err := archive.WriteHeader(&tar.Header{Name: name, Size: int64(len(contents)), Mode: 0644}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(working, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "context.tar")
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data.Bytes()) }))
	defer httpServer.Close()
	git := func(args ...string) {
		t.Helper()
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
	}
	git("init", "-b", "main", working)
	git("-C", working, "add", ".")
	git("-C", working, "-c", "user.name=Coopr Test", "-c", "user.email=coopr@example.invalid", "commit", "-m", "fixture")
	git("clone", "--bare", working, filepath.Join(root, "repository.git"))
	backend, err := exec.LookPath("git-http-backend")
	if err != nil {
		t.Fatal(err)
	}
	gitServer := httptest.NewServer(&cgi.Handler{Path: backend, Root: "/", Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}})
	defer gitServer.Close()
	for name, source := range map[string]string{"tar": path, "http": httpServer.URL + "/context.tar", "git": gitServer.URL + "/repository.git"} {
		for _, component := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "-image", true: "-component"}[component], func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				output := filepath.Join(t.TempDir(), "output")
				args := []string{"build", source, "--file", "image.coopr", "--quiet", "--output", output}
				if component {
					args = []string{"component", "build", source, "--file", "component.coopr", "--quiet", "--tag", "oci-archive:" + output}
				}
				if code := run(args, &stdout, &stderr); code != 0 {
					t.Fatalf("context build failed: %s", stderr.String())
				}
				if component {
					if info, err := os.Stat(output); err != nil || info.Size() == 0 {
						t.Fatalf("component artifact missing: %v", err)
					}
				} else if proof, err := os.ReadFile(filepath.Join(output, "proof")); err != nil || string(proof) != files["proof"] {
					t.Fatalf("context COPY result=%q, %v", proof, err)
				}
			})
		}
	}
	var stdout, stderr bytes.Buffer
	output := filepath.Join(t.TempDir(), "custom")
	if code := run([]string{"build", httpServer.URL + "/context.tar", "--file", "custom-image.coopr", "--quiet", "--output", output}, &stdout, &stderr); code != 0 {
		t.Fatalf("custom context definition failed: %s", stderr.String())
	}
	if proof, err := os.ReadFile(filepath.Join(output, "proof")); err != nil || string(proof) != files["proof"] {
		t.Fatalf("custom context definition result=%q, %v", proof, err)
	}
}
