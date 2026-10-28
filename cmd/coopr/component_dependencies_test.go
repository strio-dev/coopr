package main

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/componentstore"
	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"

	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestComponentPushWarnsAboutRuntimeDependenciesOnStderr(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	server := httptest.NewServer(registry.New())
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	definitionText := fmt.Sprintf("arg \"dependency\" %q\nextend {\n    arg \"dependency\"\n    component \"${dependency}\"\n}\n", host+"/coopr/child:default")
	def, err := definition.Parse(strings.NewReader(definitionText))
	if err != nil {
		t.Fatal(err)
	}
	publication, err := planner.Create(def, planner.Options{Mode: planner.Publish, Platform: "linux/amd64"})
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(t.TempDir(), "component")
	root, err := oci.WriteComponentLayout(t.Context(), layout, oci.ComponentMetadata{Version: oci.ComponentVersion, Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Component: *publication.Component}, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := orasoci.New(layout)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := componentstore.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := componentstore.Put(t.Context(), dir, source, root, "parent"); err != nil {
		t.Fatal(err)
	}
	destination := host + "/coopr/parent:stable"
	var stdout, stderr bytes.Buffer
	if status := run([]string{"component", "push", "--tls-verify=false", "parent", destination}, &stdout, &stderr); status != 0 {
		t.Fatalf("component push status=%d stderr=%q", status, &stderr)
	}
	want := host + "/coopr/parent@" + root.Digest.String() + "\n"
	if stdout.String() != want {
		t.Fatalf("component push stdout=%q; want only %q", &stdout, want)
	}
	if !strings.Contains(stderr.String(), "runtime-selected dependencies explicitly") {
		t.Fatalf("component push omitted dependency discovery warning: %q", &stderr)
	}
}
