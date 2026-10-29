package buildah

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah/pkg/cli"
	"go.podman.io/common/libimage"
	"go.podman.io/common/pkg/libartifact"
	artifacttypes "go.podman.io/common/pkg/libartifact/types"
	dockerreference "go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/image"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/pkg/shortnames"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"oras.land/oras-go/v2/content"
)

// resolveFileSource keeps native image precedence and admits non-image OCI
// artifacts only for file inputs. Artifacts live in Podman's artifact store;
// they never become tagged, runnable images in containers/storage.
func resolveFileSource(ctx context.Context, resolver *oci.Resolver, reference string, platform v1.Platform, store storage.Store, system *types.SystemContext) (ResolvedImageSource, error) {
	return resolveFileSourceSelection(ctx, resolver, reference, platform, store, system, true)
}

// selectFileSource defers image layer imports until publication needs to
// execute. Artifact inputs retain their native artifact-store resolution.
func selectFileSource(ctx context.Context, resolver *oci.Resolver, reference string, platform v1.Platform, store storage.Store, system *types.SystemContext) (ResolvedImageSource, error) {
	return resolveFileSourceSelection(ctx, resolver, reference, platform, store, system, false)
}

func resolveFileSourceSelection(ctx context.Context, resolver *oci.Resolver, reference string, platform v1.Platform, store storage.Store, system *types.SystemContext, materialize bool) (ResolvedImageSource, error) {
	if ctx == nil || resolver == nil || store == nil {
		return ResolvedImageSource{}, errors.New("file input requires a context, resolver, and native store")
	}
	if err := ctx.Err(); err != nil {
		return ResolvedImageSource{}, err
	}
	if strings.HasPrefix(reference, "sha256:") || dockerreference.IsFullIdentifier(reference) {
		if !materialize {
			return selectImageSource(ctx, resolver, reference, platform, store)
		}
		return ResolveImageSource(ctx, resolver, reference, platform, store, system)
	}
	canonical, err := oci.ParseReference(reference)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	configured := resolver.SystemContext()
	if system != nil {
		if system.SignaturePolicyPath != "" {
			configured.SignaturePolicyPath = system.SignaturePolicyPath
		}
		configured.BigFilesTemporaryDir = system.BigFilesTemporaryDir
	}
	configured.OSChoice, configured.ArchitectureChoice, configured.VariantChoice = platform.OS, platform.Architecture, platform.Variant

	selectImage := func(selected ResolvedImageSource) (ResolvedImageSource, error) {
		if !materialize {
			return selected, nil
		}
		return materializeImageSource(ctx, resolver, reference, platform, store, configured, selected)
	}

	var cached *ResolvedImageSource
	if resolver.PullPolicy() != oci.PullAlways {
		stored, err := oci.ResolveStoredImage(ctx, store, reference, platform)
		if err == nil {
			selected := ResolvedImageSource{ImageID: stored.StorageImageID, Root: stored.Root, Selected: stored.Selected, SourceManifest: stored.SourceManifest, ConfigData: stored.ConfigData, Reference: canonical.String()}
			if resolver.PullPolicy() != oci.PullNewer {
				return selectImage(selected)
			}
			cached = &selected
		} else if resolver.PullPolicy() == oci.PullNever && errors.Is(err, oci.ErrStoredPlatformUnavailable) {
			return ResolvedImageSource{}, err
		} else if !errors.Is(err, storage.ErrImageUnknown) && !errors.Is(err, oci.ErrStoredPlatformUnavailable) {
			return ResolvedImageSource{}, err
		}
		if cached == nil {
			selected, err := resolveStoredArtifactFileSource(ctx, reference, platform, store, configured)
			if err == nil {
				if resolver.PullPolicy() != oci.PullNewer {
					return selected, nil
				}
				cached = &selected
			} else if !errors.Is(err, artifacttypes.ErrArtifactNotExist) {
				return ResolvedImageSource{}, err
			}
		}
	}
	if resolver.PullPolicy() == oci.PullNever {
		return ResolvedImageSource{}, fmt.Errorf("file input %q is not available locally and pull policy is never", reference)
	}

	resolved, err := resolver.ResolveRemoteFileInput(ctx, reference, platform)
	if err == nil && resolved.Kind == oci.Image {
		if cached != nil && !cached.Artifact {
			previous := cached.Selected
			if cached.SourceManifest != nil {
				previous = *cached.SourceManifest
			}
			if previous.Digest == resolved.Selected.Digest {
				return selectImage(*cached)
			}
		}
		selected := ResolvedImageSource{
			ImageID: resolved.Config.Digest.Encoded(), Root: resolved.Root, Selected: resolved.Selected,
			ConfigData: resolved.ConfigData, Reference: resolved.Reference, Remote: true,
		}
		var materialized ResolvedImageSource
		materialized, err = selectImage(selected)
		if err == nil {
			if err := withArtifactInputLock(ctx, store.GraphRoot(), func() error {
				return forgetArtifactIndexReference(ctx, store.GraphRoot(), resolved.Reference)
			}); err != nil {
				return ResolvedImageSource{}, err
			}
			return materialized, nil
		}
	} else if err == nil {
		if resolved.Kind != oci.Artifact {
			return ResolvedImageSource{}, fmt.Errorf("file input %q is not an image or artifact", reference)
		}
		var selected ResolvedImageSource
		selected, err = pullArtifactFileSource(ctx, resolver, resolved, platform, store, configured)
		if err == nil {
			return selected, nil
		}
		var pullError *nativePullError
		if !errors.As(err, &pullError) {
			return ResolvedImageSource{}, err
		}
	}
	if cached != nil && resolver.PullPolicy() == oci.PullNewer && ctx.Err() == nil {
		if cached.Artifact {
			return *cached, nil
		}
		return selectImage(*cached)
	}
	return ResolvedImageSource{}, fmt.Errorf("resolve file input %q: %w", reference, err)
}

