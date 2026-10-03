package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"coopr/internal/imagecatalog"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah/pkg/cli"
	"go.podman.io/common/libimage"
	libconfig "go.podman.io/common/pkg/config"
	dockerreference "go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// ResolvedImageSource carries the verified OCI selection and config. Remote
// selections may be bound during planning before their storage image is
// materialized. Buildah's typed config is not authoritative for unknown fields.
type ResolvedImageSource struct {
	ImageID        string
	Root           v1.Descriptor
	Selected       v1.Descriptor
	SourceManifest *v1.Descriptor
	ConfigData     json.RawMessage
	Reference      string
	Remote         bool
	cached         *ResolvedImageSource
}

type nativeImagePullError struct{ err error }

func (e *nativeImagePullError) Error() string { return e.err.Error() }
func (e *nativeImagePullError) Unwrap() error { return e.err }

// SelectImageSource resolves immutable image metadata without opening
// containers/storage. Remote selections are imported only if execution is
// actually required; local catalog selections are verified at that same point.
func SelectImageSource(ctx context.Context, resolver *oci.Resolver, reference string, platform v1.Platform) (ResolvedImageSource, error) {
	if resolver == nil {
		return ResolvedImageSource{}, errors.New("nil OCI resolver")
	}
	canonical, err := oci.ParseReference(reference)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	storeDir := resolver.ImageStoreDir()
	var selectors []string
	if at := strings.LastIndex(reference, "@"); at >= 0 {
		pinned := digest.Digest(reference[at+1:])
		if pinned.Algorithm() == digest.SHA256 && pinned.Validate() == nil {
			selectors = append(selectors, pinned.String())
		}
	}
	if local, err := localstore.NormalizeImageTag(reference); err == nil {
		selectors = append(selectors, local)
	}
	selectors = append(selectors, canonical.String())
	var cached *ResolvedImageSource
	pinned := len(selectors) > 0 && strings.HasPrefix(selectors[0], "sha256:")
	if !resolver.PullImages() || pinned {
		for _, selector := range selectors {
			selection, found, err := imagecatalog.Lookup(ctx, storeDir, selector, platform)
			if err != nil {
				return ResolvedImageSource{}, fmt.Errorf("lookup cached base image %q: %w", reference, err)
			}
			if found {
				selected := ResolvedImageSource{
					ImageID: selection.ImageID, Root: selection.Root, Selected: selection.Manifest,
					ConfigData: append(json.RawMessage(nil), selection.ConfigData...), Reference: canonical.String(),
					SourceManifest: selection.SourceManifest,
				}
				if resolver.PullPolicy() != oci.PullNewer || pinned {
					return selected, nil
				}
				cached = &selected
				break
			}
		}
	}
	if resolver.PullPolicy() == oci.PullNever {
		return ResolvedImageSource{}, fmt.Errorf("base image %q is not available locally and pull policy is never", reference)
	}
	resolved, err := resolver.ResolveRemoteImage(ctx, reference, platform)
	if err != nil {
		if cached != nil && ctx.Err() == nil {
			return *cached, nil
		}
		return ResolvedImageSource{}, fmt.Errorf("resolve base image %q: %w", reference, err)
	}
	// Like Podman's newer policy, compare immutable content rather than image
	// creation timestamps, which may be reproducibly fixed or misleading.
	if cached != nil {
		previous := cached.Selected
		if cached.SourceManifest != nil {
			previous = *cached.SourceManifest
		}
		if previous.Digest == resolved.Selected.Digest {
			return *cached, nil
		}
	}
	return ResolvedImageSource{
		ImageID: resolved.Config.Digest.Encoded(), Root: resolved.Root, Selected: resolved.Selected,
		ConfigData: append(json.RawMessage(nil), resolved.ConfigData...), Reference: resolved.Reference, Remote: true,
		cached: cached,
	}, nil
}

