package buildah

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	"go.podman.io/storage"
)

// Embedding the interface makes any unexpected native-store operation fail:
// malformed metadata must be rejected before native storage is consulted.
type digestValidationStore struct{ storage.Store }

func TestMalformedDigestValidationBoundaries(t *testing.T) {
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	for _, value := range []string{"", "sha256", "malformed", "sha256:", "sha256:bad", "sha256:" + strings.Repeat("g", 64)} {
		t.Run(value, func(t *testing.T) {
			bad := digest.Digest(value)
			for _, boundary := range []struct {
				name string
				want string
				run  func() error
			}{
				{"cache mount scope", "invalid cache mount source digest", func() error {
					_, err := scopedCacheMountIDResolver(CacheMountScope{Kind: "component", Source: bad, Stage: "0", Platform: platform})
					return err
				}},
				{"selected artifact", "invalid immutable manifest", func() error {
					descriptor := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: bad}
					return validateSelectedInput(ResolvedImageSource{Artifact: true, Reference: "artifact", Root: descriptor, Selected: descriptor}, true)
				}},
				{"component identity", "identity does not match", func() error {
					importer := &ComponentPackageImporter{}
					_, err := importer.Import(context.Background(), &ResolvedComponentPlan{
						Identity: bad, Artifact: &oci.Resolved{Selected: v1.Descriptor{Digest: bad}},
					}, "0", platform)
					return err
				}},
				{"package snapshot", "invalid snapshot descriptor", func() error {
					_, _, err := ImportPackageSnapshot(context.Background(), digestValidationStore{}, nil, oci.Package{
						Stage: "0", Descriptor: v1.Descriptor{MediaType: oci.ComponentPackageType, Digest: bad},
					}, "unused.tar", platform)
					return err
				}},
				{"retained manifest", "invalid committed image manifest digest", func() error {
					return retainStoredLayoutLayers(nil, "unused", t.TempDir(), bad)
				}},
				{"retained layer", "invalid committed layer digest", func() error {
					layout, selected := digestValidationLayout(t, v1.Manifest{Layers: []v1.Descriptor{{Digest: bad}}})
					return retainStoredLayoutLayers(nil, "unused", layout, selected.Digest)
				}},
				{"imported image config", "image config has invalid digest", func() error {
					layout, selected := digestValidationLayout(t, v1.Manifest{
						Versioned: specs.Versioned{SchemaVersion: 2},
						Config:    v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: bad},
					})
					_, err := ImportSelectedImage(context.Background(), digestValidationStore{}, nil, layout, selected)
					return err
				}},
			} {
				t.Run(boundary.name, func(t *testing.T) {
					if err := boundary.run(); err == nil || !strings.Contains(err.Error(), boundary.want) {
						t.Fatalf("error = %v, want %q", err, boundary.want)
					}
				})
			}
		})
	}
}

func digestValidationLayout(t *testing.T, manifest v1.Manifest) (string, v1.Descriptor) {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	selected := descriptor(v1.MediaTypeImageManifest, data)
	layout := t.TempDir()
	path := filepath.Join(layout, "blobs", "sha256", selected.Digest.Encoded())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return layout, selected
}