func resolveStoredArtifactFileSource(ctx context.Context, reference string, platform v1.Platform, store storage.Store, system *types.SystemContext) (ResolvedImageSource, error) {
	var result ResolvedImageSource
	err := withArtifactInputLock(ctx, store.GraphRoot(), func() error {
		artifacts, err := libartifact.NewArtifactStore(filepath.Join(store.GraphRoot(), "artifacts"), system)
		if err != nil {
			return err
		}
		candidates, err := shortnames.ResolveLocally(system, reference)
		if err != nil {
			return err
		}
		for _, candidate := range candidates {
			selected, err := inspectArtifactFileSource(ctx, artifacts, store.GraphRoot(), candidate.String())
			if err == nil {
				if err := forgetArtifactIndexReference(ctx, store.GraphRoot(), candidate.String()); err != nil {
					return err
				}
			} else if errors.Is(err, artifacttypes.ErrArtifactNotExist) {
				selected, err = inspectArtifactIndexSource(ctx, artifacts, store.GraphRoot(), candidate.String(), platform)
			}
			if err == nil {
				result = selected
				return nil
			}
			if !errors.Is(err, artifacttypes.ErrArtifactNotExist) {
				return err
			}
		}
		return artifacttypes.ErrArtifactNotExist
	})
	return result, err
}

