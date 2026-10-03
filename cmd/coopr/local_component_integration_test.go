package main

import (
	"archive/tar"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildCommandConsumesLocalComponentsFromDirectoryAndArchiveContexts(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for local component builds")
	}
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	files := map[string]string{
		"image.coopr": "from \"scratch\"\ncomponent \"./components/shared.coopr\" channel=\"archive\"\n",
		"components/shared.coopr": "package as=\"settings\"\ncopy \"settings.conf\" \"/settings.conf\"\n" +
			"extend\narg \"channel\"\nenv channel=\"${channel}\"\ncopy \"/settings.conf\" \"/etc/shared.conf\" from=\"settings\"\n",
		"settings.conf": "shared local settings\n",
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for name, data := range files {
		file := filepath.Join(contextDir, name)
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteHeader(&tar.Header{Name: name, Size: int64(len(data)), Mode: 0o600}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	archiveFile := filepath.Join(root, "context.tar")
	if err := os.WriteFile(archiveFile, archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive.Bytes())
	}))
	defer server.Close()
	for name, source := range map[string]string{
		"directory": contextDir, "archive": archiveFile, "http-archive": server.URL + "/context.tar",
	} {
		t.Run(name, func(t *testing.T) {
			var output, stderr bytes.Buffer
			destination := filepath.Join(root, "output-"+name)
			definition := "image.coopr"
			if name == "directory" {
				definition = filepath.Join(contextDir, definition)
			}
			if code := run([]string{"build", source, "--file", definition, "--quiet", "--pull=never", "--output", destination}, &output, &stderr); code != 0 {
				t.Fatalf("local component build failed: %s", stderr.String())
			}
			data, err := os.ReadFile(filepath.Join(destination, "etc/shared.conf"))
			if err != nil || string(data) != files["settings.conf"] {
				t.Fatalf("component output = %q, %v", data, err)
			}
		})
	}
}

func TestBuildCommandTwoDefinitionsShareLocalComponentFromExplicitContext(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for local component builds")
	}
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	definitions := filepath.Join(contextDir, "containers")
	if err := os.MkdirAll(definitions, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"shared.coopr":  "package as=\"settings\"\ncopy \"settings.conf\" \"/settings.conf\"\nextend\ncopy \"/settings.conf\" \"/etc/shared.conf\" from=\"settings\"\n",
		"settings.conf": "same settings\n",
	} {
		if err := os.WriteFile(filepath.Join(contextDir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"api", "worker"} {
		file := filepath.Join(definitions, name+".coopr")
		if err := os.WriteFile(file, []byte("from \"scratch\"\ncomponent \"./shared.coopr\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var output, stderr bytes.Buffer
		destination := filepath.Join(root, "output-"+name)
		if code := run([]string{"build", "-f", file, contextDir, "--quiet", "--pull=never", "--output", destination}, &output, &stderr); code != 0 {
			t.Fatalf("%s local component build failed: %s", name, stderr.String())
		}
		data, err := os.ReadFile(filepath.Join(destination, "etc/shared.conf"))
		if err != nil || string(data) != "same settings\n" {
			t.Fatalf("%s shared settings = %q, %v", name, data, err)
		}
	}
}
