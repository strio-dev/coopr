package cache

import (
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	digest "github.com/opencontainers/go-digest"
	"github.com/strio-dev/coopr/internal/oci"
)

type graphRegistryBytes struct {
	http.ResponseWriter
	bytes int64
}

func (w *graphRegistryBytes) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	w.bytes += int64(n)
	return n, err
}

func TestGraphSourceRegistrySelectiveGETBytesAndCompleteExport(t *testing.T) {
	originalReader, source, local, record, payloads, localTarget := graphSourceFixture(t)
	sourceLayout, err := source.EnsureAll(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	requests := map[digest.Digest]int{}
	transferred := map[digest.Digest]int64{}
	handler := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counted := &graphRegistryBytes{ResponseWriter: w}
		handler.ServeHTTP(counted, r)
		if r.Method == http.MethodGet {
			_, name, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v2/coopr/source/"), "/")
			key := digest.Digest(name)
			if key.Validate() == nil {
				mu.Lock()
				requests[key]++
				transferred[key] += counted.bytes
				mu.Unlock()
			}
		}
	}))
	defer server.Close()
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := NewRegistryStore(resolver, strings.TrimPrefix(server.URL, "http://")+"/coopr/source", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.PutGraph(t.Context(), record.Root, record, sourceLayout); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	clear(requests)
	clear(transferred)
	mu.Unlock()
	reader, err := NewGraphReader(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	selected, err := reader.LookupMetadata(t.Context(), remote, record.Root)
	if err != nil {
		t.Fatal(err)
	}
	selected.MarkUsed("not-bound")
	if reuse, err := selected.CanReuseAt(t.Context(), remote, record.Root, record.Results[0]); err != nil || reuse || len(selected.Metadata().UsedResults) != 0 {
		t.Fatal("metadata admission or an unknown result manufactured use", reuse, err)
	}
	mu.Lock()
	for _, descriptor := range append(graphImages(record), payloads...) {
		if requests[descriptor.Digest] != 0 || transferred[descriptor.Digest] != 0 {
			t.Errorf("metadata fetched image %s: GETs=%d bytes=%d", descriptor.Digest, requests[descriptor.Digest], transferred[descriptor.Digest])
		}
	}
	mu.Unlock()
	admitted := selected.Artifact()
	changed, changedConfig := graphReaderChangedTail(t, localTarget.Target, record, nil)
	changedSource, err := originalReader.LookupMetadata(t.Context(), local, changed.Root)
	if err != nil {
		t.Fatal(err)
	}
	changedLayout, err := changedSource.EnsureAll(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	changed.Root = record.Root
	if _, err := remote.PutGraph(t.Context(), changed.Root, changed, changedLayout); err != nil {
		t.Fatal(err)
	}
	if selected.Artifact().Digest != admitted.Digest {
		t.Fatal("mutable tag changed admitted artifact")
	}
	path, err := selected.EnsureResult(t.Context(), record.Results[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	first := record
	first.Results = record.Results[:1]
	assertGraphReaderImages(t, path, first)
	if reuse, err := selected.CanReuseAt(t.Context(), remote, record.Root, record.Results[0]); err != nil || reuse {
		t.Fatal("layout materialization alone manufactured native use", reuse, err)
	}
	// The native caller reports successful selected import. Reuse performs no
	// network work and does not require the admitted source tag to stay current.
	selected.MarkUsed(record.Results[0].ID)
	mu.Lock()
	beforeReuse := maps.Clone(requests)
	mu.Unlock()
	if reuse, err := selected.CanReuseAt(t.Context(), remote, record.Root, record.Results[0]); err != nil || !reuse {
		t.Fatal("successfully used unchanged primary result was not reused", reuse, err)
	}
	mu.Lock()
	if !maps.Equal(requests, beforeReuse) {
		t.Error("use receipt triggered remote validation")
	}
	if requests[record.Results[1].Image.Digest] != 0 || requests[payloads[2].Digest] != 0 {
		t.Error("selection downloaded unused alternative")
	}
	mu.Unlock()
	if _, err := selected.EnsureAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertGraphReaderImages(t, path, record)
	mu.Lock()
	defer mu.Unlock()
	for _, descriptor := range payloads {
		if requests[descriptor.Digest] != 1 || transferred[descriptor.Digest] != descriptor.Size {
			t.Errorf("complete retained graph %s: GETs=%d bytes=%d want exactly1/%d", descriptor.Digest, requests[descriptor.Digest], transferred[descriptor.Digest], descriptor.Size)
		}
	}
	for _, descriptor := range graphImages(record) {
		// The bounded manifest parser and upstream ORAS traversal each fetch
		// metadata; bulk image payloads above still cross the network once.
		if requests[descriptor.Digest] != 2 || transferred[descriptor.Digest] != 2*descriptor.Size {
			t.Errorf("image metadata %s: GETs=%d bytes=%d want2/%d", descriptor.Digest, requests[descriptor.Digest], transferred[descriptor.Digest], 2*descriptor.Size)
		}
	}
	if requests[changed.Results[1].Image.Digest] != 0 || requests[changedConfig.Digest] != 0 {
		t.Error("deferred retrieval followed the advanced registry tag")
	}
	// The layer is shared by both retained alternatives and must cross HTTP once.
	t.Logf("two retained image roots and three payloads: selected restore omitted the alternative; full export retrieved payloads once and image manifests twice (%d shared-layer bytes)", payloads[0].Size)
}