func pullArtifactFileSource(ctx context.Context, resolver *oci.Resolver, resolved *oci.Resolved, platform v1.Platform, store storage.Store, system *types.SystemContext) (ResolvedImageSource, error) {
	var result ResolvedImageSource
	err := withArtifactInputLock(ctx, store.GraphRoot(), func() error {
		artifacts, openErr := libartifact.NewArtifactStore(filepath.Join(store.GraphRoot(), "artifacts"), system)
		if openErr != nil {
			return openErr
		}
		ref, parseErr := libartifact.NewArtifactReference(resolved.Reference)
		if parseErr != nil {
			return parseErr
		}
		var err error
		registry := resolver.RegistryOptions()
		options := libimage.CopyOptions{Credentials: registry.Credentials, Architecture: platform.Architecture, OS: platform.OS, Variant: platform.Variant, Writer: registry.ProgressWriter}
		storedReference := ref.String()
		indexed := manifest.MIMETypeIsMultiImage(resolved.Root.MediaType)
		if indexed {
			storedReference, err = artifactChildReference(resolved.Reference, resolved.Selected.Digest)
			if err != nil {
				return err
			}
			options.DestinationLookupReferenceFunc = func(types.ImageReference) (types.ImageReference, error) {
				return layout.NewReference(filepath.Join(store.GraphRoot(), "artifacts"), storedReference)
			}
		}
		if registry.RetrySet {
			options.MaxRetries = &registry.Retry
		}
		if registry.RetryDelay != 0 {
			options.RetryDelay = &registry.RetryDelay
		}
		options.OciDecryptConfig, err = cli.DecryptConfig(registry.DecryptionKeys)
		if err != nil {
			return &nativePullError{err: err}
		}
		if _, err := artifacts.Pull(ctx, ref, options); err != nil {
			return &nativePullError{err: err}
		}
		selected, inspectErr := inspectArtifactFileSource(ctx, artifacts, store.GraphRoot(), storedReference)
		if inspectErr != nil {
			return inspectErr
		}
		if !content.Equal(selected.Selected, resolved.Selected) {
			return fmt.Errorf("artifact file input %q changed while pulling: selected %s, received %s", resolved.Reference, resolved.Selected.Digest, selected.Selected.Digest)
		}
		if indexed {
			if err := retainArtifactIndex(ctx, store.GraphRoot(), resolved); err != nil {
				return fmt.Errorf("retain artifact index: %w", err)
			}
			if _, err := artifacts.Remove(ctx, ref.ToArtifactStoreReference()); err != nil && !errors.Is(err, artifacttypes.ErrArtifactNotExist) {
				return fmt.Errorf("replace cached flat artifact: %w", err)
			}
		} else if err := forgetArtifactIndexReference(ctx, store.GraphRoot(), resolved.Reference); err != nil {
			return err
		}
		result = selected
		return nil
	})
	return result, err
}

func inspectArtifactFileSource(ctx context.Context, artifacts *libartifact.ArtifactStore, graphRoot, reference string) (_ ResolvedImageSource, retErr error) {
	ref, err := libartifact.NewArtifactStorageReference(reference)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	artifact, err := artifacts.Inspect(ctx, ref)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	if artifact.Manifest.ArtifactType == "" && (artifact.Manifest.Config.MediaType == v1.MediaTypeImageConfig || artifact.Manifest.Config.MediaType == "application/vnd.docker.container.image.v1+json") {
		return ResolvedImageSource{}, fmt.Errorf("artifact store input %q is a container image", reference)
	}
	if len(artifact.Manifest.Layers) == 0 {
		return ResolvedImageSource{}, fmt.Errorf("artifact file input %q has no blobs", reference)
	}
	for _, layer := range artifact.Manifest.Layers {
		if layer.Size < 0 || layer.Digest.Algorithm() != digest.SHA256 || layer.Digest.Validate() != nil || layer.MediaType == "" {
			return ResolvedImageSource{}, fmt.Errorf("artifact file input %q contains an invalid blob descriptor", reference)
		}
	}
	// Read the original manifest, preserving its exact descriptor even when
	// typed native inspection has discarded unknown extension fields.
	native, err := layout.NewReference(filepath.Join(graphRoot, "artifacts"), artifact.Name)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	source, err := native.NewImageSource(ctx, artifacts.SystemContext)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	defer func() { retErr = errors.Join(retErr, source.Close()) }()
	raw, mediaType, err := image.UnparsedInstance(source, nil).Manifest(ctx)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	selected := oci.Descriptor(mediaType, raw)
	if selected.MediaType != v1.MediaTypeImageManifest || selected.Digest != artifact.Digest {
		return ResolvedImageSource{}, fmt.Errorf("artifact file input %q changed while being inspected", reference)
	}
	return ResolvedImageSource{Artifact: true, Root: selected, Selected: selected, Reference: reference}, nil
}