// ResolveImageSource reuses a committed image in the canonical Buildah graph.
// A cache miss pulls the selected platform directly into that graph, then
// commits its immutable storage ID and metadata to the catalog.
func ResolveImageSource(ctx context.Context, resolver *oci.Resolver, reference string, platform v1.Platform, store storage.Store, system *types.SystemContext) (ResolvedImageSource, error) {
	if resolver == nil {
		return ResolvedImageSource{}, errors.New("nil OCI resolver")
	}
	if store == nil {
		return ResolvedImageSource{}, errors.New("nil containers/storage store")
	}
	if resolver.NativeStoreShared() && resolver.PullPolicy() != oci.PullAlways && !strings.Contains(reference, "@") {
		stored, storedErr := oci.ResolveStoredImage(ctx, store, reference, platform)
		if storedErr == nil {
			selection := imagecatalog.Selection{
				Root: stored.Root, Manifest: stored.Selected, ImageID: stored.StorageImageID,
				ConfigData: append(json.RawMessage(nil), stored.ConfigData...),
			}
			for _, selector := range localImageSelectors(reference) {
				if err := imagecatalog.Commit(ctx, resolver.ImageStoreDir(), selector, platform, selection); err != nil {
					return ResolvedImageSource{}, fmt.Errorf("refresh shared-store image %q: %w", reference, err)
				}
			}
		} else if errors.Is(storedErr, storage.ErrImageUnknown) {
			// Native names are authoritative in a shared store. Remove stale
			// Coopr aliases after Podman retags/removes an image; immutable digest
			// records remain available for exact references.
			for _, selector := range localImageSelectors(reference) {
				if _, err := imagecatalog.RemoveReference(ctx, resolver.ImageStoreDir(), selector); err != nil {
					return ResolvedImageSource{}, fmt.Errorf("remove stale shared-store image %q: %w", reference, err)
				}
			}
		} else {
			return ResolvedImageSource{}, storedErr
		}
	}
	selected, err := SelectImageSource(ctx, resolver, reference, platform)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	materialized, err := materializeImageSource(ctx, resolver, reference, platform, store, system, selected)
	var pullError *nativeImagePullError
	if errors.As(err, &pullError) && selected.cached != nil && resolver.PullPolicy() == oci.PullNewer && ctx.Err() == nil {
		// Like native PullPolicyNewer, copy failures retain an available local
		// image. Resolve this before planning binds config, ONBUILD and keys.
		cached, cacheErr := materializeImageSource(ctx, resolver, reference, platform, store, system, *selected.cached)
		if cacheErr == nil {
			return cached, nil
		}
		return ResolvedImageSource{}, errors.Join(err, fmt.Errorf("cached image fallback: %w", cacheErr))
	}
	return materialized, err
}

func localImageSelectors(reference string) []string {
	selectors := []string{reference}
	if local, err := localstore.NormalizeImageTag(reference); err == nil && local != reference {
		selectors = append(selectors, local)
	}
	if canonical, err := oci.ParseReference(reference); err == nil && canonical.String() != reference {
		selectors = append(selectors, canonical.String())
	}
	return slices.Compact(selectors)
}

