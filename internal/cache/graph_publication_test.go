package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func graphPublicationForeignFixture(t *testing.T, mediaType string, nested bool) (GraphRecord, string, []v1.Descriptor) {
	t.Helper()
	record, layout := graphFixture(t)
	source, err := orasoci.New(layout)
	if err != nil {
		t.Fatal(err)
	}
	var payloads []v1.Descriptor
	for i := range record.Results {
		data, err := fetchVerified(t.Context(), source, record.Results[i].Image, maxImageRecordBytes)
		if err != nil {
			t.Fatal(err)
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatal(err)
		}
		manifest.Layers[0].MediaType = mediaType
		payloads = append(payloads, manifest.Layers[0])
		image := pushGraphTestJSON(t, source, v1.MediaTypeImageManifest, manifest)
		if nested {
			index := pushGraphTestJSON(t, source, v1.MediaTypeImageIndex, v1.Index{
				Versioned: specs.Versioned{SchemaVersion: 2},
				MediaType: v1.MediaTypeImageIndex,
				Manifests: []v1.Descriptor{image},
			})
			manifest.Layers = []v1.Descriptor{index}
			image = pushGraphTestJSON(t, source, v1.MediaTypeImageManifest, manifest)
		}
		record.Results[i].Image = image
	}
	if err := record.Validate(record.Root); err != nil {
		t.Fatal(err)
	}
	return record, layout, payloads
}

