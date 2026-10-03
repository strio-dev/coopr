package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func archiveLayerNames(t *testing.T, archive string) []string {
	t.Helper()
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	outer := tar.NewReader(file)
	blobs := map[string][]byte{}
	for {
		header, err := outer.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(outer)
		if err != nil {
			t.Fatal(err)
		}
		blobs[header.Name] = data
	}
	var index v1.Index
	if err := json.Unmarshal(blobs["index.json"], &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 1 {
		t.Fatalf("manifest count %d", len(index.Manifests))
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(blobs["blobs/sha256/"+index.Manifests[0].Digest.Encoded()], &manifest); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, layer := range manifest.Layers {
		data := blobs["blobs/sha256/"+layer.Digest.Encoded()]
		var source io.Reader = bytes.NewReader(data)
		if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
			unzipped, err := gzip.NewReader(source)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = unzipped.Close() }()
			source = unzipped
		}
		inner := tar.NewReader(source)
		for {
			header, err := inner.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			names = append(names, header.Name)
		}
	}
	return names
}

func archiveImageConfig(t *testing.T, archive string) v1.Image {
	t.Helper()
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	outer := tar.NewReader(file)
	blobs := map[string][]byte{}
	for {
		header, err := outer.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(outer)
		if err != nil {
			t.Fatal(err)
		}
		blobs[header.Name] = data
	}
	var index v1.Index
	if err := json.Unmarshal(blobs["index.json"], &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 1 {
		t.Fatalf("manifest count %d", len(index.Manifests))
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(blobs["blobs/sha256/"+index.Manifests[0].Digest.Encoded()], &manifest); err != nil {
		t.Fatal(err)
	}
	var config v1.Image
	if err := json.Unmarshal(blobs["blobs/sha256/"+manifest.Config.Digest.Encoded()], &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func containsName(names []string, wanted string) bool {
	for _, name := range names {
		if name == wanted {
			return true
		}
	}
	return false
}
