package oci

import (
	"archive/tar"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/componentstore"
)

func TestArchiveImageConfigDigest(t *testing.T) {
	archive, root := imageConfigArchiveFixture(t)
	got, err := ArchiveImageConfigDigest(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	if got == "" || got == root.Digest {
		t.Fatalf("got %q; expected image config digest distinct from manifest digest", got)
	}
}

func TestArchiveIndexAdmissionPolicies(t *testing.T) {
	root := Descriptor(v1.MediaTypeImageManifest, []byte("manifest"))
	index := func(roots ...v1.Descriptor) v1.Index {
		return v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: roots}
	}
	wrongSchema, wrongMedia, badRoot := index(root), index(root), root
	wrongSchema.SchemaVersion = 1
	wrongMedia.MediaType = "unsupported-outer-index"
	badRoot.Digest = "invalid"
	for _, test := range []struct {
		name, entry          string
		index                v1.Index
		raw                  string
		headerSize           int64
		imageOK, componentOK bool
	}{
		{name: "single", entry: "index.json", index: index(root), imageOK: true, componentOK: true},
		{name: "dot prefix", entry: "./index.json", index: index(root), imageOK: true, componentOK: true},
		{name: "nested ignored", entry: "nested/index.json", index: index(root)},
		{name: "traversal ignored", entry: "../index.json", index: index(root)},
		{name: "empty", entry: "index.json", index: index()},
		{name: "multiple", entry: "index.json", index: index(root, root), componentOK: true},
		{name: "schema", entry: "index.json", index: wrongSchema},
		{name: "outer media policy", entry: "index.json", index: wrongMedia, imageOK: true},
		{name: "descriptor policy", entry: "index.json", index: index(badRoot), componentOK: true},
		{name: "malformed", entry: "index.json", raw: "{"},
		{name: "truncated payload", entry: "index.json", raw: "{", headerSize: 64},
		{name: "oversized before payload", entry: "index.json", headerSize: maxMetadataBytes + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(test.index)
			if err != nil {
				t.Fatal(err)
			}
			if test.raw != "" {
				data = []byte(test.raw)
			}
			path := filepath.Join(t.TempDir(), "archive.tar")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			writer := tar.NewWriter(file)
			size := int64(len(data))
			if test.headerSize != 0 {
				size = test.headerSize
			}
			if err := writer.WriteHeader(&tar.Header{Name: test.entry, Mode: 0600, Size: size}); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(data); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); test.headerSize == 0 && err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			got, imageErr := ArchiveRoot(path)
			if (imageErr == nil) != test.imageOK || test.imageOK && got.Digest != root.Digest {
				t.Fatalf("image admission: root=%+v err=%v", got, imageErr)
			}
			roots, componentErr := componentstore.ArchiveRoots(path)
			if (componentErr == nil) != test.componentOK || test.componentOK && len(roots) != len(test.index.Manifests) {
				t.Fatalf("component admission: roots=%+v err=%v", roots, componentErr)
			}
		})
	}
}

func imageConfigArchiveFixture(t *testing.T) (string, v1.Descriptor) {
	t.Helper()
	configBytes := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	config := Descriptor(v1.MediaTypeImageConfig, configBytes)
	manifestBytes, err := json.Marshal(VersionedManifest(config, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	root := Descriptor(v1.MediaTypeImageManifest, manifestBytes)
	indexBytes, err := json.Marshal(v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		Manifests: []v1.Descriptor{root},
	})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`),
		"index.json": indexBytes,
		"blobs/sha256/" + config.Digest.Encoded(): configBytes,
		"blobs/sha256/" + root.Digest.Encoded():   manifestBytes,
	}
	path := filepath.Join(t.TempDir(), "image.oci.tar")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	for name, data := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path, root
}
