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
