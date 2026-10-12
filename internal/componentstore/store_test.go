package componentstore

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestPutAnchorsDigestAndOptionalTag(t *testing.T) {
	ctx := context.Background()
	source, root, blobs := testGraph(t, ctx, "first")
	dir := filepath.Join(t.TempDir(), "components")
	if err := Put(ctx, dir, source, root, ""); err != nil {
		t.Fatal(err)
	}
	store, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Resolve(ctx, root.Digest.String())
	if err != nil || got.MediaType != root.MediaType || got.Digest != root.Digest || got.Size != root.Size {
		t.Fatalf("digest root differs: %+v, %v", got, err)
	}
	assertGraphBytes(t, ctx, store, blobs)
	if err := Put(ctx, dir, source, root, "stable"); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	tagged, err := store.Resolve(ctx, "stable:latest")
	if err != nil || tagged.Digest != root.Digest {
		t.Fatalf("tag root differs: %+v, %v", tagged, err)
	}
	secondSource, secondRoot, _ := testGraph(t, ctx, "second")
	if err := Put(ctx, dir, secondSource, secondRoot, "stable"); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	tagged, err = store.Resolve(ctx, "stable:latest")
	if err != nil || tagged.Digest != secondRoot.Digest {
		t.Fatalf("moved tag differs: %+v, %v", tagged, err)
	}
	prior, err := store.Resolve(ctx, root.Digest.String())
	if err != nil || prior.Digest != root.Digest {
		t.Fatalf("prior digest no longer resolves: %+v, %v", prior, err)
	}
}

func TestWriteArchiveAnchorsSingleRoot(t *testing.T) {
	ctx := context.Background()
	source, root, blobs := testGraph(t, ctx, "archive")
	archive := filepath.Join(t.TempDir(), "component.oci.tar")
	if err := WriteArchive(ctx, source, root, archive); err != nil {
		t.Fatal(err)
	}
	store, err := orasoci.NewFromTar(ctx, archive)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Resolve(ctx, root.Digest.String())
	if err != nil || got.MediaType != root.MediaType || got.Digest != root.Digest || got.Size != root.Size {
		t.Fatalf("archive root differs: %+v, %v", got, err)
	}
	assertGraphBytes(t, ctx, store, blobs)
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err != nil {
			t.Fatalf("read archive index: %v", err)
		}
		if header.Name != "index.json" {
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		var index v1.Index
		if err := json.Unmarshal(data, &index); err != nil {
			t.Fatal(err)
		}
		if len(index.Manifests) != 1 || index.Manifests[0].Digest != root.Digest || index.Manifests[0].MediaType != root.MediaType {
			t.Fatalf("archive index does not contain exactly the component root: %+v", index.Manifests)
		}
		break
	}
}

func TestDefaultDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	dir, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dir) != "components" || filepath.Base(filepath.Dir(dir)) != "coopr" {
		t.Fatalf("unexpected default component store %q", dir)
	}
}

func TestOpenWaitsForStoreWriter(t *testing.T) {
	dir := t.TempDir()
	lock := flock.New(filepath.Join(dir, ".coopr-store.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Unlock() }()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Open(ctx, dir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reader did not wait for store writer: %v", err)
	}
}

func testGraph(t *testing.T, ctx context.Context, marker string) (*orasoci.Store, v1.Descriptor, map[digest.Digest][]byte) {
	t.Helper()
	store, err := orasoci.NewWithContext(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configData := []byte(`{"kind":"component","marker":"` + marker + `"}`)
	config := descriptor("application/vnd.test.config.v1+json", configData)
	if err := store.Push(ctx, config, bytes.NewReader(configData)); err != nil {
		t.Fatal(err)
	}
	manifestData, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest,
		ArtifactType: "application/vnd.test.component.v1", Config: config,
	})
	if err != nil {
		t.Fatal(err)
	}
	root := descriptor(v1.MediaTypeImageManifest, manifestData)
	if err := store.Push(ctx, root, bytes.NewReader(manifestData)); err != nil {
		t.Fatal(err)
	}
	if err := store.Tag(ctx, root, root.Digest.String()); err != nil {
		t.Fatal(err)
	}
	return store, root, map[digest.Digest][]byte{config.Digest: configData, root.Digest: manifestData}
}

func descriptor(mediaType string, data []byte) v1.Descriptor {
	return v1.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
}

func assertGraphBytes(t *testing.T, ctx context.Context, store content.ReadOnlyStorage, blobs map[digest.Digest][]byte) {
	t.Helper()
	for dgst, want := range blobs {
		desc := v1.Descriptor{Digest: dgst, Size: int64(len(want))}
		got, err := content.FetchAll(ctx, store, desc)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("blob %s changed", dgst)
		}
	}
}

func TestNormalizeComponentNames(t *testing.T) {
	for input, want := range map[string]string{
		"": "", "settings": "settings:latest", "settings:v1": "settings:v1",
		"team/settings": "team/settings:latest", "registry.example/team/settings:v1": "registry.example/team/settings:v1",
		"docker.io/library/settings": "settings:latest", "local:foo": "local:foo",
	} {
		got, err := NormalizeTag(input)
		if err != nil || got != want {
			t.Errorf("NormalizeTag(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	dgst := digest.FromString("component").String()
	for input, want := range map[string]string{dgst: dgst, "settings@" + dgst: "settings@" + dgst, "settings": "settings:latest"} {
		got, err := NormalizeReference(input)
		if err != nil || got != want {
			t.Errorf("NormalizeReference(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"sha256:bad", "settings@sha256:bad", "", "./settings.coopr", "../settings.coopr", "bad name"} {
		if _, err := NormalizeReference(input); err == nil {
			t.Errorf("invalid reference %q accepted", input)
		}
	}
	for _, input := range []string{dgst, "settings@" + dgst, "bad name", "Uppercase", "http://registry.example/foo"} {
		if _, err := NormalizeTag(input); err == nil {
			t.Errorf("invalid tag %q accepted", input)
		}
	}
}

func TestLocalSelector(t *testing.T) {
	dgst := digest.FromString("component").String()
	for input, want := range map[string]string{
		"settings": "settings:latest", "settings:v1": "settings:v1", "local:foo": "local:foo",
		dgst: dgst, "settings@" + dgst: dgst, "registry.example/team/settings:v1@" + dgst: dgst,
	} {
		got, err := LocalSelector(input)
		if err != nil || got != want {
			t.Errorf("LocalSelector(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "settings@sha256:bad", "sha256:bad", "./settings.coopr"} {
		if _, err := LocalSelector(input); err == nil {
			t.Errorf("invalid local selector %q accepted", input)
		}
	}
}
