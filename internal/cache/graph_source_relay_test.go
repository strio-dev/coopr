package cache

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestGraphSourceHandoffRetainsMetadataAfterOwnerAndSourceRemoval(t *testing.T) {
	original, source, store, record, _, target := graphSourceFixture(t)
	snapshot := source.Metadata()
	originalSnapshot := source.Metadata()
	// Metadata exported to a caller cannot alter the admitted source.
	snapshot.Index[0] = '['
	snapshot.Manifest[0] = '['
	snapshot.Record[0] = '['
	if !reflect.DeepEqual(source.Metadata(), originalSnapshot) {
		t.Fatal("metadata export aliases admitted bytes")
	}
	snapshot = source.Metadata()
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(snapshot.Manifest, &manifest); err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(snapshot.Index, &index); err != nil {
		t.Fatal(err)
	}
	layout := target.Target.(*orasoci.Store)
	layout.AutoGC = false // Remove metadata only; retain deferred image payloads.
	for _, descriptor := range append([]v1.Descriptor{snapshot.Artifact, index.Manifests[0], manifest.Config}, manifest.Layers...) {
		if err := layout.Delete(t.Context(), descriptor); err != nil {
			t.Fatal(err)
		}
	}
	// The immutable snapshot was admitted earlier; the source's lookup TTL must
	// not cause a second admission or invalidate this invocation's retained view.
	ttl := time.Nanosecond
	store.ttl = &ttl
	target.mu.Lock()
	before := map[digest.Digest]int{}
	for key, value := range target.fetches {
		before[key] = value
	}
	target.mu.Unlock()
	restored, err := NewGraphReader(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	next, err := restored.RestoreMetadata(t.Context(), store, record.Root, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	target.mu.Lock()
	if !reflect.DeepEqual(before, target.fetches) {
		t.Error("handoff fetched source metadata or image payloads")
	}
	target.mu.Unlock()
	// Mutating a caller's snapshot after verification cannot mutate the handle.
	snapshot.Index[0] = '['
	snapshot.Manifest[0] = '['
	snapshot.Record[0] = '['
	got, err := next.Record()
	if err != nil || !reflect.DeepEqual(got, record) {
		t.Fatal("restored source aliases caller buffer", err)
	}
	if !reflect.DeepEqual(next.Metadata(), originalSnapshot) {
		t.Fatal("handoff changed immutable bytes")
	}
	path, err := next.EnsureResult(t.Context(), record.Results[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	first := record
	first.Results = record.Results[:1]
	assertGraphReaderImages(t, path, first)
	if target.fetches[record.Results[1].Image.Digest] != 0 {
		t.Fatal("handoff fetched unselected result")
	}
	if _, err := next.EnsureAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertGraphReaderImages(t, path, record)
}

func TestGraphSourceHandoffRejectsChangedBindings(t *testing.T) {
	_, source, store, record, _, _ := graphSourceFixture(t)
	for _, test := range []struct {
		name   string
		change func(*GraphSourceMetadata, *digest.Digest)
	}{
		{"artifact digest", func(s *GraphSourceMetadata, _ *digest.Digest) { s.Artifact.Digest = digest.FromString("different") }},
		{"artifact size", func(s *GraphSourceMetadata, _ *digest.Digest) { s.Artifact.Size++ }},
		{"legacy manifest root", func(s *GraphSourceMetadata, _ *digest.Digest) { s.Artifact.MediaType = v1.MediaTypeImageManifest }},
		{"index", func(s *GraphSourceMetadata, _ *digest.Digest) { s.Index = append(s.Index, ' ') }},
		{"index image binding", func(s *GraphSourceMetadata, _ *digest.Digest) {
			var index v1.Index
			if err := json.Unmarshal(s.Index, &index); err != nil {
				t.Fatal(err)
			}
			index.Manifests[1].Digest = digest.FromString("different image")
			var err error
			s.Index, err = json.Marshal(index)
			if err != nil {
				t.Fatal(err)
			}
			s.Artifact = oci.Descriptor(v1.MediaTypeImageIndex, s.Index)
		}},
		{"manifest", func(s *GraphSourceMetadata, _ *digest.Digest) { s.Manifest = append(s.Manifest, ' ') }},
		{"record", func(s *GraphSourceMetadata, _ *digest.Digest) { s.Record = append(s.Record, ' ') }},
		{"root", func(_ *GraphSourceMetadata, r *digest.Digest) { *r = digest.FromString("different root") }},
		{"empty manifest", func(s *GraphSourceMetadata, _ *digest.Digest) { s.Manifest = nil }},
		{"empty index", func(s *GraphSourceMetadata, _ *digest.Digest) { s.Index = nil }},
		{"oversized index", func(s *GraphSourceMetadata, _ *digest.Digest) { s.Index = make([]byte, maxGraphRecordBytes+1) }},
		{"oversized record", func(s *GraphSourceMetadata, _ *digest.Digest) { s.Record = make([]byte, maxGraphRecordBytes+1) }},
		{"unknown record field", func(s *GraphSourceMetadata, _ *digest.Digest) { s.Record = []byte(`{"unknown":true}`) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader, err := NewGraphReader(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reader.Close() }()
			snapshot, root := source.Metadata(), record.Root
			test.change(&snapshot, &root)
			if _, err := reader.RestoreMetadata(t.Context(), store, root, snapshot); err == nil {
				t.Fatal("accepted changed immutable metadata")
			}
			if len(reader.sources) != 0 || reader.metadataBytes != 0 {
				t.Fatal("rejected relay retained metadata")
			}
		})
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	reader, err := NewGraphReader(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if _, err := reader.RestoreMetadata(canceled, store, record.Root, source.Metadata()); err == nil {
		t.Fatal("accepted canceled handoff")
	}
}
