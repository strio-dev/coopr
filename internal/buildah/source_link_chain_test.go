package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	storagearchive "go.podman.io/storage/pkg/archive"
)

func TestImageSourceDirectoryLinkChainsPreserveTargets(t *testing.T) {
	var source, output bytes.Buffer
	writer := tar.NewWriter(&source)
	for _, header := range []*tar.Header{
		{Name: "blobs", Typeflag: tar.TypeDir, Mode: 0700},
		{Name: "alias", Typeflag: tar.TypeSymlink, Linkname: "/blobs", Mode: 0777},
		{Name: "nested", Typeflag: tar.TypeDir, Mode: 0700},
		{Name: "nested/second", Typeflag: tar.TypeSymlink, Linkname: "../alias", Mode: 0777},
		{Name: "third", Typeflag: tar.TypeSymlink, Linkname: "nested/second", Mode: 0777},
	} {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := filterImageArchive(context.Background(), &source, &output); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := storagearchive.Untar(bytes.NewReader(output.Bytes()), root, &storagearchive.TarOptions{NoLchown: true}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alias", "nested/second", "third"} {
		resolved, err := filepath.EvalSymlinks(filepath.Join(root, name))
		if err != nil || resolved != filepath.Join(root, "blobs") {
			t.Fatalf("native extraction of %q resolved to %q: %v", name, resolved, err)
		}
	}
	want := map[string]string{"alias": "blobs", "nested/second": "../alias", "third": "nested/second"}
	reader := tar.NewReader(&output)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if target, ok := want[header.Name]; ok {
			if header.Typeflag != tar.TypeSymlink || header.Linkname != target {
				t.Fatalf("directory alias %q became type %d target %q; want symlink %q", header.Name, header.Typeflag, header.Linkname, target)
			}
			delete(want, header.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing directory links: %v", want)
	}
}

func TestImageSourceRejectsHardlinkToDirectoryAlias(t *testing.T) {
	var source, output bytes.Buffer
	writer := tar.NewWriter(&source)
	for _, header := range []*tar.Header{
		{Name: "blobs", Typeflag: tar.TypeDir, Mode: 0700},
		{Name: "alias", Typeflag: tar.TypeSymlink, Linkname: "blobs", Mode: 0777},
		{Name: "hardlink", Typeflag: tar.TypeLink, Linkname: "alias", Mode: 0700},
	} {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := filterImageArchive(context.Background(), &source, &output); err == nil {
		t.Fatal("accepted hardlink to directory alias")
	}
}
