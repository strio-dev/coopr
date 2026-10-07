package build

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestBuildArchiveAndDefinitionStdinWithMultipleDestinations(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for live input/output tests")
	}
	root := t.TempDir()
	definition := filepath.Join(root, "image.coopr")
	script := "from \"scratch\"\ncopy \"payload\" \"/payload\"\n"
	if err := os.WriteFile(definition, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("stdin fixture\n"), 0640); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	data := []byte("stdin fixture\n")
	if err := w.WriteHeader(&tar.Header{Name: "payload", Mode: 0640, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "context.tar")
	if err := os.WriteFile(path, archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"definition-stdin", "definition-and-run-stdin", "context-stdin", "context-and-run-stdin", "local-tar"} {
		t.Run(mode, func(t *testing.T) {
			store := filepath.Join(os.Getenv("XDG_DATA_HOME"), "input-fixtures", strings.ReplaceAll(t.Name(), "/", "-"))
			output := filepath.Join(t.TempDir(), "output.oci.tar")
			metadata := filepath.Join(t.TempDir(), "metadata.json")
			opts := Options{File: definition, Context: root, BuildStore: nativeBuildTestStore(store), Platform: "linux/" + runtime.GOARCH, Jobs: 1,
				Tags: []string{"app:one", "app:two", "oci-archive:" + output}, MetadataFile: metadata}
			switch mode {
			case "definition-stdin", "definition-and-run-stdin":
				opts.File, opts.Stdin = "-", strings.NewReader(script)
			case "context-stdin", "context-and-run-stdin":
				opts.Context, opts.Stdin = "-", bytes.NewReader(archive.Bytes())
			case "local-tar":
				opts.Context = path
			}
			if strings.Contains(mode, "and-run-stdin") {
				opts.RunStdin = opts.Stdin
			}
			result, err := Run(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(strings.Split(result, "\n")) != 3 {
				t.Fatalf("multiple result references = %q", result)
			}
			platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
			first, found, err := testStoredImageSelection(context.Background(), nativeBuildTestStore(store), "app:one", platform)
			if err != nil || !found {
				t.Fatalf("first local tag: %v, %t", err, found)
			}
			second, found, err := testStoredImageSelection(context.Background(), nativeBuildTestStore(store), "app:two", platform)
			if err != nil || !found || second.ImageID != first.ImageID || second.Manifest.Digest != first.Manifest.Digest {
				t.Fatalf("tags rebuilt or differ: %+v/%+v found=%t err=%v", first, second, found, err)
			}
			contents, err := os.ReadFile(metadata)
			if err != nil {
				t.Fatal(err)
			}
			var record map[string]json.RawMessage
			if err := json.Unmarshal(contents, &record); err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(record["coopr.platforms"], []byte(first.Manifest.Digest.String())) {
				t.Fatalf("metadata manifest differs: %s", contents)
			}
			if !containsName(archiveLayerNames(t, output), "payload") {
				t.Fatal("archive input payload missing")
			}
		})
	}
}

func TestBuildCooprIgnoreOverridesDefaultsAndIgnoresLegacy(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for live ignore tests")
	}
	root := t.TempDir()
	file := filepath.Join(root, "image.coopr")
	for name, data := range map[string]string{"image.coopr": "from \"scratch\"\ncopy \".\" \"/\"\n", ".dockerignore": "default-only\n", "image.coopr.dockerignore": "legacy-only\n", ".cooprignore": "specific-only\n", ".containerignore": "container-only\n", "legacy-only": "kept", "container-only": "kept", "default-only": "kept", "specific-only": "excluded"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(t.TempDir(), "output.oci.tar")
	if _, err := Run(context.Background(), Options{File: file, Tag: "oci-archive:" + output, Platform: "linux/" + runtime.GOARCH, Jobs: 1}); err != nil {
		t.Fatal(err)
	}
	names := archiveLayerNames(t, output)
	if !containsName(names, "default-only") || !containsName(names, "legacy-only") || !containsName(names, "container-only") || containsName(names, "specific-only") {
		t.Fatalf("coopr ignore did not replace defaults: %v", names)
	}
}
