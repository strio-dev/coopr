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

	"coopr/internal/buildah"
)

func TestCompletedStdoutTarSurvivesLaterPublicationFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native build integration")
	}
	root := dockerEngineWorkspace(t)
	file := filepath.Join(root, "image.coopr")
	if err := os.WriteFile(file, []byte("from \"scratch\"\ncopy \"proof\" \"/proof\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proof"), []byte("completed"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "publication refused", http.StatusForbidden)
	}))
	defer server.Close()
	authority := strings.TrimPrefix(server.URL, "http://")
	metadata := filepath.Join(root, "metadata.json")
	var stdout bytes.Buffer
	_, err := Run(context.Background(), Options{File: file, Context: root, BuildStore: nativeBuildTestStore(filepath.Join(root, "images")), Quiet: true, Stdout: &stdout,
		Output: buildah.FilesystemOutput{Type: "tar", Path: "-"}, Tags: []string{"completed:latest", "registry:" + authority + "/coopr/refused:latest"},
		TLSVerify: new(false), MetadataFile: metadata, RetrySet: true,
	})
	if err == nil {
		t.Fatal("failed publication reported success")
	}
	reader := tar.NewReader(bytes.NewReader(stdout.Bytes()))
	found := false
	for {
		header, readErr := reader.Next()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.TrimPrefix(header.Name, "./") == "proof" {
			data, readErr := io.ReadAll(reader)
			if readErr != nil || string(data) != "completed" {
				t.Fatalf("completed tar payload=%q, %v", data, readErr)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("completed tar output was withheld after publication failure")
	}
	data, readErr := os.ReadFile(metadata)
	if readErr != nil || !bytes.Contains(data, []byte(`"coopr.outputError"`)) || !bytes.Contains(data, []byte(`"status": "complete"`)) || !bytes.Contains(data, []byte(`"status": "failed"`)) {
		t.Fatalf("publication failure metadata=%s, %v", data, readErr)
	}
}
