package oci

import (
	"bytes"
	"context"
	"io"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
)

type metadataBytesStorage struct {
	content.ReadOnlyStorage
	data []byte
}

func (s metadataBytesStorage) Fetch(context.Context, v1.Descriptor) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}
func TestReadLayerMetadataVerifiesDescriptor(t *testing.T) {
	data := []byte(`{"config":{}}`)
	desc := Descriptor(v1.MediaTypeImageConfig, data)
	for _, tc := range []struct {
		name   string
		data   []byte
		failed bool
	}{
		{"valid", data, false},
		{"digest", []byte(`{"config":[]}`), true},
		{"short", data[:len(data)-1], true},
		{"trailing", append(bytes.Clone(data), ' '), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readLayerMetadata(context.Background(), metadataBytesStorage{data: tc.data}, desc)
			if (err != nil) != tc.failed {
				t.Fatalf("verification = %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readLayerMetadata(ctx, metadataBytesStorage{data: data}, desc); err == nil {
		t.Fatal("canceled metadata read accepted")
	}
}