//nolint:staticcheck // Existing nondistributable cache layers must remain complete when republished.
func TestGraphPublicationRetainsNondistributablePayloads(t *testing.T) {
	for _, mediaType := range []string{
		v1.MediaTypeImageLayerNonDistributable,
		v1.MediaTypeImageLayerNonDistributableGzip,
		v1.MediaTypeImageLayerNonDistributableZstd,
		"application/vnd.docker.image.rootfs.foreign.diff.tar.gzip",
	} {
		for _, nested := range []bool{false, true} {
			name := mediaType
			if nested {
				name += "/nested-index"
			}
			t.Run(name, func(t *testing.T) {
				record, source, payloads := graphPublicationForeignFixture(t, mediaType, nested)
				cacheRoot := t.TempDir()
				store, err := NewLocalStore(t.Context(), cacheRoot, t.TempDir(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.PutGraph(t.Context(), record.Root, record, source); err != nil {
					t.Fatal("complete graph publication failed", err)
				}
				// Reopen the empty destination's newly written cache, then restore
				// without consulting the producer's source layout again.
				store, err = NewLocalStore(t.Context(), cacheRoot, t.TempDir(), nil)
				if err != nil {
					t.Fatal(err)
				}
				got, layout, err := store.LookupGraph(t.Context(), record.Root)
				if err != nil {
					t.Fatal("successful publication did not yield a complete restorable graph", err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(layout) })
				if !reflect.DeepEqual(*got, record) {
					t.Fatal("publication changed result alternatives or graph metadata")
				}
				images, err := orasoci.New(layout)
				if err != nil {
					t.Fatal(err)
				}
				for _, descriptor := range payloads {
					if _, err := fetchVerified(t.Context(), images, descriptor, maxImageRecordBytes); err != nil {
						t.Fatal("published graph omitted a nondistributable payload", descriptor.Digest, err)
					}
				}
			})
		}
	}
}

//nolint:staticcheck // Complete cache copies retain existing nondistributable layer types.
func TestCopyGraphImagesCompletesExistingRootsAndRetainsForeignPayloads(t *testing.T) {
	for _, nested := range []bool{false, true} {
		name := "flat"
		if nested {
			name = "nested"
		}
		t.Run(name, func(t *testing.T) {
			record, sourceLayout, payloads := graphPublicationForeignFixture(t, v1.MediaTypeImageLayerNonDistributableGzip, nested)
			source, err := orasoci.New(sourceLayout)
			if err != nil {
				t.Fatal(err)
			}
			destination, err := orasoci.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for _, image := range graphImages(record) {
				data, err := fetchVerified(t.Context(), source, image, maxImageRecordBytes)
				if err != nil {
					t.Fatal(err)
				}
				if err := pushGraphBlob(t.Context(), destination, image, data); err != nil {
					t.Fatal(err)
				}
			}
			if err := CopyGraphImages(t.Context(), source, destination, graphImages(record)); err != nil {
				t.Fatal(err)
			}
			for _, descriptor := range payloads {
				want, err := fetchVerified(t.Context(), source, descriptor, maxImageRecordBytes)
				if err != nil {
					t.Fatal(err)
				}
				got, err := fetchVerified(t.Context(), destination, descriptor, maxImageRecordBytes)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatal("existing root hid missing foreign bytes", descriptor.Digest, err)
				}
			}
		})
	}
}

func TestCopyGraphImagesRejectsCorruptExistingMetadata(t *testing.T) {
	record, sourceLayout := graphFixture(t)
	source, err := orasoci.New(sourceLayout)
	if err != nil {
		t.Fatal(err)
	}
	destinationLayout := t.TempDir()
	destination, err := orasoci.New(destinationLayout)
	if err != nil {
		t.Fatal(err)
	}
	root := record.Results[0].Image
	data, err := fetchVerified(t.Context(), source, root, maxImageRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := pushGraphBlob(t.Context(), destination, root, data); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(destinationLayout, "blobs", "sha256", root.Digest.Encoded())
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(root.Size)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CopyGraphImages(t.Context(), source, destination, graphImages(record)); err == nil {
		t.Fatal("copy accepted corrupt retained metadata")
	}
}

func TestGraphPublicationRejectsCorruptLocalPayloadBeforeRetagging(t *testing.T) {
	record, source := graphFixture(t)
	cacheRoot := t.TempDir()
	store, err := NewLocalStore(t.Context(), cacheRoot, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := store.PutGraph(t.Context(), record.Root, record, source)
	if err != nil {
		t.Fatal(err)
	}
	image := record.Results[0].Image
	imageData, err := os.ReadFile(filepath.Join(source, "blobs", image.Digest.Algorithm().String(), image.Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(imageData, &manifest); err != nil {
		t.Fatal(err)
	}
	layer := manifest.Layers[0]
	path := filepath.Join(cacheRoot, "blobs", layer.Digest.Algorithm().String(), layer.Digest.Encoded())
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Repeat([]byte("x"), int(layer.Size))
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	record.CreatedAt = record.CreatedAt.Add(time.Hour)
	if _, err := store.PutGraph(t.Context(), record.Root, record, source); err == nil {
		t.Fatal("publication accepted an existing corrupt local payload")
	}
	reopened, err := NewLocalStore(t.Context(), cacheRoot, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	current, err := reopened.target.Resolve(t.Context(), graphTag(record.Root))
	if err != nil || current.Digest != previous.Digest {
		t.Fatalf("failed publication changed the previous graph tag: %v, %v", current, err)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, corrupt) {
		t.Fatalf("failed publication changed retained content: %q, %v", data, err)
	}
}

func TestGraphExistingPayloadVerificationHonorsCancellation(t *testing.T) {
	layout, err := orasoci.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("existing payload")
	descriptor := oci.Descriptor(v1.MediaTypeImageLayer, data)
	if err := layout.Push(t.Context(), descriptor, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	target := &cancelGraphPayloadTarget{Store: layout, cancel: cancel}
	exists, err := (graphCopyDestination{Storage: target, verifyExistingPayloads: true}).Exists(ctx, descriptor)
	if !errors.Is(err, context.Canceled) || exists || !target.closed {
		t.Fatalf("existing payload verification ignored cancellation or leaked its stream: exists=%v, closed=%v, err=%v", exists, target.closed, err)
	}
}

func TestGraphPublicationRejectsCorruptGeneratedMetadataBeforeRetagging(t *testing.T) {
	for _, kind := range []string{"sidecar", "config", "metadata manifest", "artifact"} {
		t.Run(kind, func(t *testing.T) {
			record, source := graphFixture(t)
			cacheRoot := t.TempDir()
			store, err := NewLocalStore(t.Context(), cacheRoot, t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			previous, err := store.PutGraph(t.Context(), record.Root, record, source)
			if err != nil {
				t.Fatal(err)
			}
			record.CreatedAt = record.CreatedAt.Add(time.Hour)
			candidate, err := NewLocalStore(t.Context(), t.TempDir(), t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			artifact, err := candidate.PutGraph(t.Context(), record.Root, record, source)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err = NewLocalStore(t.Context(), candidate.localRoot, t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			index, manifest := graphTestMetadata(t, candidate.target, artifact)
			descriptor := artifact
			switch kind {
			case "metadata manifest":
				descriptor = index.Manifests[0]
			case "config":
				descriptor = manifest.Config
			case "sidecar":
				descriptor, err = record.Results[0].Descriptor()
				if err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(cacheRoot, "blobs", descriptor.Digest.Algorithm().String(), descriptor.Digest.Encoded())
			if _, err := os.Stat(path); err == nil {
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
			}
			corrupt := bytes.Repeat([]byte("x"), int(descriptor.Size))
			if err := os.WriteFile(path, corrupt, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.PutGraph(t.Context(), record.Root, record, source); err == nil {
				t.Fatal("publication accepted corrupt existing generated metadata")
			}
			reopened, err := NewLocalStore(t.Context(), cacheRoot, t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			current, err := reopened.target.Resolve(t.Context(), graphTag(record.Root))
			if err != nil || current.Digest != previous.Digest {
				t.Fatalf("failed publication changed the graph tag: %v, %v", current, err)
			}
			if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, corrupt) {
				t.Fatalf("failed publication mutated retained metadata: %q, %v", data, err)
			}
		})
	}
}

type cancelGraphPayloadTarget struct {
	*orasoci.Store
	cancel context.CancelFunc
	closed bool
}

func (target *cancelGraphPayloadTarget) Fetch(ctx context.Context, descriptor v1.Descriptor) (io.ReadCloser, error) {
	stream, err := target.Store.Fetch(ctx, descriptor)
	if err != nil {
		return nil, err
	}
	return &cancelGraphPayloadStream{ReadCloser: stream, target: target}, nil
}

type cancelGraphPayloadStream struct {
	io.ReadCloser
	target *cancelGraphPayloadTarget
}

func (stream *cancelGraphPayloadStream) Read(buffer []byte) (int, error) {
	n, err := stream.ReadCloser.Read(buffer)
	stream.target.cancel()
	return n, err
}

func (stream *cancelGraphPayloadStream) Close() error {
	stream.target.closed = true
	return stream.ReadCloser.Close()
}

func TestCopyGraphImagesReportsSourceCloseErrors(t *testing.T) {
	for _, where := range []string{"metadata", "payload", "copy-only metadata", "copy-only payload"} {
		t.Run(where, func(t *testing.T) {
			_, record, payloads, source := graphLookupFixture(t, v1.MediaTypeImageLayer, nil)
			source.alter, source.failure = payloads[0].Digest, "close"
			if where == "metadata" || where == "copy-only metadata" {
				source.alter = record.Results[0].Image.Digest
			}
			destination, err := orasoci.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			copyImages := CopyGraphImages
			if where == "copy-only metadata" || where == "copy-only payload" {
				copyImages = CopyVerifiedGraphImages
			}
			if err := copyImages(t.Context(), source, destination, graphImages(record)); !errors.Is(err, errGraphLookupClose) {
				t.Fatal("copy discarded source close error", err)
			}
			if source.opened != source.closed {
				t.Fatal("copy leaked source streams", source.opened, source.closed)
			}
		})
	}
}

func TestCopyGraphImagesPreservesNestedIndexPlatformMetadata(t *testing.T) {
	var children []v1.Descriptor
	createLayer := func(target oras.Target, layer v1.Descriptor) v1.Descriptor {
		index, _ := graphLookupNestedLayer(t, target, layer, v1.MediaTypeImageIndex)
		data, err := fetchVerified(t.Context(), target, index, maxImageRecordBytes)
		if err != nil {
			t.Fatal(err)
		}
		var document v1.Index
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		document.Manifests[0].Platform = &v1.Platform{OS: "linux", Architecture: "amd64"}
		document.Manifests[0].Annotations = map[string]string{"example.test/name": "retained"}
		children = document.Manifests
		data, err = json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		index = oci.Descriptor(v1.MediaTypeImageIndex, data)
		if err := pushGraphBlob(t.Context(), target, index, data); err != nil {
			t.Fatal(err)
		}
		return index
	}
	_, record, _, source := graphLookupCustomLayerFixture(t, v1.MediaTypeImageIndex, nil, createLayer)
	destination, err := orasoci.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := CopyGraphImages(t.Context(), source, destination, graphImages(record)); err != nil {
		t.Fatal("valid nested index metadata rejected", err)
	}
	for _, child := range children {
		if err := verifyGraphBlob(t.Context(), destination, child); err != nil {
			t.Fatal("nested child was not copied", err)
		}
	}
	for _, image := range graphImages(record) {
		data, err := fetchVerified(t.Context(), destination, image, maxImageRecordBytes)
		if err != nil {
			t.Fatal(err)
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatal(err)
		}
		data, err = fetchVerified(t.Context(), destination, manifest.Layers[0], maxImageRecordBytes)
		if err != nil {
			t.Fatal(err)
		}
		var index v1.Index
		if err := json.Unmarshal(data, &index); err != nil || !reflect.DeepEqual(index.Manifests, children) {
			t.Fatal("recursive copy changed child platform/annotations", index.Manifests, err)
		}
	}
}

func TestCopyGraphImagesRetainsNestedPayloadAliases(t *testing.T) {
	var nested []v1.Descriptor
	createLayer := func(target oras.Target, layer v1.Descriptor) v1.Descriptor {
		var descriptor v1.Descriptor
		descriptor, nested = graphLookupNestedLayer(t, target, layer, v1.MediaTypeImageIndex)
		return descriptor
	}
	_, record, _, source := graphLookupCustomLayerFixture(t, v1.MediaTypeImageIndex, nil, createLayer)
	data, err := fetchVerified(t.Context(), source.Target, record.Results[1].Image, maxImageRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Config = nested[2]
	manifest.Config.MediaType = v1.MediaTypeImageConfig
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	record.Results[1].Image = oci.Descriptor(v1.MediaTypeImageManifest, data)
	if err := pushGraphBlob(t.Context(), source.Target, record.Results[1].Image, data); err != nil {
		t.Fatal(err)
	}
	destination, err := orasoci.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Visit the opaque config alias before the image/index that gives its bytes
	// structural meaning. Existing metadata must still traverse its descendants.
	if err := CopyGraphImages(t.Context(), source.Target, destination, []v1.Descriptor{record.Results[1].Image, record.Results[0].Image}); err != nil {
		t.Fatal(err)
	}
	for _, descriptor := range nested {
		if _, err := fetchVerified(t.Context(), destination, descriptor, maxImageRecordBytes); err != nil {
			t.Fatal("metadata alias hid a nested descendant", descriptor.Digest, err)
		}
	}
}

func TestCopyGraphImagesCancelsAndDrainsSource(t *testing.T) {
	_, record, payloads, source := graphLookupFixture(t, v1.MediaTypeImageLayer, nil)
	source.alter, source.failure, source.blocked = payloads[0].Digest, "cancel", make(chan struct{})
	destination, err := orasoci.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- CopyGraphImages(ctx, source, destination, graphImages(record)) }()
	select {
	case <-source.blocked:
	case err := <-done:
		t.Fatal("copy ended before reaching the blocking source", err)
	case <-ctx.Done():
		t.Fatal("copy did not reach the blocking source", ctx.Err())
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("copy lost cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled copy did not drain")
	}
	if source.opened != source.closed {
		t.Fatal("canceled copy leaked source streams", source.opened, source.closed)
	}
}
