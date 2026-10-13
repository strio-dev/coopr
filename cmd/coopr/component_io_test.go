package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
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

func TestComponentIOCommandsRoundTripWithoutNativeImageStore(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ctx := context.Background()
	def, err := definition.Parse(strings.NewReader(`extend as=base {
    env GREETING=hello
}
`))
	if err != nil {
		t.Fatal(err)
	}
	publication, err := planner.Create(def, planner.Options{Mode: planner.Publish, Platform: "linux/amd64"})
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(t.TempDir(), "component")
	root, err := oci.WriteComponentLayout(ctx, layout, oci.ComponentMetadata{Version: oci.ComponentVersion, Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Component: *publication.Component}, nil)
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
	if err := componentstore.Put(ctx, dir, source, root, "source"); err != nil {
		t.Fatal(err)
	}
	t.Run("save literal output paths", func(t *testing.T) {
		for _, test := range []struct {
			name     string
			path     string
			relative bool
		}{
			{"absolute", "component:v1.tar", false},
			{"relative", "component:v1.tar", true},
			{"spaces", "component:release candidate.tar", false},
			{"directory", "components:v1/archive.tar", false},
		} {
			t.Run(test.name, func(t *testing.T) {
				directory := t.TempDir()
				output := filepath.Join(directory, test.path)
				if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
					t.Fatal(err)
				}
				truncated, _, _ := strings.Cut(output, ":")
				if err := os.WriteFile(truncated, []byte("keep this file"), 0o600); err != nil {
					t.Fatal(err)
				}
				argument := output
				if test.relative {
					t.Chdir(directory)
					argument = test.path
				}
				var out, errs bytes.Buffer
				status := run([]string{"component", "save", "--output", argument, "source"}, &out, &errs)
				data, err := os.ReadFile(truncated)
				if err != nil || string(data) != "keep this file" {
					t.Fatalf("save changed truncated output path %q (read %d bytes): %v", truncated, len(data), err)
				}
				if status != 0 || out.Len() != 0 {
					t.Fatalf("save status=%d stdout=%q stderr=%q", status, &out, &errs)
				}
				archived, err := oci.ArchiveRoot(output)
				if err != nil || archived.Digest != root.Digest {
					t.Fatalf("literal output archive = %v, %v; want %s", archived, err, root.Digest)
				}
			})
		}
		output := filepath.Join(dir, "component:v1.tar")
		var out, errs bytes.Buffer
		if status := run([]string{"component", "save", "--output", output, "source"}, &out, &errs); status != 0 || out.Len() != 0 || errs.Len() != 0 {
			t.Fatalf("save within owned store status=%d stdout=%q stderr=%q", status, &out, &errs)
		}
		archived, err := oci.ArchiveRoot(output)
		if err != nil || archived.Digest != root.Digest {
			t.Fatalf("owned-store archive = %v, %v; want %s", archived, err, root.Digest)
		}
		saved, err := componentstore.Open(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		retained, err := saved.Resolve(ctx, "source:latest")
		if err != nil || retained.Digest != root.Digest {
			t.Fatalf("save changed component source = %v, %v", retained, err)
		}
	})
	server := httptest.NewServer(registry.New())
	defer server.Close()
	remote := strings.TrimPrefix(server.URL, "http://") + "/coopr/component:one"
	pushArchive := filepath.Join(t.TempDir(), "pushed.oci.tar")
	for _, test := range []struct {
		args []string
		code int
	}{
		{[]string{"exists", "source"}, 0}, {[]string{"exists", "missing"}, 1},
		{[]string{"tag", "source", "alias"}, 0}, {[]string{"exists", "alias:latest"}, 0}, {[]string{"exists", "local:alias"}, 1},
		{[]string{"tag", "source:latest", "local:alias"}, 0}, {[]string{"exists", "local:alias"}, 0},
		{[]string{"copy", "local:alias", "copied", "also-copied"}, 0}, {[]string{"exists", "copied:latest"}, 0}, {[]string{"exists", "also-copied"}, 0},
		{[]string{"tag", "source", "release:one"}, 0}, {[]string{"exists", "release:one"}, 0},
		{[]string{"push", "--tls-verify=false", "alias", "docker://" + remote}, 0},
		{[]string{"pull", "--tls-verify=false", "--tag", "pulled", "docker://" + remote}, 0},
		{[]string{"exists", "pulled"}, 0},
		{[]string{"push", "source", "oci-archive:" + pushArchive + ":pushed"}, 0},
		{[]string{"load", "--input", pushArchive}, 0}, {[]string{"exists", "pushed"}, 0},
	} {
		var out, errs bytes.Buffer
		if code := run(append([]string{"component"}, test.args...), &out, &errs); code != test.code {
			t.Fatalf("%v code=%d stderr=%q", test.args, code, &errs)
		}
		if (test.args[0] == "exists" || test.args[0] == "tag" || test.args[0] == "copy") && (out.Len() != 0 || errs.Len() != 0) {
			t.Fatalf("exists output=%q stderr=%q", &out, &errs)
		}
	}
	archivedRoot, err := oci.ArchiveRoot(pushArchive)
	if err != nil || archivedRoot.Annotations[v1.AnnotationRefName] != "pushed" {
		t.Fatalf("component archive suffix annotation = %v, %v", archivedRoot.Annotations, err)
	}
	if info, err := os.Stat(pushArchive); err != nil || info.Size() == 0 {
		t.Fatalf("component push archive missing or empty: %v", err)
	}
	var archive, errs bytes.Buffer
	if code := run([]string{"component", "save", "source"}, &archive, &errs); code != 0 {
		t.Fatalf("save code=%d stderr=%q", code, &errs)
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	var out bytes.Buffer
	cmd := newRootCommandWithStorageNamespace(func() error {
		t.Fatal("component load invoked native image storage")
		return nil
	})
	cmd.SetIn(bytes.NewReader(archive.Bytes()))
	cmd.SetOut(&out)
	cmd.SetErr(&errs)
	cmd.SetArgs([]string{"component", "load"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	resolver, err := oci.NewResolver(oci.Options{})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(ctx, "source", v1.Platform{OS: "linux", Architecture: "amd64"}, oci.Component)
	if err != nil || resolved.Root.Digest != root.Digest {
		t.Fatalf("restored bytes changed: %v", err)
	}
	t.Run("build consumes loaded component", func(t *testing.T) {
		if testing.Short() {
			t.Skip("skipping the native user namespace in short mode")
		}
		store := maintenanceStoreOptions(t.TempDir())
		file := filepath.Join(t.TempDir(), "container.coopr")
		if err := os.WriteFile(file, []byte(`from scratch {
    component source
}
`), 0o600); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		errs.Reset()
		args := []string{"--root", store.GraphRoot, "--runroot", store.RunRoot, "--storage-driver", "vfs", "build", file, "--platform", "linux/amd64", "--tag", "component-consumer"}
		if status := run(args, &out, &errs); status != 0 {
			t.Fatalf("build with loaded component status=%d stderr=%s", status, &errs)
		}
		out.Reset()
		errs.Reset()
		if status := run(imageIOArgs(store, "inspect", "component-consumer"), &out, &errs); status != 0 || !strings.Contains(out.String(), "GREETING=hello") {
			t.Fatalf("component transformation missing: status=%d stdout=%s stderr=%s", status, &out, &errs)
		}
	})
}

func TestComponentExistsStorageFailureExit125(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "relative/path")
	var out, errs bytes.Buffer
	if code := run([]string{"component", "exists", "any"}, &out, &errs); code != 125 || errs.Len() == 0 {
		t.Fatalf("storage error code=%d stderr=%q", code, &errs)
	}
}

func TestComponentNamedDigestCommandsIgnoreMovedTag(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ctx := t.Context()
	directory, err := componentstore.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	put := func(greeting string) v1.Descriptor {
		t.Helper()
		def, err := definition.Parse(strings.NewReader(fmt.Sprintf(`extend as=base {
    env GREETING=%q
}
`, greeting)))
		if err != nil {
			t.Fatal(err)
		}
		publication, err := planner.Create(def, planner.Options{Mode: planner.Publish, Platform: "linux/amd64"})
		if err != nil {
			t.Fatal(err)
		}
		layout := filepath.Join(t.TempDir(), "component")
		root, err := oci.WriteComponentLayout(ctx, layout, oci.ComponentMetadata{Version: oci.ComponentVersion, Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Component: *publication.Component}, nil)
		if err != nil {
			t.Fatal(err)
		}
		source, err := orasoci.New(layout)
		if err != nil {
			t.Fatal(err)
		}
		if err := componentstore.Put(ctx, directory, source, root, "greeting"); err != nil {
			t.Fatal(err)
		}
		return root
	}
	original := put("hello")
	updated := put("updated")
	if original.Digest == updated.Digest {
		t.Fatal("fixture did not retarget greeting")
	}
	pinned := "greeting@" + original.Digest.String()
	for _, args := range [][]string{
		{"exists", pinned}, {"tag", pinned, "tagged"}, {"copy", pinned, "copied"},
	} {
		var out, errs bytes.Buffer
		if status := run(append([]string{"component"}, args...), &out, &errs); status != 0 {
			t.Fatalf("%v status=%d stderr=%s", args, status, &errs)
		}
	}
	var archive, errs bytes.Buffer
	if status := run([]string{"component", "save", pinned}, &archive, &errs); status != 0 {
		t.Fatalf("save pinned status=%d stderr=%s", status, &errs)
	}
	cmd := newRootCommandWithStorageNamespace(func() error { t.Fatal("component load initialized image storage"); return nil })
	cmd.SetIn(bytes.NewReader(archive.Bytes()))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&errs)
	cmd.SetArgs([]string{"component", "load", "--tag", "saved"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{pinned, "tagged", "copied", "saved", "greeting"} {
		var out, errs bytes.Buffer
		if status := run([]string{"component", "inspect", selector}, &out, &errs); status != 0 {
			t.Fatalf("inspect %s status=%d stderr=%s", selector, status, &errs)
		}
		var got v1.Descriptor
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		want := original.Digest
		if selector == "greeting" {
			want = updated.Digest
		}
		if got.Digest != want {
			t.Fatalf("%s selected %s, want %s", selector, got.Digest, want)
		}
	}
}
