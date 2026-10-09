package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	"oras.land/oras-go/v2/content"
)

// ResolvedImageSource carries the verified OCI selection and config. Remote
// selections may be bound during planning before their storage image is
// materialized. Buildah's typed config is not authoritative for unknown fields.
type ResolvedImageSource struct {
	ImageID        string
	Root           v1.Descriptor
	Selected       v1.Descriptor
	SourceManifest *v1.Descriptor
	ConfigData     []byte
	Reference      string
	Remote         bool
	cached         *ResolvedImageSource
}

type nativeImagePullError struct{ err error }

func (e *nativeImagePullError) Error() string { return e.err.Error() }
func (e *nativeImagePullError) Unwrap() error { return e.err }

// SelectImageSource resolves immutable image metadata without opening
// containers/storage. Remote selections are imported only if execution is
// actually required; local native selections are verified at that same point.
func SelectImageSource(ctx context.Context, resolver *oci.Resolver, reference string, platform v1.Platform) (ResolvedImageSource, error) {
	if resolver == nil {
		return ResolvedImageSource{}, errors.New("nil OCI resolver")
	}
	storeOptions := resolver.NativeStoreOptions()
	if storeOptions.GraphRoot == "" {
		defaults, err := DefaultStoreOptions()
		if err != nil {
			return ResolvedImageSource{}, err
		}
		storeOptions = NativeStoreOptions(defaults)
	}
	lease, err := acquireStore(StoreOptions{GraphRoot: storeOptions.GraphRoot, RunRoot: storeOptions.RunRoot, ImageStore: storeOptions.ImageStore, GraphDriverName: storeOptions.GraphDriverName, GraphDriverOptions: storeOptions.GraphDriverOptions, TransientStore: storeOptions.TransientStore, Native: storeOptions})
	if err != nil {
		return ResolvedImageSource{}, err
	}
	defer func() { _ = lease.Close() }()
	return selectImageSource(ctx, resolver, reference, platform, lease.store)
}

