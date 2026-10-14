package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// These blobs exercise native signature framing and storage, not cryptography.
func storedSignatureFixture(t *testing.T) (storage.Store, string, digest.Digest, digest.Digest) {
	t.Helper()
	root := t.TempDir()
	store, err := storage.GetStore(storage.StoreOptions{GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"), GraphDriverName: "vfs"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.Shutdown(false); err != nil {
			t.Error(err)
		}
		store.Free()
	})
	image, err := store.CreateImage(digest.FromString(t.Name()).Encoded(), nil, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	selected, sibling := digest.FromString("selected"), digest.FromString("sibling")
	signature := func(label string) []byte { return append([]byte{0xc2}, []byte(label)...) }
	blobs := map[string][]byte{"signatures": signature("default"), "signature-" + selected.Encoded(): signature("selected"), "signature-" + sibling.Encoded(): signature("sibling")}
	for key, blob := range blobs {
		if err := store.SetImageBigData(image.ID, key, blob, nil); err != nil {
			t.Fatal(err)
		}
	}
	metadata, err := json.Marshal(map[string]any{"signature-sizes": []int{len(blobs["signatures"])}, "signatures-sizes": map[digest.Digest][]int{selected: {len(blobs["signature-"+selected.Encoded()])}, sibling: {len(blobs["signature-"+sibling.Encoded()])}}, "unknown": map[string]any{"preserve": true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(image.ID, string(metadata)); err != nil {
		t.Fatal(err)
	}
	return store, image.ID, selected, sibling
}

func assertNativeStoredSignatures(t *testing.T, store storage.Store, imageID string, selected *digest.Digest, want [][]byte) {
	t.Helper()
	reference, err := imagestorage.Transport.NewStoreReference(store, nil, imageID)
	if err != nil {
		t.Fatal(err)
	}
	source, err := reference.NewImageSource(context.Background(), &types.SystemContext{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	}()
	// The public compatibility method invokes native GetSignaturesWithFormat.
	got, err := source.GetSignatures(context.Background(), selected)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("native signatures = %q, want %q", got, want)
	}
}

func TestWriteStoredManifestSignaturesPreservesOtherMetadata(t *testing.T) {
	for _, isDefault := range []bool{false, true} {
		t.Run(map[bool]string{false: "member", true: "default"}[isDefault], func(t *testing.T) {
			store, id, selected, sibling := storedSignatureFixture(t)
			before, err := CaptureStoredSignatures(store, id)
			if err != nil {
				t.Fatal(err)
			}
			replacement := [][]byte{{0xc2, 'n'}, {0xc2, 'm'}}
			capturedSizes := slices.Clone(before.parsed.SignaturesSizes[selected])
			if err := WriteStoredManifestSignatures(store, id, selected, isDefault, replacement, before); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.parsed.SignaturesSizes[selected], capturedSizes) {
				t.Fatal("write mutated captured rollback sizes")
			}
			after, err := CaptureStoredSignatures(store, id)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after.metadataRaw["unknown"], before.metadataRaw["unknown"]) {
				t.Fatal("unknown metadata changed")
			}
			if !bytes.Equal(after.blobs[sibling], before.blobs[sibling]) || !reflect.DeepEqual(after.parsed.SignaturesSizes[sibling], before.parsed.SignaturesSizes[sibling]) {
				t.Fatal("sibling signatures changed")
			}
			assertNativeStoredSignatures(t, store, id, &selected, replacement)
			assertNativeStoredSignatures(t, store, id, &sibling, [][]byte{before.blobs[sibling]})
			wantDefault := [][]byte{before.defaultBlob}
			if isDefault {
				wantDefault = replacement
			} else if !bytes.Equal(after.defaultBlob, before.defaultBlob) || !reflect.DeepEqual(after.parsed.SignatureSizes, before.parsed.SignatureSizes) {
				t.Fatal("default signatures changed")
			}
			assertNativeStoredSignatures(t, store, id, nil, wantDefault)
		})
	}
}

func TestRestoreStoredSignaturesAfterPartialMutation(t *testing.T) {
	for _, replacement := range [][][]byte{{{0xc2, 'n'}}, nil} {
		name := "replace"
		if replacement == nil {
			name = "clear"
		}
		t.Run(name, func(t *testing.T) {
			store, id, selected, sibling := storedSignatureFixture(t)
			before, err := CaptureStoredSignatures(store, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteStoredManifestSignatures(store, id, selected, true, replacement, before); err != nil {
				t.Fatal(err)
			}
			// Simulate a later failed operation after signature and metadata mutation.
			if err := store.SetImageBigData(id, "signature-"+sibling.Encoded(), []byte("partial mutation"), nil); err != nil {
				t.Fatal(err)
			}
			if err := store.SetMetadata(id, `{"partial":true}`); err != nil {
				t.Fatal(err)
			}
			if err := RestoreStoredSignatures(store, id, before); err != nil {
				t.Fatal(err)
			}
			after, err := CaptureStoredSignatures(store, id)
			if err != nil {
				t.Fatal(err)
			}
			if after.metadata != before.metadata || !bytes.Equal(after.defaultBlob, before.defaultBlob) || !reflect.DeepEqual(after.blobs, before.blobs) {
				t.Fatal("rollback did not restore captured metadata and signature blobs")
			}
			assertNativeStoredSignatures(t, store, id, nil, [][]byte{before.defaultBlob})
			for _, item := range []digest.Digest{selected, sibling} {
				assertNativeStoredSignatures(t, store, id, &item, [][]byte{before.blobs[item]})
			}
		})
	}
}

func TestStoredSignatureBlobsRejectsInvalidSizeVector(t *testing.T) {
	for _, test := range []struct {
		name  string
		sizes []int
		blob  []byte
	}{
		{"negative", []int{-1}, []byte("a")}, {"oversized", []int{2}, []byte("a")}, {"trailing", []int{1}, []byte("ab")},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := storedSignatureState{parsed: storedSignatureMetadata{SignatureSizes: test.sizes}, defaultBlob: test.blob}
			if _, err := StoredSignatureBlobs(state, digest.FromString("default"), true); err == nil || !strings.Contains(err.Error(), "stored-image signatures") {
				t.Fatalf("invalid size vector accepted: %v", err)
			}
		})
	}
}
