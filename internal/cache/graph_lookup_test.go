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
	"sync"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

var errGraphLookupClose = errors.New("injected graph payload close failure")

type graphLookupTarget struct {
	oras.Target
	mu                   sync.Mutex
	fetches              map[digest.Digest]int
	read                 map[digest.Digest]int64
	opened               int
	closed               int
	alter                digest.Digest
	failure              string
	closeFailure         error
	closeFailureOnce     bool
	closeFailureInjected bool
	blocked              chan struct{}
	blockOnce            sync.Once
}

func (target *graphLookupTarget) Fetch(ctx context.Context, desc v1.Descriptor) (io.ReadCloser, error) {
	target.mu.Lock()
	target.fetches[desc.Digest]++
	target.mu.Unlock()
	if desc.Digest == target.alter && target.failure == "missing" {
		return nil, errdef.ErrNotFound
	}
	stream, err := target.Target.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	if desc.Digest == target.alter && target.failure != "" && target.failure != "cancel" && target.failure != "close" {
		data, err := io.ReadAll(stream)
		closeErr := stream.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return nil, err
		}
		switch target.failure {
		case "corrupt":
			data = bytes.Repeat([]byte("x"), len(data))
		case "truncated":
			data = data[:len(data)-1]
		case "trailing":
			data = append(data, 'x')
		}
		stream = io.NopCloser(bytes.NewReader(data))
	}
	target.mu.Lock()
	target.opened++
	target.mu.Unlock()
	return &graphLookupReader{ReadCloser: stream, target: target, ctx: ctx, descriptor: desc}, nil
}

type graphLookupReader struct {
	io.ReadCloser
	target     *graphLookupTarget
	ctx        context.Context
	descriptor v1.Descriptor
	closeOnce  sync.Once
	closeErr   error
}

func (reader *graphLookupReader) Read(p []byte) (int, error) {
	if reader.descriptor.Digest == reader.target.alter && reader.target.failure == "cancel" {
		reader.target.blockOnce.Do(func() { close(reader.target.blocked) })
		<-reader.ctx.Done()
		return 0, reader.ctx.Err()
	}
	n, err := reader.ReadCloser.Read(p)
	reader.target.mu.Lock()
	reader.target.read[reader.descriptor.Digest] += int64(n)
	reader.target.mu.Unlock()
	return n, err
}

func (reader *graphLookupReader) Close() error {
	reader.closeOnce.Do(func() {
		reader.closeErr = reader.ReadCloser.Close()
		if reader.descriptor.Digest == reader.target.alter && reader.target.failure == "close" {
			reader.closeErr = errors.Join(reader.closeErr, errGraphLookupClose)
		}
		reader.target.mu.Lock()
		if reader.descriptor.Digest == reader.target.alter && reader.target.closeFailure != nil && (!reader.target.closeFailureOnce || !reader.target.closeFailureInjected) {
			reader.closeErr = errors.Join(reader.closeErr, reader.target.closeFailure)
			reader.target.closeFailureInjected = true
		}
		reader.target.closed++
		reader.target.mu.Unlock()
	})
	return reader.closeErr
}

// Build two physical image alternatives sharing one layer. Publish metadata
// directly so malformed or nondistributable-layer fixtures reach LookupGraph
// without PutGraph's independent validation/copy policy masking the case.
func graphLookupFixture(t *testing.T, layerMediaType string, changeSecondLayer func(*v1.Descriptor)) (*OCIStore, GraphRecord, []v1.Descriptor, *graphLookupTarget) {
	t.Helper()
	return graphLookupCustomLayerFixture(t, layerMediaType, changeSecondLayer, nil)
}

