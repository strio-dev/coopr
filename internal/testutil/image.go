package testutil

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func ReadImage(t testing.TB, ctx context.Context, archive string) (v1.Manifest, v1.Image) {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	r := tar.NewReader(f)
	var index v1.Index
	for {
		h, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if h.Name != "index.json" && h.Name != "./index.json" {
			continue
		}
		if err := json.NewDecoder(io.LimitReader(r, h.Size)).Decode(&index); err != nil {
			t.Fatal(err)
		}
		break
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 1 {
		t.Fatalf("expected one final image root, got %d", len(index.Manifests))
	}
	store, err := orasoci.NewFromTar(ctx, archive)
	if err != nil {
		t.Fatal(err)
	}
	read := func(desc v1.Descriptor, dst any) {
		stream, err := store.Fetch(ctx, desc)
		if err != nil {
			t.Fatal(err)
		}
		decodeErr := json.NewDecoder(io.LimitReader(stream, 8<<20)).Decode(dst)
		closeErr := stream.Close()
		if decodeErr != nil || closeErr != nil {
			t.Fatalf("decode OCI metadata %s: %v, close: %v", desc.Digest, decodeErr, closeErr)
		}
	}
	var manifest v1.Manifest
	read(index.Manifests[0], &manifest)
	var image v1.Image
	read(manifest.Config, &image)
	return manifest, image
}
