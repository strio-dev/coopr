package main

import (
	"bytes"
	"context"
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
	def, err := definition.Parse(strings.NewReader("extend as=base\nenv GREETING=hello\n"))
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
	server := httptest.NewServer(registry.New())
	defer server.Close()
	remote := strings.TrimPrefix(server.URL, "http://") + "/coopr/component:one"
	for _, test := range []struct {
		args []string
		code int
	}{
		{[]string{"exists", "source"}, 0}, {[]string{"exists", "missing"}, 1},
		{[]string{"tag", "source", "alias"}, 0}, {[]string{"exists", "local:alias"}, 0},
		{[]string{"push", "--tls-verify=false", "alias", remote}, 0},
		{[]string{"pull", "--tls-verify=false", "--tag", "pulled", remote}, 0},
		{[]string{"exists", "pulled"}, 0},
	} {
		var out, errs bytes.Buffer
		if code := run(append([]string{"component"}, test.args...), &out, &errs); code != test.code {
			t.Fatalf("%v code=%d stderr=%q", test.args, code, &errs)
		}
		if test.args[0] == "exists" && (out.Len() != 0 || errs.Len() != 0) {
			t.Fatalf("exists output=%q stderr=%q", &out, &errs)
		}
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
	resolved, err := resolver.Resolve(ctx, "local:source", v1.Platform{OS: "linux", Architecture: "amd64"}, oci.Component)
	if err != nil || resolved.Root.Digest != root.Digest {
		t.Fatalf("restored bytes changed: %v", err)
	}
	t.Run("build consumes loaded component", func(t *testing.T) {
		if os.Getenv("COOPR_TEST_BUILDAH") == "" {
			t.Skip("set COOPR_TEST_BUILDAH=1 for the native user namespace")
		}
		store := maintenanceStoreOptions(t.TempDir())
		file := filepath.Join(t.TempDir(), "container.coopr")
		if err := os.WriteFile(file, []byte("from scratch\ncomponent local:source\n"), 0o600); err != nil {
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
