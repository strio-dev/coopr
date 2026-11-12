package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

func graphSourceFixture(t *testing.T) (*GraphReader, *GraphSource, *OCIStore, GraphRecord, []v1.Descriptor, *graphLookupTarget) {
	t.Helper()
	store, record, payloads, target := graphLookupFixture(t, v1.MediaTypeImageLayer, nil)
	reader, err := NewGraphReader(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	source, err := reader.LookupMetadata(t.Context(), store, record.Root)
	if err != nil {
		t.Fatal(err)
	}
	return reader, source, store, record, payloads, target
}

func TestGraphSourcePreservesCloseFailureOrigin(t *testing.T) {
	for _, closeErr := range []error{io.ErrUnexpectedEOF, content.ErrMismatchedDigest} {
		for _, corrupt := range []bool{false, true} {
			for _, where := range []string{"payload", "manifest"} {
				t.Run(fmt.Sprintf("%v/corrupt-%t/%s", closeErr, corrupt, where), func(t *testing.T) {
					reader, source, _, record, payloads, target := graphSourceFixture(t)
					target.alter = payloads[0].Digest
					if where == "manifest" {
						target.alter = record.Results[0].Image.Digest
					}
					target.closeFailure = closeErr
					target.closeFailureOnce = true
					if corrupt {
						target.failure = "corrupt"
					}
					if _, err := source.EnsureResult(t.Context(), record.Results[0].ID); !errors.Is(err, closeErr) || !errors.Is(err, ErrGraphSourceClose) {
						t.Fatalf("source close provenance or original error lost: %v", err)
					}
					if _, retained := reader.verified[record.Results[0].Image.Digest]; retained || target.opened != target.closed {
						t.Fatalf("failed close admitted a result or leaked streams: retained=%v, opened=%d, closed=%d", retained, target.opened, target.closed)
					}
				})
			}
		}
	}
}

func TestGraphSourceMetadataDoesNotFetchImagesAndSelectedRestoreIsComplete(t *testing.T) {
	reader, source, store, record, payloads, target := graphSourceFixture(t)
	got, err := source.Record()
	if err != nil || !reflect.DeepEqual(got, record) {
		t.Fatal("metadata admission changed graph", got, err)
	}
	for _, descriptor := range append(graphImages(record), payloads...) {
		if target.fetches[descriptor.Digest] != 0 {
			t.Fatalf("metadata admission fetched image content %s", descriptor.Digest)
		}
	}
	if target.opened != target.closed {
		t.Fatal("metadata admission retained source streams")
	}
	metadataBytes := reader.metadataBytes
	repeated, err := reader.LookupMetadata(t.Context(), store, record.Root)
	if err != nil || repeated != source || reader.metadataBytes != metadataBytes {
		t.Fatal("duplicate admission retained another metadata handle", err)
	}
	if _, err := source.EnsureResult(t.Context(), "not-bound"); err == nil {
		t.Fatal("unbound result accepted")
	}
	layout, err := source.EnsureResult(t.Context(), record.Results[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	first := record
	first.Results = record.Results[:1]
	assertGraphReaderImages(t, layout, first)
	if target.fetches[record.Results[1].Image.Digest] != 0 || target.fetches[payloads[2].Digest] != 0 {
		t.Fatal("selected restoration fetched an unused alternative")
	}
	if _, err := source.EnsureAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertGraphReaderImages(t, layout, record)
	for _, payload := range payloads {
		if target.fetches[payload.Digest] != 1 || target.read[payload.Digest] != payload.Size {
			t.Fatalf("selected plus complete retrieval duplicated payload %s: fetches=%d bytes=%d", payload.Digest, target.fetches[payload.Digest], target.read[payload.Digest])
		}
	}
	// Ingestion must own separate bytes, not a link into mutable source storage.
	stream, err := target.Target.Fetch(t.Context(), payloads[0])
	if err != nil {
		t.Fatal(err)
	}
	file := stream.(*os.File)
	original, statErr := file.Stat()
	if err := errors.Join(statErr, stream.Close()); err != nil {
		t.Fatal(err)
	}
	owned, err := os.Stat(filepath.Join(layout, "blobs", payloads[0].Digest.Algorithm().String(), payloads[0].Digest.Encoded()))
	if err != nil || os.SameFile(original, owned) || owned.Mode().Perm()&0o222 != 0 {
		t.Fatal("private payload aliases source storage or remains writable", err)
	}
	if err := os.Chmod(file.Name(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file.Name(), bytes.Repeat([]byte("x"), int(payloads[0].Size)), 0o600); err != nil {
		t.Fatal(err)
	}
	assertGraphReaderImages(t, layout, record)
	fresh, err := NewGraphReader(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fresh.Close() }()
	if _, _, err := fresh.Lookup(t.Context(), store, record.Root); err == nil {
		t.Fatal("fresh reader accepted persistent source corruption")
	}
}

func TestGraphSourceRecordIsDefensiveAndArtifactIsImmutable(t *testing.T) {
	_, source, store, record, _, target := graphSourceFixture(t)
	artifact, err := target.Resolve(t.Context(), graphTag(record.Root))
	if err != nil {
		t.Fatal(err)
	}
	if source.Artifact().Digest != artifact.Digest {
		t.Fatal("source lost admitted artifact identity")
	}
	source.primary = true // Model primary admission for this identity-only fixture.
	source.MarkUsed(record.Results[1].ID)
	if reuse, err := source.CanReuseAt(t.Context(), store, record.Root, record.Results[1]); err != nil || !reuse {
		t.Fatal("used unchanged root result rejected", reuse, err)
	}
	for _, change := range []func(*GraphResult){
		func(result *GraphResult) { result.Metadata = json.RawMessage(`{"root_mode":448}`) },
		func(result *GraphResult) { result.Image.Size++ },
		func(result *GraphResult) { result.BaseImage = nil },
	} {
		current := record.Results[1]
		change(&current)
		if reuse, err := source.CanReuseAt(t.Context(), store, record.Root, current); err != nil || reuse {
			t.Fatal("changed result metadata or descriptor accepted", reuse, err)
		}
	}
	got, err := source.Record()
	if err != nil {
		t.Fatal(err)
	}
	got.Results[0].ID = "changed"
	got.Results[0].Metadata[0] = '['
	got.Results[1].BaseImage.MediaType = "changed"
	got.Metadata[0] = '['
	got, err = source.Record()
	if err != nil || !reflect.DeepEqual(got, record) {
		t.Fatal("caller changed admitted graph metadata", err)
	}
	changed, _ := graphReaderChangedTail(t, target.Target, record, nil)
	changed.Root = record.Root
	changed = publishGraphLookupRecord(t, target.Target, changed)
	if _, err := source.EnsureResult(t.Context(), record.Results[1].ID); err != nil {
		t.Fatal(err)
	}
	if target.fetches[changed.Results[1].Image.Digest] != 0 {
		t.Fatal("deferred retrieval re-resolved a changed graph tag")
	}
	if source.Artifact().Digest != artifact.Digest {
		t.Fatal("source artifact changed after tag replacement")
	}
}

func TestGraphSourceUsedResultMustBelongToRoot(t *testing.T) {
	store, record, _, target := graphLookupFixture(t, v1.MediaTypeImageLayer, nil)
	var metadata GraphMetadata
	if err := json.Unmarshal(record.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.Roots = []string{metadata.Nodes[0].ID} // Results belong only to another node.
	var err error
	record.Metadata, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	record = publishGraphLookupRecord(t, target, record)
	reader, err := NewGraphReader(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	source, err := reader.LookupMetadata(t.Context(), store, record.Root)
	if err != nil {
		t.Fatal(err)
	}
	source.primary = true
	source.MarkUsed(record.Results[0].ID)
	if reuse, err := source.CanReuseAt(t.Context(), store, record.Root, record.Results[0]); err != nil || reuse {
		t.Fatal("non-root use suppressed root publication", reuse, err)
	}
}

func TestGraphSourceRecordPreservesRawMetadata(t *testing.T) {
	store, record, _, target := graphLookupFixture(t, v1.MediaTypeImageLayer, nil)
	record.Metadata = bytes.ReplaceAll(record.Metadata, []byte("\":"), []byte("\": "))
	compactMetadata, err := json.Marshal(record.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	record.Results[0].Metadata = json.RawMessage(`{ "result": [ 3, 4 ] }`)
	// Artifact encoders may preserve whitespace in embedded raw metadata.
	// Keep sidecars canonical, but publish a valid noncanonical config value.
	record = publishGraphLookupRecord(t, target.Target, record)
	artifact, err := target.Resolve(t.Context(), graphTag(record.Root))
	if err != nil {
		t.Fatal(err)
	}
	index, manifest := graphTestMetadata(t, target, artifact)
	configData, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	configData = []byte(strings.ReplaceAll(string(configData), string(compactMetadata), string(record.Metadata)))
	configData = []byte(strings.ReplaceAll(string(configData), `{"result":[3,4]}`, string(record.Results[0].Metadata)))
	config := pushSourceTestJSON(t, target, GraphConfigMediaType, configData)
	manifest.Config = config
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	index.Manifests[0] = pushSourceTestJSON(t, target, v1.MediaTypeImageManifest, manifestData)
	artifact = pushGraphTestJSON(t, target, v1.MediaTypeImageIndex, index)
	if err := target.Tag(t.Context(), artifact, graphTag(record.Root)); err != nil {
		t.Fatal(err)
	}
	reader, err := NewGraphReader(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	source, err := reader.LookupMetadata(t.Context(), store, record.Root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := source.Record()
	if err != nil || string(got.Metadata) != string(record.Metadata) || string(got.Results[0].Metadata) != string(record.Results[0].Metadata) {
		t.Fatal("defensive copying changed raw metadata bytes", err, string(got.Metadata), string(got.Results[0].Metadata))
	}
}

func pushSourceTestJSON(t *testing.T, target *graphLookupTarget, mediaType string, data []byte) v1.Descriptor {
	t.Helper()
	descriptor := v1.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
	if err := pushGraphBlob(t.Context(), target, descriptor, data); err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func TestGraphSourceAdmissionTTLDoesNotExpireOwnedMetadata(t *testing.T) {
	_, source, store, record, _, _ := graphSourceFixture(t)
	expired := time.Nanosecond
	store.ttl = &expired
	if _, err := source.EnsureResult(t.Context(), record.Results[0].ID); err != nil {
		t.Fatal("materialization reapplied admission TTL", err)
	}
	if _, _, err := store.LookupGraph(t.Context(), record.Root); !errors.Is(err, ErrMiss) {
		t.Fatal("fresh admission ignored TTL", err)
	}
}

func TestGraphSourceUnavailablePayloadCanRetryWithoutTagReplacement(t *testing.T) {
	_, source, _, record, payloads, target := graphSourceFixture(t)
	target.alter, target.failure = payloads[0].Digest, "missing"
	if _, err := source.EnsureResult(t.Context(), record.Results[0].ID); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatal("unavailable selected content was accepted", err)
	}
	target.failure = ""
	if _, err := source.EnsureResult(t.Context(), record.Results[0].ID); err != nil {
		t.Fatal("failed materialization poisoned its retry", err)
	}
}

func TestGraphSourceRemovalBeforeAndAfterMaterialization(t *testing.T) {
	for _, materialized := range []bool{false, true} {
		t.Run(fmt.Sprintf("materialized=%t", materialized), func(t *testing.T) {
			_, source, _, record, _, target := graphSourceFixture(t)
			selected := record.Results[1]
			if materialized {
				if _, err := source.EnsureResult(t.Context(), selected.ID); err != nil {
					t.Fatal(err)
				}
			}
			replacement, _ := graphReaderChangedTail(t, target.Target, record, nil)
			replacement.Root = record.Root
			replacement = publishGraphLookupRecord(t, target.Target, replacement)
			if err := target.Target.(*orasoci.Store).Delete(t.Context(), selected.Image); err != nil {
				t.Fatal(err)
			}
			layout, err := source.EnsureResult(t.Context(), selected.ID)
			if materialized {
				if err != nil {
					t.Fatal("owned private result still required removed source", err)
				}
				owned := record
				owned.Results = []GraphResult{selected}
				assertGraphReaderImages(t, layout, owned)
			} else if !errors.Is(err, errdef.ErrNotFound) {
				t.Fatal("removed unmaterialized result did not fail clearly", err)
			}
			if target.fetches[replacement.Results[1].Image.Digest] != 0 {
				t.Fatal("source removal substituted a replacement graph tag")
			}
		})
	}
}

func TestGraphSourceConcurrentSelectionFetchesSharedPayloadOnce(t *testing.T) {
	_, source, _, record, payloads, target := graphSourceFixture(t)
	var work sync.WaitGroup
	errors := make(chan error, 8)
	for range cap(errors) {
		work.Go(func() {
			_, err := source.EnsureResult(t.Context(), record.Results[0].ID)
			errors <- err
		})
	}
	work.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if target.fetches[payloads[0].Digest] != 1 {
		t.Fatalf("concurrent retrieval fetched shared payload %d times", target.fetches[payloads[0].Digest])
	}
}

func TestGraphSourceCanceledWaiterAndFetchRemainRetryable(t *testing.T) {
	_, source, _, record, payloads, target := graphSourceFixture(t)
	target.alter, target.failure, target.blocked = payloads[0].Digest, "cancel", make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := source.EnsureResult(ctx, record.Results[0].ID)
		result <- err
	}()
	select {
	case <-target.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("selected payload did not reach cancellation gate")
	}
	waiter, stop := context.WithCancel(t.Context())
	stop()
	if _, err := source.EnsureAll(waiter); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled waiter entered materialization", err)
	}
	select {
	case err := <-result:
		t.Fatal("canceling waiter interrupted active fetch", err)
	default:
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("fetch cancellation was lost", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("selected fetch did not drain cancellation")
	}
	target.failure = ""
	if _, err := source.EnsureAll(t.Context()); err != nil {
		t.Fatal("canceled fetch poisoned source", err)
	}
	if target.opened != target.closed {
		t.Fatal("canceled source leaked streams", target.opened, target.closed)
	}
}

func TestGraphSourceCloseDrainsActiveFetchAndRejectsNewWork(t *testing.T) {
	reader, source, store, record, payloads, target := graphSourceFixture(t)
	target.alter, target.failure, target.blocked = payloads[0].Digest, "cancel", make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	active := make(chan error, 1)
	go func() { _, err := source.EnsureAll(ctx); active <- err }()
	select {
	case <-target.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("selected payload did not reach close gate")
	}
	closed := make(chan error, 1)
	go func() { closed <- reader.Close() }()
	select {
	case <-reader.closing:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not stop admission")
	}
	if _, err := reader.LookupMetadata(t.Context(), store, record.Root); err == nil {
		t.Fatal("closing reader admitted metadata")
	}
	if _, err := source.EnsureResult(t.Context(), record.Results[0].ID); err == nil {
		t.Fatal("closing reader admitted retrieval")
	}
	select {
	case err := <-closed:
		t.Fatal("reader removed active materialization before cancellation", err)
	default:
	}
	cancel()
	select {
	case err := <-active:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("active retrieval did not drain")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader close did not finish")
	}
	if _, err := os.Stat(reader.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("closed reader left its private layout", err)
	}
}

func TestGraphSourceAdmissionChecksMetadataAndBounds(t *testing.T) {
	for _, fault := range []string{"corrupt", "truncated", "trailing", "close"} {
		t.Run(fault, func(t *testing.T) {
			store, record, _, target := graphLookupFixture(t, v1.MediaTypeImageLayer, nil)
			desc, err := record.Results[0].Descriptor()
			if err != nil {
				t.Fatal(err)
			}
			target.alter, target.failure = desc.Digest, fault
			reader, err := NewGraphReader(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reader.Close() })
			if _, err := reader.LookupMetadata(t.Context(), store, record.Root); err == nil {
				t.Fatal("invalid sidecar admitted")
			}
			if len(reader.sources) != 0 || len(reader.declared) != 0 || reader.metadataBytes != 0 {
				t.Fatal("failed metadata admission retained state")
			}
			if target.opened != target.closed {
				t.Fatal("failed metadata admission leaked streams")
			}
		})
	}
	t.Run("byte budget", func(t *testing.T) {
		reader, source, store, record, _, target := graphSourceFixture(t)
		reader.metadataBytes = maxGraphReaderMetadataBytes
		if _, err := reader.LookupMetadata(t.Context(), store, record.Root); err != nil {
			t.Fatal("deduplicated handle consumed more budget", err)
		}
		second := record
		second.Root = digest.FromString("another admission")
		second = publishGraphLookupRecord(t, target.Target, second)
		if _, err := reader.LookupMetadata(t.Context(), store, second.Root); err == nil || !strings.Contains(err.Error(), "metadata limit") {
			t.Fatal("metadata byte budget was ignored", err)
		}
		if len(reader.sources) != 1 || reader.sources[graphSourceKey{store: store, artifact: source.Artifact().Digest}] != source {
			t.Fatal("failed bounded admission changed the previous source")
		}
	})
	t.Run("handle count", func(t *testing.T) {
		reader, _, store, record, _, target := graphSourceFixture(t)
		for i := range maxGraphEntries - 1 {
			reader.sources[graphSourceKey{artifact: digest.FromString(fmt.Sprint(i))}] = &GraphSource{}
		}
		second := record
		second.Root = digest.FromString("another admission")
		second = publishGraphLookupRecord(t, target.Target, second)
		if _, err := reader.LookupMetadata(t.Context(), store, second.Root); err == nil {
			t.Fatal("metadata handle bound was ignored")
		}
	})
}

func TestGraphSourceUnmaterializedRootsCannotBecomeOpaquePayloads(t *testing.T) {
	reader, _, _, first, _, target := graphSourceFixture(t)
	second, _ := graphReaderChangedTail(t, target.Target, first, func(manifest *v1.Manifest, _ *v1.Image) {
		manifest.Layers[0] = first.Results[0].Image
		manifest.Layers[0].MediaType = v1.MediaTypeImageLayer
	})
	source, err := reader.LookupMetadata(t.Context(), &OCIStore{target: target, stagingDir: t.TempDir()}, second.Root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.EnsureResult(t.Context(), second.Results[1].ID); err == nil {
		t.Fatal("unmaterialized admitted root was reinterpreted as opaque payload")
	}
}
