package buildah

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"coopr/internal/localstore"
	"coopr/internal/oci"
	"github.com/gofrs/flock"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/pkg/libartifact"
	artifacttypes "go.podman.io/common/pkg/libartifact/types"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/types"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

// Only source indexes live here. Selected manifests and payloads remain in the
// native artifact store; retaining the index keeps offline platform selection.
func artifactIndexDirectory(graphRoot string) string {
	return filepath.Join(graphRoot, "coopr-artifact-indexes")
}

// Cached admission and refresh update two independent stores. Coordinate those
// transitions so a cached flat lookup cannot erase an indexed refresh's alias.
func withArtifactInputLock(ctx context.Context, graphRoot string, operation func() error) error {
	directory := artifactIndexDirectory(graphRoot)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(directory, "input.lock"), flock.SetFlag(os.O_CREATE|os.O_RDWR), flock.SetPermissions(0o600))
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return errors.Join(fmt.Errorf("lock artifact input: %w", err), lock.Close())
	}
	if !locked {
		return errors.Join(errors.New("artifact input lock not acquired"), ctx.Err(), lock.Close())
	}
	defer func() { _ = lock.Close() }()
	return operation()
}

func artifactChildReference(reference string, selected digest.Digest) (string, error) {
	ref, err := oci.ParseReference(reference)
	if err != nil {
		return "", err
	}
	ref.Reference = selected.String()
	return ref.String(), nil
}

func inspectArtifactIndexSource(ctx context.Context, artifacts *libartifact.ArtifactStore, graphRoot, reference string, platform v1.Platform) (ResolvedImageSource, error) {
	directory := artifactIndexDirectory(graphRoot)
	if _, err := os.Stat(filepath.Join(directory, v1.ImageIndexFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ResolvedImageSource{}, artifacttypes.ErrArtifactNotExist
		}
		return ResolvedImageSource{}, err
	}
	var selected ResolvedImageSource
	err := localstore.WithReadLock(ctx, directory, func() error {
		metadata, err := orasoci.NewFromFS(ctx, os.DirFS(directory))
		if err != nil {
			return err
		}
		root, err := metadata.Resolve(ctx, reference)
		if errors.Is(err, errdef.ErrNotFound) {
			return artifacttypes.ErrArtifactNotExist
		}
		if err != nil {
			return err
		}
		data, err := content.FetchAll(ctx, metadata, root)
		if err != nil {
			return err
		}
		list, err := manifest.ListFromBlob(data, root.MediaType)
		if err != nil {
			return fmt.Errorf("parse cached artifact index: %w", err)
		}
		system := &types.SystemContext{}
		if artifacts.SystemContext != nil {
			*system = *artifacts.SystemContext
		}
		system.OSChoice, system.ArchitectureChoice, system.VariantChoice = platform.OS, platform.Architecture, platform.Variant
		child, err := list.ChooseInstance(system)
		if err != nil {
			return errors.Join(artifacttypes.ErrArtifactNotExist, fmt.Errorf("select cached artifact platform: %w", err))
		}
		childReference, err := artifactChildReference(reference, child)
		if err != nil {
			return err
		}
		selected, err = inspectArtifactFileSource(ctx, artifacts, graphRoot, childReference)
		return err
	})
	return selected, err
}

func retainArtifactIndex(ctx context.Context, graphRoot string, resolved *oci.Resolved) error {
	data, err := content.FetchAll(ctx, resolved.Source(), resolved.Root)
	if err != nil {
		return err
	}
	rootReference, err := artifactChildReference(resolved.Reference, resolved.Root.Digest)
	if err != nil {
		return err
	}
	return localstore.Update(ctx, artifactIndexDirectory(graphRoot), func(metadata *orasoci.Store) error {
		if err := metadata.Push(ctx, resolved.Root, bytes.NewReader(data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
			return err
		}
		for _, reference := range []string{resolved.Reference, rootReference} {
			if err := metadata.Tag(ctx, resolved.Root, reference); err != nil {
				return err
			}
		}
		return nil
	})
}

func forgetArtifactIndexReference(ctx context.Context, graphRoot, reference string) error {
	directory := artifactIndexDirectory(graphRoot)
	if _, err := os.Stat(filepath.Join(directory, v1.ImageIndexFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	ref, err := oci.ParseReference(reference)
	if err != nil {
		return err
	}
	if digest.Digest(ref.Reference).Validate() == nil {
		return nil
	}
	_, err = localstore.RemoveTag(ctx, directory, ref.String())
	return err
}