func graphLookupCustomLayerFixture(t *testing.T, layerMediaType string, changeSecondLayer func(*v1.Descriptor), createLayer func(oras.Target, v1.Descriptor) v1.Descriptor) (*OCIStore, GraphRecord, []v1.Descriptor, *graphLookupTarget) {
	t.Helper()
	ctx := t.Context()
	record, layout := graphFixture(t)
	target, err := orasoci.New(layout)
	if err != nil {
		t.Fatal(err)
	}
	first, err := imageGraphDescriptors(ctx, target, record.Results[0].Image)
	if err != nil {
		t.Fatal(err)
	}
	layer := first[1]
	layer.MediaType = layerMediaType
	if createLayer != nil {
		layer = createLayer(target, layer)
	}
	marshal := func(value any) []byte {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	push := func(mediaType string, data []byte) v1.Descriptor {
		t.Helper()
		desc := oci.Descriptor(mediaType, data)
		if err := pushGraphBlob(ctx, target, desc, data); err != nil {
			t.Fatal(err)
		}
		return desc
	}
	payloads := []v1.Descriptor{layer}
	for i := range record.Results {
		config := push(v1.MediaTypeImageConfig, marshal(v1.Image{
			Platform: v1.Platform{OS: "linux", Architecture: "amd64"},
			Config:   v1.ImageConfig{Labels: map[string]string{"variant": record.Results[i].ID}},
			RootFS:   v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{layer.Digest}},
		}))
		payloads = append(payloads, config)
		imageLayer := layer
		if i == 1 && changeSecondLayer != nil {
			changeSecondLayer(&imageLayer)
		}
		record.Results[i].Image = push(v1.MediaTypeImageManifest, marshal(oci.VersionedManifest(config, []v1.Descriptor{imageLayer}, "")))
	}
	record.Results[1].BaseImage = &record.Results[0].Image
	record = publishGraphLookupRecord(t, target, record)
	counted := &graphLookupTarget{Target: target, fetches: map[digest.Digest]int{}, read: map[digest.Digest]int64{}}
	return &OCIStore{target: counted, stagingDir: t.TempDir()}, record, payloads, counted
}

