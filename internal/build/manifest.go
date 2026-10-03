package build

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"coopr/internal/buildah"
	"coopr/internal/imagecatalog"
	"coopr/internal/oci"
	"github.com/containerd/platforms"
	"github.com/gofrs/flock"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// appendManifest serializes updates to one local name so simultaneous builds
// cannot discard each other's platforms. Filesystem bytes stay in the canonical
// image graph; old children are exported only to assemble the requested output.
func appendManifest(ctx context.Context, storeDir, name, output, format string, store buildah.StoreOptions, variants []oci.ImageVariant, selections map[string]imagecatalog.Selection) (_ v1.Descriptor, _ []byte, _ map[string]imagecatalog.Selection, retErr error) {
	if err := os.MkdirAll(storeDir, 0755); err != nil {
		return v1.Descriptor{}, nil, nil, err
	}
	lock := flock.New(filepath.Join(storeDir, ".manifest-"+digest.FromString(name).Encoded()+".lock"))
	locked, err := lock.TryLockContext(ctx, 20*time.Millisecond)
	if err != nil || !locked {
		if err == nil {
			err = fmt.Errorf("manifest lock was not acquired")
		}
		return v1.Descriptor{}, nil, nil, err
	}
	defer func() {
		if err := lock.Unlock(); retErr == nil {
			retErr = err
		}
	}()
	_, _, previous, found, err := imagecatalog.LookupIndex(ctx, storeDir, name)
	if err != nil {
		return v1.Descriptor{}, nil, nil, err
	}
	if !found {
		if _, _, exists, err := imagecatalog.LookupSole(ctx, storeDir, name); err != nil {
			return v1.Descriptor{}, nil, nil, err
		} else if exists {
			return v1.Descriptor{}, nil, nil, fmt.Errorf("manifest name %s already identifies a single image", name)
		}
	}
	keys := make([]string, 0, len(previous))
	for key := range previous {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if _, replaced := selections[key]; replaced {
			continue
		}
		selection := previous[key]
		platform, err := platforms.Parse(key)
		if err != nil {
			return v1.Descriptor{}, nil, nil, err
		}
		layout := filepath.Join(filepath.Dir(output), "previous-"+selection.Manifest.Digest.Encoded())
		exported, err := buildah.ExportStoredImageSelectedSupervised(ctx, store, selection.ImageID, selection.Manifest.Digest, layout)
		if err != nil {
			return v1.Descriptor{}, nil, nil, fmt.Errorf("export previous manifest platform %s: %w", key, err)
		}
		root, err := oci.LayoutRoot(exported.Layout)
		if err != nil {
			return v1.Descriptor{}, nil, nil, err
		}
		if root.Digest != selection.Manifest.Digest || root.MediaType != selection.Manifest.MediaType || root.Size != selection.Manifest.Size {
			return v1.Descriptor{}, nil, nil, fmt.Errorf("previous manifest platform %s changed during export", key)
		}
		variants = append(variants, oci.ImageVariant{Layout: layout, Manifest: root, Platform: platform})
		selections[key] = selection
	}
	root, data, err := oci.AssembleImageIndex(ctx, output, variants, format)
	if err != nil {
		return v1.Descriptor{}, nil, nil, err
	}
	for key, selection := range selections {
		selection.Root = root
		selections[key] = selection
	}
	if err := imagecatalog.CommitIndex(ctx, storeDir, name, root, data, selections); err != nil {
		return v1.Descriptor{}, nil, nil, err
	}
	return root, data, selections, nil
}