// extractArtifactSource uses Podman's raw-blob naming and extraction, then
// verifies the extracted files before Buildah can consume them.
func extractArtifactSource(ctx context.Context, store storage.Store, selected ResolvedImageSource) (_ string, _ func() error, retErr error) {
	if ctx == nil || store == nil || !selected.Artifact {
		return "", nil, errors.New("invalid artifact file input selection")
	}
	if err := validateSelectedInput(selected, true); err != nil {
		return "", nil, err
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	canonical, err := oci.ParseReference(selected.Reference)
	if err != nil {
		return "", nil, err
	}
	reference := canonical.Registry + "/" + canonical.Repository + "@" + selected.Selected.Digest.String()
	ref, err := libartifact.NewArtifactStorageReference(reference)
	if err != nil {
		return "", nil, err
	}
	artifacts, err := libartifact.NewArtifactStore(filepath.Join(store.GraphRoot(), "artifacts"), nil)
	if err != nil {
		return "", nil, err
	}
	frozen, err := inspectArtifactFileSource(ctx, artifacts, store.GraphRoot(), reference)
	if err != nil {
		return "", nil, err
	}
	if !content.Equal(frozen.Selected, selected.Selected) {
		return "", nil, errors.New("selected artifact manifest differs from its stored descriptor")
	}
	artifact, err := artifacts.Inspect(ctx, ref)
	if err != nil {
		return "", nil, fmt.Errorf("selected artifact %s is no longer available: %w", selected.Selected.Digest, err)
	}
	mounts, err := artifacts.BlobMountPaths(ctx, ref, &artifacttypes.BlobMountPathOptions{})
	if err != nil {
		return "", nil, err
	}
	if len(mounts) != len(artifact.Manifest.Layers) {
		return "", nil, errors.New("artifact blob metadata changed during extraction")
	}
	seen := make(map[string]digest.Digest, len(mounts))
	for i, mount := range mounts {
		if previous, exists := seen[mount.Name]; exists && previous != artifact.Manifest.Layers[i].Digest {
			return "", nil, fmt.Errorf("artifact has conflicting blob filename %q", mount.Name)
		}
		seen[mount.Name] = artifact.Manifest.Layers[i].Digest
	}
	directory, err := os.MkdirTemp("", "coopr-artifact-files-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() error { return os.RemoveAll(directory) }
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, cleanup())
		}
	}()
	if err := artifacts.Extract(ctx, ref, directory, &artifacttypes.ExtractOptions{}); err != nil {
		return "", nil, err
	}
	for i, mount := range mounts {
		filePath := filepath.Join(directory, mount.Name)
		// Blobs have no mode metadata. Match Podman's artifact tar export
		// instead of letting the host umask change portable cache results.
		if err := os.Chmod(filePath, 0o600); err != nil {
			return "", nil, err
		}
		file, err := os.Open(filePath)
		if err != nil {
			return "", nil, err
		}
		verified := content.NewVerifyReader(contextReader{ctx: ctx, reader: file}, artifact.Manifest.Layers[i])
		_, copyErr := io.Copy(io.Discard, verified)
		if copyErr == nil {
			copyErr = verified.Verify()
		}
		if err := errors.Join(copyErr, file.Close()); err != nil {
			return "", nil, fmt.Errorf("verify artifact file %q: %w", mount.Name, err)
		}
	}
	return directory, cleanup, nil
}