func publishGraphLookupRecord(t *testing.T, target oras.Target, record GraphRecord) GraphRecord {
	t.Helper()
	layers := make([]v1.Descriptor, 0, len(record.Results))
	for _, result := range record.Results {
		desc := pushGraphTestJSON(t, target, GraphResultMediaType, result)
		layers = append(layers, desc)
	}
	config := pushGraphTestJSON(t, target, GraphConfigMediaType, record)
	metadata := pushGraphTestJSON(t, target, v1.MediaTypeImageManifest, oci.VersionedManifest(config, layers, GraphArtifactType))
	artifact := pushGraphTestJSON(t, target, v1.MediaTypeImageIndex, oci.VersionedIndex(append([]v1.Descriptor{metadata}, graphImages(record)...), GraphArtifactType))
	if err := target.Tag(t.Context(), artifact, graphTag(record.Root)); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestGraphMetadataAdmissionRejectsIndexAndArtifactBindingChanges(t *testing.T) {
	for _, fault := range []string{
		"legacy manifest root", "empty index", "missing image", "extra image", "reordered images", "image size",
		"metadata descriptor hint", "metadata type", "metadata subject", "config type", "image as sidecar", "extra sidecar",
	} {
		t.Run(fault, func(t *testing.T) {
			store, record, _, target := graphLookupFixture(t, v1.MediaTypeImageLayer, nil)
			root, err := target.Resolve(t.Context(), graphTag(record.Root))
			if err != nil {
				t.Fatal(err)
			}
			index, manifest := graphTestMetadata(t, target, root)
			switch fault {
			case "metadata type":
				manifest.ArtifactType = "application/vnd.coopr.strio.dev.graph-cache.v1+json"
			case "metadata subject":
				manifest.Subject = &record.Results[0].Image
			case "config type":
				manifest.Config.MediaType = v1.MediaTypeImageConfig
			case "image as sidecar":
				manifest.Layers[0] = record.Results[0].Image
			case "extra sidecar":
				manifest.Layers = append(manifest.Layers, manifest.Layers[0])
			}
			metadata := pushGraphTestJSON(t, target, v1.MediaTypeImageManifest, manifest)
			index.Manifests[0] = metadata
			switch fault {
			case "empty index":
				index.Manifests = nil
			case "missing image":
				index.Manifests = index.Manifests[:len(index.Manifests)-1]
			case "extra image":
				index.Manifests = append(index.Manifests, index.Manifests[1])
			case "reordered images":
				index.Manifests[1], index.Manifests[2] = index.Manifests[2], index.Manifests[1]
			case "image size":
				index.Manifests[1].Size++
			case "metadata descriptor hint":
				index.Manifests[0].ArtifactType = GraphArtifactType
			}
			root = pushGraphTestJSON(t, target, v1.MediaTypeImageIndex, index)
			if fault == "legacy manifest root" {
				root = metadata
			}
			if err := target.Tag(t.Context(), root, graphTag(record.Root)); err != nil {
				t.Fatal(err)
			}
			reader, err := NewGraphReader(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reader.Close() }()
			if _, err := reader.LookupMetadata(t.Context(), store, record.Root); err == nil {
				t.Fatal("changed index or metadata binding admitted")
			}
			if len(reader.sources) != 0 || reader.metadataBytes != 0 {
				t.Fatal("rejected metadata retained a source or byte charge")
			}
			for _, image := range graphImages(record) {
				if target.fetches[image.Digest] != 0 {
					t.Fatal("invalid metadata fetched an image before rejecting its bindings")
				}
			}
		})
	}
}

func assertGraphLookupCleaned(t *testing.T, store *OCIStore, target *graphLookupTarget) {
	t.Helper()
	entries, err := os.ReadDir(store.stagingDir)
	if err != nil || len(entries) != 0 {
		t.Errorf("failed lookup leaked staging files: entries=%v err=%v", entries, err)
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	if target.opened != target.closed {
		t.Errorf("lookup leaked readers: opened=%d closed=%d", target.opened, target.closed)
	}
}

func TestGraphLookupFetchesSharedPayloadOnce(t *testing.T) {
	store, record, payloads, target := graphLookupFixture(t, v1.MediaTypeImageLayer, nil)
	got, path, err := store.LookupGraph(t.Context(), record.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	if !reflect.DeepEqual(*got, record) {
		t.Fatal("lookup lost graph alternatives, dependency or restoration metadata")
	}
	images, err := orasoci.New(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range graphImages(record) {
		if err := verifyImageGraph(t.Context(), images, image); err != nil {
			t.Fatalf("incomplete restored physical alternative %s: %v", image.Digest, err)
		}
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	for _, payload := range payloads {
		if target.fetches[payload.Digest] != 1 || target.read[payload.Digest] != payload.Size {
			t.Errorf("payload %s fetched %d times / read %d bytes; want once / %d bytes", payload.Digest, target.fetches[payload.Digest], target.read[payload.Digest], payload.Size)
		}
	}
	if target.opened != target.closed {
		t.Errorf("lookup leaked readers: opened=%d closed=%d", target.opened, target.closed)
	}
}

func TestGraphReaderReusesVerifiedPayloadsAcrossPrefixesAndStores(t *testing.T) {
	store, first, payloads, target := graphLookupFixture(t, v1.MediaTypeImageLayer, nil)
	second := first
	second.Root = digest.FromString("second prefix")
	second = publishGraphLookupRecord(t, target.Target, second)
	otherTarget := &graphLookupTarget{Target: target.Target, fetches: map[digest.Digest]int{}, read: map[digest.Digest]int64{}}
	otherStore := &OCIStore{target: otherTarget, stagingDir: t.TempDir()}
	reader, err := NewGraphReader(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	var layout string
	for i, record := range []GraphRecord{first, second} {
		selected := store
		if i == 1 {
			selected = otherStore
		}
		got, path, err := reader.Lookup(t.Context(), selected, record.Root)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(*got, record) || i > 0 && path != layout {
			t.Fatal("reader changed graph alternatives or returned a per-lookup layout")
		}
		layout = path
		assertGraphReaderImages(t, layout, record)
	}
	var actual, unique int64
	for _, payload := range payloads {
		actual += target.read[payload.Digest] + otherTarget.read[payload.Digest]
		unique += payload.Size
		if target.fetches[payload.Digest] != 1 || target.read[payload.Digest] != payload.Size || otherTarget.fetches[payload.Digest] != 0 {
			t.Errorf("cross-prefix payload %s fetched %d+%d times / %d+%d bytes; want1+0/%d+0", payload.Digest, target.fetches[payload.Digest], otherTarget.fetches[payload.Digest], target.read[payload.Digest], otherTarget.read[payload.Digest], payload.Size)
		}
	}
	t.Logf("complete-prefix shared-reader payload bytes=%d; unique verified payload bytes=%d", actual, unique)
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(layout); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reader did not release its owned layout", err)
	}
	if _, _, err := reader.Lookup(t.Context(), store, first.Root); err == nil {
		t.Fatal("closed reader accepted a lookup")
	}
}

func assertGraphReaderImages(t *testing.T, layout string, record GraphRecord) {
	t.Helper()
	images, err := orasoci.New(layout)
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range graphImages(record) {
		if err := verifyImageGraph(t.Context(), images, image); err != nil {
			t.Fatal("reader lost a complete physical alternative", err)
		}
	}
}

func graphReaderChangedTail(t *testing.T, target oras.Target, first GraphRecord, change func(*v1.Manifest, *v1.Image)) (GraphRecord, v1.Descriptor) {
	t.Helper()
	second := first
	second.Root = digest.FromString("changed tail prefix")
	second.Results = append([]GraphResult(nil), first.Results...)
	data, err := fetchVerified(t.Context(), target, first.Results[1].Image, maxImageRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	data, err = fetchVerified(t.Context(), target, manifest.Config, maxImageRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	var config v1.Image
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	config.Config.Labels["prefix"] = "second"
	if change != nil {
		change(&manifest, &config)
	}
	manifest.Config = pushGraphTestJSON(t, target, v1.MediaTypeImageConfig, config)
	second.Results[1].Image = pushGraphTestJSON(t, target, v1.MediaTypeImageManifest, manifest)
	return publishGraphLookupRecord(t, target, second), manifest.Config
}

func TestGraphReaderRejectsCrossPrefixDescriptorConflicts(t *testing.T) {
	for _, field := range []string{"size", "media type"} {
		t.Run(field, func(t *testing.T) {
			store, first, _, target := graphLookupFixture(t, v1.MediaTypeImageLayer, nil)
			second, _ := graphReaderChangedTail(t, target.Target, first, func(manifest *v1.Manifest, _ *v1.Image) {
				if field == "size" {
					manifest.Layers[0].Size++
				} else {
					manifest.Layers[0].MediaType = v1.MediaTypeImageLayerGzip
				}
			})
			reader, err := NewGraphReader(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reader.Close() })
			_, layout, err := reader.Lookup(t.Context(), store, first.Root)
			if err != nil {
				t.Fatal(err)
			}
			if got, path, err := reader.Lookup(t.Context(), store, second.Root); err == nil || got != nil || path != "" {
				t.Fatal("cross-prefix descriptor conflict was accepted", got, path, err)
			}
			assertGraphReaderImages(t, layout, first)
		})
	}
}

func TestGraphReaderRejectsCrossPrefixImageRootPayloadAlias(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%v", reverse), func(t *testing.T) {
			store, first, _, target := graphLookupFixture(t, v1.MediaTypeImageLayer, nil)
			data, err := fetchVerified(t.Context(), target.Target, first.Results[1].Image, maxImageRecordBytes)
			if err != nil {
				t.Fatal(err)
			}
			var manifest v1.Manifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			manifest.Config = first.Results[0].Image
			manifest.Config.MediaType = v1.MediaTypeImageConfig
			data, err = json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			image := oci.Descriptor(v1.MediaTypeImageManifest, data)
			if err := pushGraphBlob(t.Context(), target.Target, image, data); err != nil {
				t.Fatal(err)
			}
			second := first
			second.Root = digest.FromString("root reused as payload in another prefix")
			second.Results = append([]GraphResult(nil), first.Results...)
			for i := range second.Results {
				second.Results[i].Image, second.Results[i].BaseImage = image, nil
			}
			second = publishGraphLookupRecord(t, target.Target, second)
			if reverse {
				first, second = second, first
			}
			reader, err := NewGraphReader(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reader.Close() })
			_, layout, err := reader.Lookup(t.Context(), store, first.Root)
			if err != nil {
				t.Fatal(err)
			}
			if got, path, err := reader.Lookup(t.Context(), store, second.Root); err == nil || got != nil || path != "" {
				t.Fatal("cross-prefix image root/payload alias accepted", got, path, err)
			}
			assertGraphReaderImages(t, layout, first)
		})
	}
}

func TestGraphReaderRejectsCorruptRetainedContent(t *testing.T) {
	for _, kind := range []string{"root", "nested manifest", "nested index", "nested layer", "ordinary payload in place"} {
		t.Run(kind, func(t *testing.T) {
			var nested []v1.Descriptor
			store, record, payloads, _ := graphLookupCustomLayerFixture(t, v1.MediaTypeImageLayer, nil, func(target oras.Target, child v1.Descriptor) v1.Descriptor {
				if kind == "ordinary payload in place" {
					// Keep direct leaf reuse enabled for its in-place integrity case.
					return child
				}
				var index v1.Descriptor
				index, nested = graphLookupNestedLayer(t, target, child, v1.MediaTypeImageIndex)
				return index
			})
			reader, err := NewGraphReader(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reader.Close() })
			_, layout, err := reader.Lookup(t.Context(), store, record.Root)
			if err != nil {
				t.Fatal(err)
			}
			descriptor := record.Results[0].Image
			if kind == "ordinary payload in place" {
				descriptor = payloads[0]
			} else if kind != "root" {
				want := v1.MediaTypeImageManifest
				switch kind {
				case "nested index":
					want = v1.MediaTypeImageIndex
				case "nested layer":
					want = v1.MediaTypeImageLayer
				}
				found := false
				for _, candidate := range nested {
					if candidate.MediaType == want {
						descriptor, found = candidate, true
						break
					}
				}
				if !found {
					t.Fatal("fixture lacks selected nested descriptor", kind)
				}
			}
			path := filepath.Join(layout, "blobs", descriptor.Digest.Algorithm().String(), descriptor.Digest.Encoded())
			if kind == "ordinary payload in place" {
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, int(descriptor.Size)), 0600); err != nil {
				t.Fatal(err)
			}
			if got, path, err := reader.Lookup(t.Context(), store, record.Root); err == nil || got != nil || path != "" {
				t.Fatal("corrupt retained content accepted", got, path, err)
			}
		})
	}
}

