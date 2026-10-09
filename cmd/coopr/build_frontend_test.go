package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestResolveBuildInput(t *testing.T) {
	root := t.TempDir()
	definition := filepath.Join(root, "custom.coopr")
	if err := os.WriteFile(definition, []byte("from \"scratch\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, context, _, err := resolveBuildInput(definition, "")
	if err != nil {
		t.Fatal(err)
	}
	if file != definition || context != root {
		t.Fatalf("got (%q,%q)", file, context)
	}

}

func TestResolveBuildInputReadsExplicitExtractedContextDefinitions(t *testing.T) {
	for _, source := range []string{"https://registry.invalid/context.tar", "https://git.invalid/project.git", "-"} {
		file, context, inContext, err := resolveBuildInput(source, "custom-definition")
		if err != nil || file != "custom-definition" || context != source || !inContext {
			t.Fatalf("input %q = (%q,%q,%t), %v", source, file, context, inContext, err)
		}
	}
}

func TestResolveBuildInputKeepsExplicitHTTPDefinitionOutsideContext(t *testing.T) {
	definition := "https://example.invalid/custom.coopr"
	for _, contextSource := range []string{".", "https://example.invalid/context.tar"} {
		file, context, inContext, err := resolveBuildInput(contextSource, definition)
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
	got, err := readBuildArgFiles([]string{path}, []string{"one=flag"})
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
			_, _, _, err := resolveBuildInput(argument, "")
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
		file, context, extracted, err := resolveBuildInput(path, "")
		if err != nil || file != path || context != root || extracted {
			t.Fatalf("input %q: (%q,%q,%t), %v", path, file, context, extracted, err)
		}
	}
}

func TestResolveBuildInputExplicitFileContextAndStdin(t *testing.T) {
	root := t.TempDir()
	for _, file := range []string{"-", filepath.Join(root, "definition")} {
		resolved, context, extracted, err := resolveBuildInput(root, file)
		if err != nil || resolved != file || context != root || extracted {
			t.Fatalf("(%q,%q,%t), %v", resolved, context, extracted, err)
		}
	}
	resolved, context, extracted, err := resolveBuildInput("", "-")
	if err != nil || resolved != "-" || context != "." || extracted {
		t.Fatalf("stdin definition default = (%q,%q,%t), %v", resolved, context, extracted, err)
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

func TestBuildArgFilesPreserveOrderedCLIUnset(t *testing.T) {
	const name = "COOPR_TEST_ARG_UNSET"
	t.Setenv(name, "temporary")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "args")
	if err := os.WriteFile(path, []byte(name+"=file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, explicit := range [][]string{{name}, {name + "=explicit", name}, {name, name + "=last"}} {
		got, err := readBuildArgFiles([]string{path}, explicit)
		if err != nil {
			t.Fatal(err)
		}
		if explicit[len(explicit)-1] == name {
			if _, exists := got[name]; exists {
				t.Fatalf("%v retained file value: %v", explicit, got)
			}
		} else if got[name] != "last" {
			t.Fatalf("%v: %v", explicit, got)
		}
	}
}

func TestBuildArgFilesAcceptLongLines(t *testing.T) {
	value := strings.Repeat("x", 70000)
	path := filepath.Join(t.TempDir(), "args")
	if err := os.WriteFile(path, []byte("LARGE="+value+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readBuildArgFiles([]string{path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got["LARGE"] != value {
		t.Fatal("long argument changed")
	}
}

func TestBuildCommandsRetainRawOrderedArguments(t *testing.T) {
	const name = "COOPR_TEST_RAW_ARGS"
	t.Setenv(name, "temporary")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "args")
	if err := os.WriteFile(path, []byte(name+"=file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []*cobra.Command{newBuildCommand(), newComponentBuildCommand()} {
		if err := cmd.ParseFlags([]string{"--build-arg=" + name + "=explicit", "--build-arg=" + name}); err != nil {
			t.Fatal(err)
		}
		raw, err := cmd.Flags().GetStringArray("build-arg")
		if err != nil {
			t.Fatal(err)
		}
		got, err := readBuildArgFiles([]string{path}, raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := got[name]; exists {
			t.Fatalf("%s retained deleted file argument", cmd.Name())
		}
	}
}

func TestBuildArgFilesPreserveEnvironmentEmptyAndLineSemantics(t *testing.T) {
	t.Setenv("COOPR_TEST_FILE_ENV", "environment")
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first"), filepath.Join(dir, "second")
	if err := os.WriteFile(first, []byte("#comment\r\n\r\nVALUE=first\r\nCOOPR_TEST_FILE_ENV\r\nEMPTY=\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("VALUE=second"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readBuildArgFiles([]string{first, second}, []string{"VALUE=explicit"})
	if err != nil {
		t.Fatal(err)
	}
	if got["VALUE"] != "explicit" || got["COOPR_TEST_FILE_ENV"] != "environment" {
		t.Fatalf("arguments: %v", got)
	}
	if value, exists := got["EMPTY"]; !exists || value != "" {
		t.Fatalf("empty value lost: %v", got)
	}
}

func TestBuildArgFilesReportReadAndArgumentErrors(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	if _, err := readBuildArgFiles([]string{missing}, nil); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing file error: %v", err)
	}
	if _, err := readBuildArgFiles([]string{dir}, nil); err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("directory read error: %v", err)
	}
	invalid := filepath.Join(dir, "invalid")
	if err := os.WriteFile(invalid, []byte("BAD NAME=value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBuildArgFiles([]string{invalid}, nil); err == nil || !strings.Contains(err.Error(), invalid) {
		t.Fatalf("invalid file argument error: %v", err)
	}
	for _, value := range []string{"=value", "BAD NAME=value", "UTF8=\xff"} {
		if _, err := readBuildArgFiles(nil, []string{value}); err == nil {
			t.Fatalf("invalid explicit argument accepted: %q", value)
		}
	}
}

func TestResolveBuildInputsKeepsRepeatedExplicitFiles(t *testing.T) {
	root := t.TempDir()
	files := []string{filepath.Join(root, "first.coopr"), filepath.Join(root, "second.coopr")}
	for _, file := range files {
		if err := os.WriteFile(file, []byte("from \"scratch\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, context, inContext, err := resolveBuildInputs(root, files)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != strings.Join(files, ",") || context != root || len(inContext) != 2 {
		t.Fatalf("inputs: %v %s %v", got, context, inContext)
	}
	command := newBuildCommand()
	if err := command.ParseFlags([]string{"-f", files[0], "-f", files[1]}); err != nil {
		t.Fatal(err)
	}
	parsed, err := command.Flags().GetStringArray("file")
	if err != nil || strings.Join(parsed, ",") != strings.Join(files, ",") {
		t.Fatalf("repeated files: %v %v", parsed, err)
	}
}

func TestResolveBuildInputsReadsContextRelativeFiles(t *testing.T) {
	root := t.TempDir()
	t.Chdir(t.TempDir())
	if err := os.WriteFile(filepath.Join(root, "custom.coopr"), []byte("from \"scratch\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	files, _, contained, err := resolveBuildInputs(root, []string{"custom.coopr"})
	if err != nil || len(files) != 1 || files[0] != "custom.coopr" || !contained[0] {
		t.Fatalf("context-relative definition: %v %v %v", files, contained, err)
	}
}