func materializeImageSource(ctx context.Context, resolver *oci.Resolver, reference string, platform v1.Platform, store storage.Store, system *types.SystemContext, selected ResolvedImageSource) (ResolvedImageSource, error) {
	var missingLocal error
	if !selected.Remote {
		stored, err := store.Image(selected.ImageID)
		if err == nil && stored != nil && stored.ID == selected.ImageID {
			selection := imagecatalog.Selection{Root: selected.Root, Manifest: selected.Selected, ImageID: selected.ImageID, ConfigData: selected.ConfigData}
			if err := verifyStoredManifest(ctx, store, system, selection); err != nil {
				return ResolvedImageSource{}, fmt.Errorf("cached base image %q: %w", reference, err)
			}
			return selected, nil
		}
		if err != nil && !errors.Is(err, storage.ErrImageUnknown) {
			return ResolvedImageSource{}, fmt.Errorf("inspect cached base image %q: %w", reference, err)
		}
		missingLocal = err
		if missingLocal == nil {
			missingLocal = errors.New("catalog image ID was not found")
		}
	}
	if resolver == nil {
		if missingLocal != nil {
			return ResolvedImageSource{}, fmt.Errorf("cached base image %q is missing from the image graph and cannot be restored without an OCI resolver: %w", reference, missingLocal)
		}
		return ResolvedImageSource{}, errors.New("nil OCI resolver for selected remote image")
	}
	registryReference := selected.Reference
	if registryReference == "" {
		registryReference = reference
	}
	pullSystem := resolver.NativeSystemContext(registryReference)
	destinationName, err := dockerreference.ParseDockerRef(registryReference)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	if _, pinnedDestination := destinationName.(dockerreference.Digested); pinnedDestination {
		destinationName = dockerreference.TagNameOnly(dockerreference.TrimNamed(destinationName))
	}
	// Materialize the immutable selection whose config was already admitted,
	// including when repairing a cache miss after its mutable tag has moved.
	pinned, err := oci.ParseReference(registryReference)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	pinned.Reference = selected.Selected.Digest.String()
	if selected.SourceManifest != nil {
		pinned.Reference = selected.SourceManifest.Digest.String()
	}
	registryReference = pinned.String()
	if system != nil {
		if system.SignaturePolicyPath != "" {
			pullSystem.SignaturePolicyPath = system.SignaturePolicyPath
		}
		if system.BigFilesTemporaryDir != "" {
			pullSystem.BigFilesTemporaryDir = system.BigFilesTemporaryDir
		}
	}
	pullSystem.OSChoice = platform.OS
	pullSystem.ArchitectureChoice = platform.Architecture
	pullSystem.VariantChoice = platform.Variant
	runtime, err := libimage.RuntimeFromStore(store, &libimage.RuntimeOptions{SystemContext: pullSystem})
	if err != nil {
		return ResolvedImageSource{}, fmt.Errorf("initialize native image pull: %w", err)
	}
	registryOptions := resolver.RegistryOptions()
	decryptConfig, err := cli.DecryptConfig(registryOptions.DecryptionKeys)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	pullOptions := &libimage.PullOptions{CopyOptions: libimage.CopyOptions{
		Architecture: platform.Architecture, OS: platform.OS, Variant: platform.Variant,
		Credentials: registryOptions.Credentials, OciDecryptConfig: decryptConfig,
		SignaturePolicyPath: pullSystem.SignaturePolicyPath,
		// The source is immutable, but the destination name must permit the
		// native copier to rewrite encrypted layer descriptors after decryption.
		DestinationLookupReferenceFunc: func(types.ImageReference) (types.ImageReference, error) {
			return imagestorage.Transport.ParseStoreReference(store, destinationName.String())
		},
	}}
	if registryOptions.RetrySet {
		pullOptions.MaxRetries = &registryOptions.Retry
	}
	if registryOptions.RetryDelay != 0 {
		pullOptions.RetryDelay = &registryOptions.RetryDelay
	}
	images, err := runtime.Pull(ctx, registryReference, libconfig.PullPolicyAlways, pullOptions)
	if err != nil {
		err = &nativeImagePullError{err: err}
		if missingLocal != nil {
			return ResolvedImageSource{}, fmt.Errorf("cached base image %q is missing from the image graph and selected manifest %s could not be restored: %w", reference, selected.Selected.Digest, err)
		}
		return ResolvedImageSource{}, fmt.Errorf("pull base image %q into image graph: %w", reference, err)
	}
	if len(images) != 1 {
		return ResolvedImageSource{}, fmt.Errorf("pull base image %q returned %d images; expected one", reference, len(images))
	}
	imageID := images[0].ID()
	selection := imagecatalog.Selection{
		Root: selected.Root, Manifest: selected.Selected, ImageID: imageID, ConfigData: selected.ConfigData,
		SourceManifest: selected.SourceManifest,
	}
	if err := verifyStoredManifest(ctx, store, system, selection); err != nil {
		return ResolvedImageSource{}, fmt.Errorf("verify imported base image %q: %w", reference, err)
	}
	if decryptConfig != nil {
		selection, err = decryptedImageSelection(ctx, store, system, selection)
		if err != nil {
			return ResolvedImageSource{}, fmt.Errorf("select decrypted base image %q: %w", reference, err)
		}
	}
	if err := imagecatalog.Commit(ctx, resolver.ImageStoreDir(), selected.Reference, platform, selection); err != nil {
		return ResolvedImageSource{}, fmt.Errorf("catalog imported base image %q: %w", reference, err)
	}
	if selection.SourceManifest != nil {
		canonical, err := oci.ParseReference(selected.Reference)
		if err != nil {
			return ResolvedImageSource{}, err
		}
		canonical.Reference = selection.SourceManifest.Digest.String()
		if err := imagecatalog.CommitSelected(ctx, resolver.ImageStoreDir(), canonical.String(), platform, selection); err != nil {
			return ResolvedImageSource{}, fmt.Errorf("catalog encrypted source reference %q: %w", reference, err)
		}
	}
	selected.ImageID = imageID
	selected.Root, selected.Selected, selected.SourceManifest = selection.Root, selection.Manifest, selection.SourceManifest
	selected.Remote = false
	return selected, nil
}

