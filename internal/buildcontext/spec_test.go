package buildcontext

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseNormalizesSupportedContexts(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	got, err := Parse([]string{
		"Sources=./src",
		"base=docker-image://registry.example/app:latest",
		"compatible=container-image://registry.example/compatible:latest",
		"layout=oci-layout://./images/app:stable",
		"pinned=oci-layout://./images/app@sha256:0123456789abcdef",
		"repository=https://example.com/source.git#main:subdir",
		"ssh=ssh://git@example.com/source.git?branch=main",
		"scp=git@example.com:team/source.git#main",
		"archive=https://example.com/context.tar.gz",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Spec{
		{Name: "sources", Kind: Local, Path: filepath.Join(root, "src")},
		{Name: "base", Kind: DockerImage, Reference: "registry.example/app:latest"},
		{Name: "compatible", Kind: DockerImage, Reference: "registry.example/compatible:latest"},
		{Name: "layout", Kind: OCILayout, Path: filepath.Join(root, "images/app"), Reference: "stable"},
		{Name: "pinned", Kind: OCILayout, Path: filepath.Join(root, "images/app"), Reference: "sha256:0123456789abcdef"},
		{Name: "repository", Kind: Git, Reference: "https://example.com/source.git#main:subdir"},
		{Name: "ssh", Kind: Git, Reference: "ssh://git@example.com/source.git?branch=main"},
		{Name: "scp", Kind: Git, Reference: "git@example.com:team/source.git#main"},
		{Name: "archive", Kind: HTTPArchive, Reference: "https://example.com/context.tar.gz"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse() = %#v, want %#v", got, want)
	}
}

func TestParseRejectsInvalidContexts(t *testing.T) {
	for _, test := range []struct {
		value, message string
	}{
		{"missing", "expected NAME=VALUE"},
		{"=path", "expected NAME=VALUE"},
		{"name=", "expected NAME=VALUE"},
		{"bad/name=.", "invalid build context name"},
		{"0=.", "invalid build context name"},
		{"scratch=.", "reserved"},
		{"source=ssh://git:password@example.com/repo.git", "must not contain credentials"},
		{"source=https://user:secret@example.com/context.tar", "must not contain credentials"},
		{"source=ftp://example.com/context", "unsupported build context scheme"},
		{"base=docker-image://", "invalid docker-image"},
		{"base=container-image://", "invalid docker-image"},
		{"layout=oci-layout://./images/app", "tag or digest"},
	} {
		t.Run(test.value, func(t *testing.T) {
			_, err := Parse([]string{test.value})
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("Parse(%q) error = %v, want %q", test.value, err, test.message)
			}
		})
	}
}

func TestParsePrimary(t *testing.T) {
	local := t.TempDir()
	for _, test := range []struct {
		name, value string
		kind        Kind
	}{
		{name: "default local", kind: Local},
		{name: "explicit local", value: local, kind: Local},
		{name: "git", value: "https://example.com/source.git?branch=main&checksum=0123456789ab", kind: Git},
		{name: "http archive", value: "https://example.com/context.tar.gz", kind: HTTPArchive},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec, err := ParsePrimary(test.value, local)
			if err != nil {
				t.Fatal(err)
			}
			if spec.Name != "context" || spec.Kind != test.kind {
				t.Fatalf("primary context = %#v", spec)
			}
		})
	}
	if _, err := ParsePrimary("docker-image://example.com/base:latest", local); err == nil {
		t.Fatal("accepted image as the primary filesystem context")
	}
}

func TestParseRepeatedNamesUseLastContext(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	got, err := Parse([]string{"Source=./first", "other=./other", "source=./last"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Spec{
		{Name: "source", Kind: Local, Path: filepath.Join(root, "last")},
		{Name: "other", Kind: Local, Path: filepath.Join(root, "other")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("contexts = %+v, want %+v", got, want)
	}
}