func TestGraphReaderFailedUniquePayloadRetainsPriorGraphAndAllowsRetry(t *testing.T) {
	for _, failure := range []string{"missing", "corrupt", "truncated", "trailing", "close"} {
		t.Run(failure, func(t *testing.T) {
			store, first, _, target := graphLookupFixture(t, v1.MediaTypeImageLayer, nil)
			second, tail := graphReaderChangedTail(t, target.Target, first, nil)
			reader, err := NewGraphReader(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reader.Close() })
			_, layout, err := reader.Lookup(t.Context(), store, first.Root)
			if err != nil {
				t.Fatal(err)
			}
			target.alter, target.failure = tail.Digest, failure
			if got, path, err := reader.Lookup(t.Context(), store, second.Root); err == nil || got != nil || path != "" {
				t.Fatal("invalid unique payload was accepted", got, path, err)
			}
			if _, admitted := reader.verified[tail.Digest]; admitted {
				t.Fatal("failed payload entered the reusable set")
			}
			assertGraphReaderImages(t, layout, first)
			target.failure = ""
			if _, path, err := reader.Lookup(t.Context(), store, second.Root); err != nil || path != layout {
				t.Fatal("retry did not reuse the prior complete layout", path, err)
			}
			assertGraphReaderImages(t, layout, second)
			if target.fetches[tail.Digest] != 2 {
				t.Fatal("retry trusted a failed payload instead of fetching again", target.fetches[tail.Digest])
			}
			if target.opened != target.closed {
				t.Fatal("reader leaked payload streams", target.opened, target.closed)
			}
		})
	}
}

