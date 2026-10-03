package build

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRejectsComponentDefinitionBeforeBuilder(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "component.coopr")
	if err := os.WriteFile(file, []byte("package as=\"payload\"\nextend as=\"base\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Options{File: file, Tag: "oci-archive:" + filepath.Join(dir, "out.tar")})
	if err == nil || !strings.Contains(err.Error(), "requires coopr component build") {
		t.Fatalf("image build did not reject component definition: %v", err)
	}
}

func TestRequestedPlatforms(t *testing.T) {
	for _, tc := range []struct {
		name, fallback string
		values         []string
		want           []string
		wantError      string
	}{
		{name: "single default", fallback: "linux/amd64", want: []string{"linux/amd64"}},
		{name: "comma separated", values: []string{"linux/amd64,linux/arm64"}, want: []string{"linux/amd64", "linux/arm64"}},
		{name: "repeated", values: []string{"linux/arm64", "linux/amd64"}, want: []string{"linux/arm64", "linux/amd64"}},
		{name: "duplicate", values: []string{"linux/arm64,linux/arm64/v8"}, wantError: "duplicate build platform"},
		{name: "empty", values: []string{"linux/amd64,"}, wantError: "invalid build platform"},
		{name: "non linux", values: []string{"windows/amd64"}, wantError: "invalid build platform"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := requestedPlatforms(tc.fallback, tc.values)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("requestedPlatforms error = %v, want %q", err, tc.wantError)
				}
				return
			}
			if err != nil || strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("requestedPlatforms = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestRunRefusesToOverwriteDefinition(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scratch.coopr")
	content := []byte("from \"scratch\"\n")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Options{File: path, Tag: "oci-archive:" + path})
	if err == nil || !strings.Contains(err.Error(), "would overwrite an input") {
		t.Fatalf("expected overwrite rejection, got %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(content) {
		t.Fatalf("definition changed: %q, %v", got, err)
	}
}

func TestRunRefusesSymlinkAliasesOfInputs(t *testing.T) {
	dir := t.TempDir()
	definitionPath := filepath.Join(dir, "app.coopr")
	definitionData := []byte("from \"scratch\"\n")
	if err := os.WriteFile(definitionPath, definitionData, 0600); err != nil {
		t.Fatal(err)
	}
	aliasDir := filepath.Join(dir, "alias-dir")
	if err := os.Symlink(dir, aliasDir); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, target string
		contents     []byte
	}{
		{"definition symlink", definitionPath, definitionData},
		{"definition parent alias", filepath.Join(aliasDir, "app.coopr"), definitionData},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := tc.target
			if !strings.Contains(tc.name, "parent") {
				output = filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".tar")
				if err := os.Symlink(tc.target, output); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Run(context.Background(), Options{File: definitionPath, Tag: "oci-archive:" + output})
			if err == nil || !strings.Contains(err.Error(), "would overwrite an input") {
				t.Fatalf("expected alias rejection before builder, got %v", err)
			}
			got, err := os.ReadFile(tc.target)
			if err != nil || string(got) != string(tc.contents) {
				t.Fatalf("input changed: %q, %v", got, err)
			}
		})
	}
}

func TestStageLayoutUsesTempWhenContextParentIsReadOnly(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "mounted")
	contextDir := filepath.Join(parent, "workspace")
	for _, dir := range []string{contextDir, filepath.Join(root, "temp")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMPDIR", filepath.Join(root, "temp"))
	if err := os.Chmod(parent, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0755) })
	layout, cleanup, err := stageLayout(filepath.Join(contextDir, "result.oci.tar"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !strings.HasPrefix(layout, filepath.Join(root, "temp")+string(filepath.Separator)) {
		t.Fatalf("staging used context parent rather than process temp: %s", layout)
	}
}

func TestStageLayoutFallsBackToOutputDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(dir, "missing-temp"))
	layout, cleanup, err := stageLayout(filepath.Join(dir, "out.oci.tar"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !strings.HasPrefix(layout, dir+string(filepath.Separator)+".coopr-stage-") {
		t.Fatalf("staging did not use writable output directory: %s", layout)
	}
}
