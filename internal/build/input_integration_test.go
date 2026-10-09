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
	if testing.Short() {
		t.Skip("requires native build integration")
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
	if testing.Short() {
		t.Skip("requires native build integration")
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

func TestBuildCombinesDefinitionsAndWritesRawIID(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native build integration")
	}
	for _, secondFrom := range []bool{false, true} {
		t.Run(map[bool]string{false: "append", true: "new-stage"}[secondFrom], func(t *testing.T) {
			root := t.TempDir()
			first := filepath.Join(root, "first.coopr")
			second := filepath.Join(root, "second.coopr")
			firstSource := "from \"scratch\" as=\"base\"\nlabel first=\"present\"\ncopy \"payload\" \"/payload\"\n"
			secondSource := "label second=\"present\"\n"
			if secondFrom {
				secondSource = "from \"scratch\" as=\"final\"\n" + secondSource + "copy \"/payload\" \"/payload\" from=\"base\"\n"
			}
			for path, source := range map[string]string{first: firstSource, second: secondSource, filepath.Join(root, "payload"): "multifile fixture\n"} {
				if err := os.WriteFile(path, []byte(source), 0600); err != nil {
					t.Fatal(err)
				}
			}
			raw := filepath.Join(t.TempDir(), "raw-iid")
			iid := filepath.Join(t.TempDir(), "iid")
			store := nativeBuildTestStore(filepath.Join(os.Getenv("XDG_DATA_HOME"), "multi-definitions", strings.ReplaceAll(t.Name(), "/", "-")))
			if _, err := Run(context.Background(), Options{Files: []string{first, second}, Context: root, BuildStore: store, Tag: "combined:test", Platform: "linux/" + runtime.GOARCH, IIDFile: iid, IIDFileRaw: raw}); err != nil {
				t.Fatal(err)
			}
			selection, found, err := testStoredImageSelection(context.Background(), store, "combined:test", v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
			if err != nil || !found {
				t.Fatalf("combined image: %v %t", err, found)
			}
			data, err := os.ReadFile(raw)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != selection.ImageID {
				t.Fatalf("raw IID: %q", data)
			}
			data, err = os.ReadFile(iid)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "sha256:"+selection.ImageID {
				t.Fatalf("IID: %q", data)
			}
			var config v1.Image
			if err := json.Unmarshal(selection.ConfigData, &config); err != nil {
				t.Fatal(err)
			}
			if config.Config.Labels["second"] != "present" || (config.Config.Labels["first"] == "present") == secondFrom {
				t.Fatalf("combined config labels: %v", config.Config.Labels)
			}
		})
	}
}
