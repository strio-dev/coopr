package buildah

import (
	"context"
	"errors"
	"strings"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	"go.podman.io/storage"
)

type missingNativeSourceStore struct{ storage.Store }

func (missingNativeSourceStore) Image(string) (*storage.Image, error) {
	return nil, storage.ErrImageUnknown
}

func TestMaterializeMissingImageIDDoesNotRestoreFromRegistry(t *testing.T) {
	resolver, err := oci.NewResolver(oci.Options{})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 64)
	for _, reference := range []string{id, "sha256:" + id} {
		_, err := materializeImageSource(context.Background(), resolver, reference, v1.Platform{OS: "linux", Architecture: "amd64"}, missingNativeSourceStore{}, nil, ResolvedImageSource{ImageID: id, Reference: reference})
		if !errors.Is(err, storage.ErrImageUnknown) || !strings.Contains(err.Error(), "selected local image") {
			t.Fatalf("%s: %v", reference, err)
		}
	}
}