func TestGraphReaderConcurrentLookupsFetchPayloadOnce(t *testing.T) {
	store, first, payloads, target := graphLookupFixture(t, v1.MediaTypeImageLayer, nil)
	second := first
	second.Root = digest.FromString("concurrent prefix")
	second = publishGraphLookupRecord(t, target.Target, second)
	reader, err := NewGraphReader(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	done := make(chan error, 2)
	for _, root := range []digest.Digest{first.Root, second.Root} {
		go func() {
			_, _, err := reader.Lookup(t.Context(), store, root)
			done <- err
		}()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	for _, payload := range payloads {
		if target.fetches[payload.Digest] != 1 || target.read[payload.Digest] != payload.Size {
			t.Fatal("concurrent lookups duplicated payload fetch", payload.Digest, target.fetches[payload.Digest], target.read[payload.Digest])
		}
	}
}

func TestGraphReaderCanceledWaiterAndFetchDoNotPoisonReuse(t *testing.T) {
	store, record, payloads, target := graphLookupFixture(t, v1.MediaTypeImageLayer, nil)
	target.alter, target.failure, target.blocked = payloads[0].Digest, "cancel", make(chan struct{})
	reader, err := NewGraphReader(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := reader.Lookup(ctx, store, record.Root)
		done <- err
	}()
	select {
	case <-target.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("first lookup did not reach blocking payload")
	}
	waiter, stopWaiter := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer stopWaiter()
	if _, _, err := reader.Lookup(waiter, store, record.Root); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("waiting reader did not honor cancellation", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("active reader lost cancellation", err)
	}
	if _, admitted := reader.verified[payloads[0].Digest]; admitted {
		t.Fatal("canceled payload entered reusable set")
	}
	target.failure = ""
	if _, layout, err := reader.Lookup(t.Context(), store, record.Root); err != nil {
		t.Fatal("canceled fetch poisoned a later lookup", err)
	} else {
		assertGraphReaderImages(t, layout, record)
	}
	if target.opened != target.closed {
		t.Fatal("cancellation leaked readers", target.opened, target.closed)
	}
}

//nolint:staticcheck // Existing nondistributable cache layers remain supported.
func TestGraphReaderRetainsNestedAndNondistributableGraphs(t *testing.T) {
	for _, mediaType := range []string{v1.MediaTypeImageManifest, v1.MediaTypeImageIndex, v1.MediaTypeImageLayerNonDistributableGzip} {
		t.Run(mediaType, func(t *testing.T) {
			var nested []v1.Descriptor
			var createLayer func(oras.Target, v1.Descriptor) v1.Descriptor
			if mediaType != v1.MediaTypeImageLayerNonDistributableGzip {
				createLayer = func(target oras.Target, layer v1.Descriptor) v1.Descriptor {
					var root v1.Descriptor
					root, nested = graphLookupNestedLayer(t, target, layer, mediaType)
					return root
				}
			}
			store, record, _, _ := graphLookupCustomLayerFixture(t, mediaType, nil, createLayer)
			reader, err := NewGraphReader(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reader.Close() })
			for range 2 {
				_, path, err := reader.Lookup(t.Context(), store, record.Root)
				if err != nil {
					t.Fatal(err)
				}
				assertGraphReaderImages(t, path, record)
				images, err := orasoci.New(path)
				if err != nil {
					t.Fatal(err)
				}
				for _, descriptor := range nested {
					if _, err := fetchVerified(t.Context(), images, descriptor, maxImageRecordBytes); err != nil {
						t.Fatal("shared layout lost nested graph content", descriptor.Digest, err)
					}
				}
			}
		})
	}
}

func TestGraphReaderReleasesLocalSourceLockBetweenLookups(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	store, err := NewLocalStore(ctx, root, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	record, imageLayout := graphFixture(t)
	if _, err := store.PutGraph(ctx, record.Root, record, imageLayout); err != nil {
		t.Fatal(err)
	}
	reader, err := NewGraphReader(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if _, _, err := reader.Lookup(ctx, store, record.Root); err != nil {
		t.Fatal(err)
	}
	other, err := NewLocalStore(ctx, root, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.PutGraph(ctx, record.Root, record, imageLayout); err != nil {
		t.Fatal("reader held the local source lock past lookup", err)
	}
}

func TestGraphLookupRejectsInvalidPayloadAndCleansStaging(t *testing.T) {
	for _, name := range []string{"layer", "config"} {
		for _, failure := range []string{"corrupt", "truncated", "trailing", "missing", "close"} {
			t.Run(name+"/"+failure, func(t *testing.T) {
				store, record, payloads, target := graphLookupFixture(t, v1.MediaTypeImageLayer, nil)
				index := 0
				if name == "config" {
					index = 1
				}
				target.alter, target.failure = payloads[index].Digest, failure
				got, path, err := store.LookupGraph(t.Context(), record.Root)
				if err == nil || got != nil || path != "" {
					t.Fatalf("invalid payload accepted: record=%v path=%q err=%v", got, path, err)
				}
				if failure == "close" && !errors.Is(err, errGraphLookupClose) {
					t.Fatalf("payload close error was lost: %v", err)
				}
				assertGraphLookupCleaned(t, store, target)
			})
		}
	}
}

func TestGraphLookupRejectsSharedDescriptorConflictsAndExternalURLs(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*v1.Descriptor)
	}{
		{"size", func(desc *v1.Descriptor) { desc.Size++ }},
		{"media type", func(desc *v1.Descriptor) { desc.MediaType = v1.MediaTypeImageLayerGzip }},
		{"external URL", func(desc *v1.Descriptor) { desc.URLs = []string{"https://example.invalid/layer"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, record, _, target := graphLookupFixture(t, v1.MediaTypeImageLayer, test.change)
			got, path, err := store.LookupGraph(t.Context(), record.Root)
			if err == nil || got != nil || path != "" {
				t.Fatalf("invalid shared descriptor accepted: record=%v path=%q err=%v", got, path, err)
			}
			assertGraphLookupCleaned(t, store, target)
		})
	}
}

func TestGraphLookupRejectsPayloadAliasingNestedImageRoot(t *testing.T) {
	store, record, _, target := graphLookupFixture(t, v1.MediaTypeImageLayer, nil)
	imageManifest := func(desc v1.Descriptor) v1.Manifest {
		t.Helper()
		data, err := fetchVerified(t.Context(), target.Target, desc, maxImageRecordBytes)
		if err != nil {
			t.Fatal(err)
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatal(err)
		}
		return manifest
	}
	// Give the first root nested descendants that a false destination.Exists
	// hit would omit. The later image then calls that root's bytes its config.
	first := imageManifest(record.Results[0].Image)
	index := pushGraphTestJSON(t, target.Target, v1.MediaTypeImageIndex, v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{record.Results[0].Image},
	})
	first.Layers = []v1.Descriptor{index}
	record.Results[0].Image = pushGraphTestJSON(t, target.Target, v1.MediaTypeImageManifest, first)
	second := imageManifest(record.Results[1].Image)
	second.Config = record.Results[0].Image
	second.Config.MediaType = v1.MediaTypeImageConfig
	record.Results[1].Image = pushGraphTestJSON(t, target.Target, v1.MediaTypeImageManifest, second)
	record = publishGraphLookupRecord(t, target.Target, record)
	got, path, err := store.LookupGraph(t.Context(), record.Root)
	if err == nil || got != nil || path != "" {
		t.Fatalf("image root/config descriptor conflict accepted: record=%v path=%q err=%v", got, path, err)
	}
	assertGraphLookupCleaned(t, store, target)
}

func graphLookupNestedLayer(t *testing.T, target oras.Target, childLayer v1.Descriptor, mediaType string) (v1.Descriptor, []v1.Descriptor) {
	t.Helper()
	childLayer.MediaType = v1.MediaTypeImageLayer
	config := pushGraphTestJSON(t, target, v1.MediaTypeImageConfig, v1.Image{
		Platform: v1.Platform{OS: "linux", Architecture: "amd64"},
		Config:   v1.ImageConfig{Labels: map[string]string{"nested": "child"}},
		RootFS:   v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{childLayer.Digest}},
	})
	manifest := pushGraphTestJSON(t, target, v1.MediaTypeImageManifest, oci.VersionedManifest(config, []v1.Descriptor{childLayer}, ""))
	nested := []v1.Descriptor{childLayer, config, manifest}
	if mediaType == v1.MediaTypeImageManifest {
		return manifest, nested
	}
	index := pushGraphTestJSON(t, target, v1.MediaTypeImageIndex, v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{manifest},
	})
	return index, append(nested, index)
}

