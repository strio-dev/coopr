// Package nativeimage seeds private native-store test fixtures.
package nativeimage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/imagestore"
	ocilayout "go.podman.io/image/v5/oci/layout"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// Store seeds native test fixtures through the production copy and index writers.
type Store struct {
	*imagestore.Store
	Backend storage.Store
	System  *types.SystemContext
}

func BorrowStore(backend storage.Store) *Store {
	return &Store{Store: imagestore.FromStore(backend), Backend: backend}
}

func NewStore(options storage.StoreOptions) (*Store, error) {
	backend, err := storage.GetStore(options)
	if err != nil {
		return nil, err
	}
	return BorrowStore(backend), nil
}

// Close releases this fixture's handle without unmounting another owner's layers.
func (s *Store) Close() error {
	if s == nil || s.Backend == nil {
		return nil
	}
	_, err := s.Backend.Shutdown(false)
	if errors.Is(err, storage.ErrLayerUsedByContainer) {
		s.Backend, s.Store = nil, nil
		return nil
	}
	if err != nil {
		return fmt.Errorf("close containers-storage: %w", err)
	}
	s.Backend.Free()
	s.Backend, s.Store = nil, nil
	return nil
}

func (s *Store) SeedLayout(ctx context.Context, layout string, root v1.Descriptor, tag string) (string, error) {
	// A disposable root view selects even unanchored children and shared-config
	// manifest variants. Blob bytes stay in the original private test fixture.
	view, err := os.MkdirTemp("", "coopr-fixture-root-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(view) }()
	layout, err = filepath.Abs(layout)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []v1.Descriptor{root}})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(view, "index.json"), data, 0600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(view, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0600); err != nil {
		return "", err
	}
	system := types.SystemContext{BigFilesTemporaryDir: os.TempDir()}
	if s.System != nil {
		system = *s.System
	}
	system.OCISharedBlobDirPath = filepath.Join(layout, "blobs")
	source, err := ocilayout.NewReference(view, "")
	if err != nil {
		return "", err
	}
	image, err := source.NewImage(ctx, &system)
	if err != nil {
		return "", err
	}
	imageID := image.ConfigInfo().Digest
	if err := image.Close(); err != nil {
		return "", err
	}
	destination, err := imagestorage.Transport.NewStoreReference(s.Backend, nil, imageID.Encoded())
	if err != nil {
		return "", err
	}
	if err := imagestore.Copy(ctx, &system, source, destination, true); err != nil {
		return "", err
	}
	if tag == "" {
		return imageID.String(), nil
	}
	return s.Tag(imageID.Encoded(), tag)
}

func (s *Store) SeedIndex(ctx context.Context, layout string, root v1.Descriptor, tag string) (string, error) {
	source, err := orasoci.NewStorage(layout)
	if err != nil {
		return "", err
	}
	data, err := content.FetchAll(ctx, source, root)
	if err != nil {
		return "", err
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return "", err
	}
	imageIDs := make(map[digest.Digest]string, len(index.Manifests))
	for _, child := range index.Manifests {
		id, err := s.SeedLayout(ctx, layout, child, "")
		if err != nil {
			return "", err
		}
		imageIDs[child.Digest] = digest.Digest(id).Encoded()
	}
	return s.WriteStoredIndex(ctx, root, data, imageIDs, tag)
}