func selectImageSource(ctx context.Context, resolver *oci.Resolver, reference string, platform v1.Platform, store storage.Store) (ResolvedImageSource, error) {
	normalized, transport, err := normalizeBaseSource(reference, "")
	if err != nil {
		return ResolvedImageSource{}, err
	}
	if transport {
		return resolveBaseSource(ctx, resolver, normalized, platform, store, nil, "", "")
	}
	reference = normalized
	canonical, err := oci.ParseReference(reference)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	var cached *ResolvedImageSource
	pinned := strings.Contains(reference, "@")
	if resolver.PullPolicy() != oci.PullAlways || pinned {
		stored, err := oci.ResolveStoredImage(ctx, store, reference, platform)
		if err == nil {
			selected := ResolvedImageSource{ImageID: stored.StorageImageID, Root: stored.Root, Selected: stored.Selected, SourceManifest: stored.SourceManifest, ConfigData: append(json.RawMessage(nil), stored.ConfigData...), Reference: canonical.String()}
			if resolver.PullPolicy() != oci.PullNewer || pinned {
				return selected, nil
			}
			cached = &selected
		} else if resolver.PullPolicy() == oci.PullNever && errors.Is(err, oci.ErrStoredPlatformUnavailable) {
			return ResolvedImageSource{}, err
		} else if !errors.Is(err, storage.ErrImageUnknown) && !errors.Is(err, oci.ErrStoredPlatformUnavailable) {
			return ResolvedImageSource{}, err
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
// returns its immutable storage ID and verified metadata.
func ResolveImageSource(ctx context.Context, resolver *oci.Resolver, reference string, platform v1.Platform, store storage.Store, system *types.SystemContext) (ResolvedImageSource, error) {
	if resolver == nil {
		return ResolvedImageSource{}, errors.New("nil OCI resolver")
	}
	if store == nil {
		return ResolvedImageSource{}, errors.New("nil containers/storage store")
	}
	selected, err := selectImageSource(ctx, resolver, reference, platform, store)
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

func materializeImageSource(ctx context.Context, resolver *oci.Resolver, reference string, platform v1.Platform, store storage.Store, system *types.SystemContext, selected ResolvedImageSource) (ResolvedImageSource, error) {
	var missingLocal error
	if !selected.Remote {
		stored, err := store.Image(selected.ImageID)
		if err == nil && stored != nil && stored.ID == selected.ImageID {
			selection := oci.StoredSelection{Root: selected.Root, Manifest: selected.Selected, ImageID: selected.ImageID, ConfigData: selected.ConfigData}
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
			missingLocal = errors.New("selected native image ID was not found")
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
	pullSystem := resolver.SystemContext()
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
	selection := oci.StoredSelection{
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

	if manifest.MIMETypeIsMultiImage(selection.Root.MediaType) {
		rootReference, err := oci.ParseReference(selected.Reference)
		if err != nil {
			return ResolvedImageSource{}, err
		}
		rootReference.Reference = selection.Root.Digest.String()
		resolvedRoot, err := resolver.ResolveRemoteImage(ctx, rootReference.String(), platform)
		if err != nil {
			return ResolvedImageSource{}, fmt.Errorf("retain pulled index metadata: %w", err)
		}
		data, err := content.FetchAll(ctx, resolvedRoot.Source(), selection.Root)
		if err != nil {
			return ResolvedImageSource{}, err
		}
		if digest.FromBytes(data) != selection.Root.Digest || int64(len(data)) != selection.Root.Size {
			return ResolvedImageSource{}, errors.New("pulled root metadata differs from selection")
		}
		if err := store.SetImageBigData(imageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+selection.Root.Digest.String(), data, manifest.Digest); err != nil {
			return ResolvedImageSource{}, err
		}
	}
	if err := oci.RecordStoredOrigin(ctx, store, destinationName.String(), selection); err != nil {
		return ResolvedImageSource{}, fmt.Errorf("record pulled native origin: %w", err)
	}
	selected.ImageID = imageID
	selected.Root, selected.Selected, selected.SourceManifest = selection.Root, selection.Manifest, selection.SourceManifest
	selected.Remote = false
	return selected, nil
}

// Native pulls retain the original encrypted manifest for provenance as well
// as the decrypted default. Builders and exports consume the latter, just as
// they do when Podman pulls an encrypted image into containers/storage.
func decryptedImageSelection(ctx context.Context, store storage.Store, system *types.SystemContext, selection oci.StoredSelection) (oci.StoredSelection, error) {
	reference, err := imagestorage.Transport.NewStoreReference(store, nil, selection.ImageID)
	if err != nil {
		return oci.StoredSelection{}, err
	}
	source, err := reference.NewImageSource(ctx, system)
	if err != nil {
		return oci.StoredSelection{}, err
	}
	defer func() { _ = source.Close() }()
	originalManifest := selection.Manifest
	if selection.SourceManifest != nil {
		originalManifest = *selection.SourceManifest
	}
	original, _, err := source.GetManifest(ctx, &originalManifest.Digest)
	if err != nil {
		return oci.StoredSelection{}, err
	}
	var encrypted v1.Manifest
	if err := json.Unmarshal(original, &encrypted); err != nil {
		return oci.StoredSelection{}, err
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
		return oci.StoredSelection{}, err
	}
	var decrypted v1.Manifest
	if err := json.Unmarshal(raw, &decrypted); err != nil {
		return oci.StoredSelection{}, err
	}
	if decrypted.Config.Digest != encrypted.Config.Digest || len(decrypted.Layers) != len(encrypted.Layers) {
		return oci.StoredSelection{}, errors.New("decrypted image differs from the verified source configuration or layer count")
	}
	for _, layer := range decrypted.Layers {
		if strings.HasSuffix(layer.MediaType, "+encrypted") {
			return oci.StoredSelection{}, errors.New("native image pull retained an encrypted default manifest")
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

func verifyStoredManifest(ctx context.Context, store storage.Store, system *types.SystemContext, selection oci.StoredSelection) error {
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
	// (for example OCI and Docker schema 2). Verify the immutable native
	// selection itself instead of whichever alternate is currently the default.
	manifest, mediaType, err := source.GetManifest(ctx, &selection.Manifest.Digest)
	if err != nil {
		return err
	}
	if mediaType != selection.Manifest.MediaType || int64(len(manifest)) != selection.Manifest.Size || digest.FromBytes(manifest) != selection.Manifest.Digest {
		return fmt.Errorf("stored image manifest differs from selected manifest %s", selection.Manifest.Digest)
	}
	return nil
}