func TestGraphLookupNestedPayloadAliasKeepsRecursiveCopy(t *testing.T) {
	var nested []v1.Descriptor
	createLayer := func(target oras.Target, childLayer v1.Descriptor) v1.Descriptor {
		var index v1.Descriptor
		index, nested = graphLookupNestedLayer(t, target, childLayer, v1.MediaTypeImageIndex)
		return index
	}
	store, record, _, target := graphLookupCustomLayerFixture(t, v1.MediaTypeImageIndex, nil, createLayer)
	data, err := fetchVerified(t.Context(), target.Target, record.Results[1].Image, maxImageRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	var second v1.Manifest
	if err := json.Unmarshal(data, &second); err != nil {
		t.Fatal(err)
	}
	// A later config aliases a nested manifest, not a declared image root.
	// Prepopulating it would make CopyGraph skip the nested manifest's children.
	second.Config = nested[2]
	second.Config.MediaType = v1.MediaTypeImageConfig
	data, err = json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	record.Results[1].Image = oci.Descriptor(v1.MediaTypeImageManifest, data)
	if err := pushGraphBlob(t.Context(), target.Target, record.Results[1].Image, data); err != nil {
		t.Fatal(err)
	}
	record = publishGraphLookupRecord(t, target.Target, record)
	_, path, err := store.LookupGraph(t.Context(), record.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	images, err := orasoci.New(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, desc := range nested {
		if _, err := fetchVerified(t.Context(), images, desc, maxImageRecordBytes); err != nil {
			t.Fatalf("nested alias produced incomplete success: %s: %v", desc.Digest, err)
		}
	}
}

func TestGraphLookupRestoresNestedManifestAndIndexLayers(t *testing.T) {
	for _, mediaType := range []string{v1.MediaTypeImageManifest, v1.MediaTypeImageIndex} {
		t.Run(mediaType, func(t *testing.T) {
			var nested []v1.Descriptor
			createLayer := func(target oras.Target, childLayer v1.Descriptor) v1.Descriptor {
				var layer v1.Descriptor
				layer, nested = graphLookupNestedLayer(t, target, childLayer, mediaType)
				return layer
			}
			store, record, _, target := graphLookupCustomLayerFixture(t, mediaType, nil, createLayer)
			_, path, err := store.LookupGraph(t.Context(), record.Root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(path) })
			images, err := orasoci.New(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, desc := range nested {
				if _, err := fetchVerified(t.Context(), images, desc, maxImageRecordBytes); err != nil {
					t.Fatalf("nested graph child %s missing or invalid: %v", desc.Digest, err)
				}
			}
			target.mu.Lock()
			defer target.mu.Unlock()
			if target.opened != target.closed {
				t.Errorf("lookup leaked nested graph readers: opened=%d closed=%d", target.opened, target.closed)
			}
		})
	}
}

//nolint:staticcheck // Existing nondistributable cache payloads still require integrity checks.
func TestGraphLookupRetainsNondistributablePayloadValidation(t *testing.T) {
	for _, mediaType := range []string{
		v1.MediaTypeImageLayerNonDistributable,
		v1.MediaTypeImageLayerNonDistributableGzip,
		v1.MediaTypeImageLayerNonDistributableZstd,
		"application/vnd.docker.image.rootfs.foreign.diff.tar.gzip",
	} {
		for _, failure := range []string{"", "corrupt", "missing"} {
			name := failure
			if name == "" {
				name = "intact"
			}
			t.Run(mediaType+"/"+name, func(t *testing.T) {
				store, record, payloads, target := graphLookupFixture(t, mediaType, nil)
				target.alter, target.failure = payloads[0].Digest, failure
				got, path, err := store.LookupGraph(t.Context(), record.Root)
				if failure != "" {
					if err == nil || got != nil || path != "" {
						t.Fatalf("invalid nondistributable payload accepted: record=%v path=%q err=%v", got, path, err)
					}
					assertGraphLookupCleaned(t, store, target)
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(path) })
				images, err := orasoci.New(path)
				if err != nil {
					t.Fatal(err)
				}
				for _, image := range graphImages(*got) {
					if err := verifyImageGraph(t.Context(), images, image); err != nil {
						t.Fatalf("nondistributable layer missing from returned layout: %v", err)
					}
				}
			})
		}
	}
}

func TestGraphLookupCancellationClosesPayloadAndCleansStaging(t *testing.T) {
	store, record, payloads, target := graphLookupFixture(t, v1.MediaTypeImageLayer, nil)
	target.alter, target.failure, target.blocked = payloads[0].Digest, "cancel", make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	type result struct {
		record *GraphRecord
		path   string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		got, path, err := store.LookupGraph(ctx, record.Root)
		done <- result{got, path, err}
	}()
	select {
	case <-target.blocked:
	case got := <-done:
		t.Fatalf("lookup ended before payload read: %+v", got)
	case <-ctx.Done():
		t.Fatal("lookup never reached payload read")
	}
	cancel()
	drained := time.NewTimer(20 * time.Second)
	defer drained.Stop()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || got.record != nil || got.path != "" {
			t.Fatalf("canceled lookup: %+v", got)
		}
	case <-drained.C:
		t.Fatal("canceled lookup did not drain")
	}
	assertGraphLookupCleaned(t, store, target)
}
