package oci

import (
	"errors"
	"strings"
	"testing"

	"go.podman.io/storage"
)

type fullIDStore struct {
	storage.Store
	image   *storage.Image
	queried string
}

func (s *fullIDStore) Image(name string) (*storage.Image, error) {
	s.queried = name
	if s.image != nil {
		return s.image, nil
	}
	return nil, storage.ErrImageUnknown
}
func TestStoredFullImageIDBypassesNameExpansion(t *testing.T) {
	id := strings.Repeat("a", 64)
	backend := &fullIDStore{image: &storage.Image{ID: id}}
	image, matched, err := findStoredImageAndName(backend, id)
	if err != nil || image != backend.image || matched != id || backend.queried != id {
		t.Fatalf("lookup = %v, %s, %v; queried %s", image, matched, err, backend.queried)
	}
	backend.image = nil
	if _, _, err := findStoredImageAndName(backend, id); !errors.Is(err, storage.ErrImageUnknown) {
		t.Fatalf("missing full ID: %v", err)
	}
}