// Native pulls retain the original encrypted manifest for provenance as well
// as the decrypted default. Builders and exports consume the latter, just as
// they do when Podman pulls an encrypted image into containers/storage.
func decryptedImageSelection(ctx context.Context, store storage.Store, system *types.SystemContext, selection imagecatalog.Selection) (imagecatalog.Selection, error) {
	reference, err := imagestorage.Transport.NewStoreReference(store, nil, selection.ImageID)
	if err != nil {
		return imagecatalog.Selection{}, err
	}
	source, err := reference.NewImageSource(ctx, system)
	if err != nil {
		return imagecatalog.Selection{}, err
	}
	defer func() { _ = source.Close() }()
	originalManifest := selection.Manifest
	if selection.SourceManifest != nil {
		originalManifest = *selection.SourceManifest
	}
	original, _, err := source.GetManifest(ctx, &originalManifest.Digest)
	if err != nil {
		return imagecatalog.Selection{}, err
	}
	var encrypted v1.Manifest
	if err := json.Unmarshal(original, &encrypted); err != nil {
		return imagecatalog.Selection{}, err
	}
	encryptedInput := false
	for _, layer := range encrypted.Layers {
		encryptedInput = encryptedInput || strings.HasSuffix(layer.MediaType, "+encrypted")
	}
	if !encryptedInput {
		return selection, nil
	}
	raw, mediaType, err := source.GetManifest(ctx, nil)
	if err != nil {
		return imagecatalog.Selection{}, err
	}
	var decrypted v1.Manifest
	if err := json.Unmarshal(raw, &decrypted); err != nil {
		return imagecatalog.Selection{}, err
	}
	if decrypted.Config.Digest != encrypted.Config.Digest || len(decrypted.Layers) != len(encrypted.Layers) {
		return imagecatalog.Selection{}, errors.New("decrypted image differs from the verified source configuration or layer count")
	}
	for _, layer := range decrypted.Layers {
		if strings.HasSuffix(layer.MediaType, "+encrypted") {
			return imagecatalog.Selection{}, errors.New("native image pull retained an encrypted default manifest")
		}
	}
	origin := originalManifest
	selection.SourceManifest = &origin
	selection.Manifest = oci.Descriptor(mediaType, raw)
	if !manifest.MIMETypeIsMultiImage(selection.Root.MediaType) {
		selection.Root = selection.Manifest
	}
	return selection, nil
}

func verifyStoredManifest(ctx context.Context, store storage.Store, system *types.SystemContext, selection imagecatalog.Selection) error {
	reference, err := imagestorage.Transport.NewStoreReference(store, nil, selection.ImageID)
	if err != nil {
		return err
	}
	source, err := reference.NewImageSource(ctx, system)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	// containers/storage can retain multiple manifests for one image-config ID
	// (for example OCI and Docker schema 2). Verify the immutable catalog
	// selection itself instead of whichever alternate is currently the default.
	manifest, mediaType, err := source.GetManifest(ctx, &selection.Manifest.Digest)
	if err != nil {
		return err
	}
	if mediaType != selection.Manifest.MediaType || int64(len(manifest)) != selection.Manifest.Size || digest.FromBytes(manifest) != selection.Manifest.Digest {
		return fmt.Errorf("stored image manifest differs from catalog selection %s", selection.Manifest.Digest)
	}
	return nil
}
