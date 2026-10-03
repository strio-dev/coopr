package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveBuildInput(t *testing.T) {
	root := t.TempDir()
	definition := filepath.Join(root, "custom.coopr")
	if err := os.WriteFile(definition, []byte("from \"scratch\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, context, _, err := resolveBuildInput(definition, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if file != definition || context != root {
		t.Fatalf("got (%q,%q)", file, context)
	}

}

func TestResolveBuildInputReadsExplicitExtractedContextDefinitions(t *testing.T) {
	for _, source := range []string{"https://registry.invalid/context.tar", "https://git.invalid/project.git", "-"} {
		file, context, inContext, err := resolveBuildInput(source, "custom-definition", "")
		if err != nil || file != "custom-definition" || context != source || !inContext {
			t.Fatalf("input %q = (%q,%q,%t), %v", source, file, context, inContext, err)
		}
	}
}

func TestResolveBuildInputKeepsExplicitHTTPDefinitionOutsideContext(t *testing.T) {
	definition := "https://example.invalid/custom.coopr"
	for _, contextSource := range []string{".", "https://example.invalid/context.tar"} {
		file, context, inContext, err := resolveBuildInput(contextSource, definition, "")
		if err != nil {
			t.Fatal(err)
		}
		if file != definition || context != contextSource || inContext {
			t.Fatalf("context %q = (%q,%q,%t), want explicit remote definition", contextSource, file, context, inContext)
		}
	}
}

func TestBuildArgFilesPrecedeExplicitArguments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "args")
	if err := os.WriteFile(path, []byte("one=file\ntwo=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readBuildArgFiles([]string{path}, map[string]string{"one": "flag"})
	if err != nil {
		t.Fatal(err)
	}
	if got["one"] != "flag" || got["two"] != "value" {
		t.Fatalf("args = %#v", got)
	}
}

func TestResolveBuildInputRequiresExplicitDefinition(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"image.coopr", "component.coopr"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("from \"scratch\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	archiveFile := filepath.Join(root, "context.tar")
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	if err := writer.WriteHeader(&tar.Header{Name: "proof", Mode: 0600, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archiveFile, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	for _, argument := range []string{"", root, archiveFile, "https://example.invalid/context.tar", "https://git.invalid/project.git", "-"} {
		t.Run(argument, func(t *testing.T) {
			_, _, _, err := resolveBuildInput(argument, "", "")
			if err == nil || !strings.Contains(err.Error(), "definition file is required") {
				t.Fatalf("input %q: got %v, want required definition error", argument, err)
			}
		})
	}
}

func TestResolveBuildInputKeepsExactExtensionlessPaths(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"definition", "missing.coopr"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("from \"scratch\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"definition", "missing"} {
		path := filepath.Join(root, name)
		file, context, extracted, err := resolveBuildInput(path, "", "")
		if err != nil || file != path || context != root || extracted {
			t.Fatalf("input %q: (%q,%q,%t), %v", path, file, context, extracted, err)
		}
	}
}

func TestResolveBuildInputExplicitFileContextAndStdin(t *testing.T) {
	root := t.TempDir()
	for _, file := range []string{"-", filepath.Join(root, "definition")} {
		resolved, context, extracted, err := resolveBuildInput("", file, root)
		if err != nil || resolved != file || context != root || extracted {
			t.Fatalf("(%q,%q,%t), %v", resolved, context, extracted, err)
		}
	}
	if _, _, _, err := resolveBuildInput(root, "definition", root); err == nil || !strings.Contains(err.Error(), "context specified both") {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestBuildCommandsRequireDefinitionFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	for _, args := range [][]string{{"build"}, {"component", "build"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "definition file is required") {
			t.Fatalf("%v: code=%d, stderr=%q", args, code, stderr.String())
		}
	}
}
