// Package transfer copies already-built Coopr artifacts to named destinations.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"runtime"
	"strings"
	"time"

	"oras.land/oras-go/v2/content"

	"go.podman.io/common/libimage/manifests"
	"go.podman.io/storage"

	"coopr/internal/buildah"
	"coopr/internal/componentstore"
	"coopr/internal/enginecopy"
	"coopr/internal/imagestore"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	"coopr/internal/storeactivity"

	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	dockertransport "go.podman.io/image/v5/docker"
	dockerdaemon "go.podman.io/image/v5/docker/daemon"
	imagereference "go.podman.io/image/v5/docker/reference"
	ociarchive "go.podman.io/image/v5/oci/archive"
	"go.podman.io/image/v5/signature"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
)

type Destination struct {
	Transport        string
	Name             string
	ArchiveReference string
}

func (destination Destination) String() string {
	switch destination.Transport {
	case "local":
		return destination.Name
	case "registry":
		return "docker://" + destination.Name
	case "docker":
		return "docker-daemon:" + destination.Name
	case "oci-archive", "oci", "docker-archive":
		if destination.ArchiveReference != "" {
			return destination.Transport + ":" + destination.Name + ":" + destination.ArchiveReference
		}
	}
	return destination.Transport + ":" + destination.Name
}

type PushOptions = buildah.PushOptions

type Options struct {
	Push                PushOptions
	ProgressWriter      io.Writer `json:"-"`
	signingOutput       io.Writer
	ArchiveUncompressed *bool
	ArchiveReference    string // Name in the archive's outer index; rooted graph bytes are unchanged.
	ComponentStoreDir   string
	BuildStore          buildah.StoreOptions
	AuthFile            string
	CertDir             string
	TLSVerify           *bool
	Credentials         string
	Retry               uint
	RetrySet            bool
	RetryDelay          time.Duration
	DecryptionKeys      []string
	SignaturePolicyPath string
	Platform            v1.Platform
	PlatformExplicit    bool
	Signing             SigningOptions
}

func (opts Options) isPlainLocalAlias(destination Destination) bool {
	return destination.Transport == "local" && !opts.PlatformExplicit && !opts.Signing.Enabled() && !opts.Push.RemoveSignatures && !opts.Push.ChangesManifest()
}

func (opts Options) RegistryOptions() oci.Options {
	return oci.Options{
		ProgressWriter: opts.ProgressWriter, AuthFile: opts.AuthFile, CertDir: opts.CertDir, TLSVerify: opts.TLSVerify,
		Credentials: opts.Credentials, Retry: opts.Retry, RetrySet: opts.RetrySet, RetryDelay: opts.RetryDelay,
		DecryptionKeys: opts.DecryptionKeys, SignaturePolicyPath: opts.SignaturePolicyPath,
		ComponentStoreDir: opts.ComponentStoreDir, NativeStore: buildah.NativeStoreOptions(opts.BuildStore),
	}
}

func nativeStoreOptions(opts Options) (buildah.StoreOptions, error) {
	store := opts.BuildStore
	if store.RunRoot == "" && store.GraphRoot == "" {
		var err error
		store, err = buildah.DefaultStoreOptions()
		if err != nil {
			return buildah.StoreOptions{}, err
		}
	}
	return buildah.NormalizeStoreOptions(store)
}

// ParseTag accepts ordinary names, without interpreting transport prefixes.
func ParseTag(value string, kind oci.Kind) (Destination, error) {
	if value == "" {
		return Destination{}, errors.New("empty tag")
	}
	var name string
	var err error
	if kind == oci.Component {
		name, err = componentstore.NormalizeTag(value)
	} else {
		name, err = localstore.NormalizeImageTag(value)
	}
	if err != nil {
		return Destination{}, err
	}
	return Destination{Transport: "local", Name: name}, nil
}

// ParseDestination accepts ordinary local names or supported containers/image transports.
func ParseDestination(value string, kind oci.Kind) (Destination, error) {
	if value == "" {
		return Destination{}, errors.New("empty copy destination")
	}
	transport, name, explicit := destinationTransport(value)
	if !explicit {
		return ParseTag(value, kind)
	}
	if name == "" {
		return Destination{}, fmt.Errorf("%s destination requires a name", transport)
	}
	archiveReference := ""
	switch transport {
	case "registry":
		if _, err := dockertransport.Transport.ParseReference("//" + name); err != nil {
			return Destination{}, fmt.Errorf("registry destination: %w", err)
		}
	case "oci-archive":
		parsed, err := ociarchive.ParseReference(name)
		if err != nil {
			return Destination{}, fmt.Errorf("oci-archive destination: %w", err)
		}
		name, archiveReference, _ = strings.Cut(parsed.StringWithinTransport(), ":")
	case "containers-storage":
		if kind == oci.Component {
			return Destination{}, errors.New("containers-storage cannot store Coopr component artifacts")
		}
		if !strings.HasPrefix(name, "[") {
			if _, err := imagereference.ParseNormalizedNamed(name); err != nil {
				return Destination{}, err
			}
		}
	case "dir", "oci", "docker-archive":
		if kind == oci.Component {
			return Destination{}, fmt.Errorf("%s cannot store Coopr component artifacts", transport)
		}
		parsed, err := alltransports.ParseImageName(value)
		if err != nil {
			return Destination{}, err
		}
		name = parsed.StringWithinTransport()
		if transport == "oci" || transport == "docker-archive" {
			name, archiveReference, _ = strings.Cut(name, ":")
		}
	case "docker":
		if kind == oci.Component {
			return Destination{}, errors.New("docker-daemon cannot store Coopr component artifacts; use a component name, registry, or OCI archive")
		}
		if _, err := dockerdaemon.Transport.ParseReference(name); err != nil {
			return Destination{}, fmt.Errorf("invalid docker-daemon image tag %q: %w", name, err)
		}
	default:
		if kind == oci.Component {
			return Destination{}, fmt.Errorf("%s cannot store Coopr component artifacts", transport)
		}
		parsed, err := alltransports.ParseImageName(value)
		if err != nil {
			return Destination{}, err
		}
		name = parsed.StringWithinTransport()
	}
	return Destination{Transport: transport, Name: name, ArchiveReference: archiveReference}, nil
}

// ParsePushDestination defaults ordinary names to a registry destination.
func ParsePushDestination(value string, kind oci.Kind) (Destination, error) {
	if _, _, explicit := destinationTransport(value); explicit {
		return ParseDestination(value, kind)
	}
	return ParseDestination("docker://"+value, kind)
}

func destinationTransport(value string) (transport, name string, explicit bool) {
	for _, candidate := range []struct{ prefix, transport string }{
		{"docker://", "registry"}, {"docker-daemon:", "docker"}, {"oci-archive:", "oci-archive"},
	} {
		if name, found := strings.CutPrefix(value, candidate.prefix); found {
			return candidate.transport, name, true
		}
	}
	if native := alltransports.TransportFromImageName(value); native != nil && native.Name() != "docker" {
		transport, name, _ := strings.Cut(value, ":")
		return transport, name, true
	}
	return "", value, false
}

func LocalReference(tag string, root v1.Descriptor) string {
	if tag == "" {
		return root.Digest.String()
	}
	return tag
}

// Copy resolves a Coopr-local source without running a build step. Ordinary
// local aliases preserve native record identity; digest-selected exports retain
// the requested immutable manifest even after the source name changes.
func Copy(ctx context.Context, kind oci.Kind, source string, destination Destination, opts Options) (result string, retErr error) {
	if kind == oci.Image && !opts.isPlainLocalAlias(destination) {
		var err error
		opts.Push, err = opts.Push.WithDefaults()
		if err != nil {
			return "", err
		}
	}
	if destination.ArchiveReference != "" {
		opts.ArchiveReference = destination.ArchiveReference
	}
	if err := ValidateSigningDestination(kind, destination, opts.Signing); err != nil {
		return "", err
	}
	storeDir, err := storeDirectory(kind, opts)
	if err != nil {
		return "", err
	}
	roots := []string{storeDir}
	if kind == oci.Image {
		options, err := nativeStoreOptions(opts)
		if err != nil {
			return "", err
		}
		roots = buildah.ActivityRoots(options, "")
	}
	var activity *storeactivity.Lease
	if kind == oci.Image && destination.Transport == "local" && opts.Signing.SignBy != "" {
		// Native storage commits replace image-wide signature metadata when an
		// existing config ID is reused. Hold the store exclusively while local
		// signing updates the signature blobs and metadata as one Coopr operation.
		activity, err = storeactivity.AcquireExclusive(ctx, roots...)
	} else {
		activity, err = storeactivity.AcquireShared(ctx, roots...)
	}
	if err != nil {
		return "", fmt.Errorf("acquire copy store activity lease: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, activity.Close()) }()
	ctx = storeactivity.ContextWithLease(ctx, activity)
	if kind == oci.Image {
		return copyStoredImage(ctx, source, destination, opts)
	}
	selector, err := localSelector(source, kind)
	if err != nil {
		return "", err
	}
	store, err := localstore.Open(ctx, storeDir)
	if err != nil {
		return "", err
	}
	root, err := store.Resolve(ctx, selector)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", source, err)
	}
	if pinned := digest.Digest(selector); pinned.Validate() == nil && pinned != root.Digest {
		return "", fmt.Errorf("local digest %s resolved to %s", pinned, root.Digest)
	}
	return CopyRoot(ctx, kind, storeDir, root, destination, opts)
}

func copyStoredImage(ctx context.Context, source string, destination Destination, opts Options) (result string, retErr error) {
	selector, err := localSelector(source, oci.Image)
	if err != nil {
		return "", err
	}
	if within, explicit := strings.CutPrefix(selector, "containers-storage:"); explicit {
		options, err := nativeStoreOptions(opts)
		if err != nil {
			return "", err
		}
		err = buildah.WithStore(options, func(backend storage.Store) error {
			var ref types.ImageReference
			var err error
			if strings.HasPrefix(within, "[") {
				ref, err = alltransports.ParseImageName("containers-storage:" + within)
			} else {
				ref, err = imagestorage.Transport.ParseStoreReference(backend, within)
			}
			if err != nil {
				return err
			}
			resolved, image, err := imagestorage.ResolveReference(ref)
			if err != nil {
				return err
			}
			bound := resolved.Transport().(imagestorage.StoreTransport).GetStoreIfSet()
			if bound == nil || bound.GraphRoot() != backend.GraphRoot() || bound.RunRoot() != backend.RunRoot() || bound.GraphDriverName() != backend.GraphDriverName() {
				return errors.New("containers-storage source must use the configured image store")
			}
			selector = image.ID
			if named, ok := ref.DockerReference().(imagereference.Digested); ok {
				selector = named.Digest().String()
			}

			return nil
		})
		if err != nil {
			return "", err
		}
	}

	// Ordinary local aliases follow native tag identity, independently of the
	// immutable manifest selected for export or explicit-platform copies.
	if opts.isPlainLocalAlias(destination) {
		options, err := nativeStoreOptions(opts)
		if err != nil {
			return "", err
		}
		if err := buildah.WithStore(options, func(backend storage.Store) error {
			image, err := oci.LookupStoredNativeImage(ctx, backend, selector)
			if err != nil {
				return err
			}
			return image.Tag(destination.Name)
		}); err != nil {
			return "", fmt.Errorf("tag stored image %s: %w", source, err)
		}
		return destination.Name, nil
	}
	if !opts.PlatformExplicit {
		root, indexData, selections, err := storedSelections(ctx, opts, selector)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", source, err)
		}
		// Native list identity survives member transformations such as
		// decryption. Retained origin metadata cannot promote a leaf to a list.
		copyIndex := false
		if len(indexData) != 0 {
			options, err := nativeStoreOptions(opts)
			if err != nil {
				return "", err
			}
			if err := buildah.WithStore(options, func(backend storage.Store) error {
				image, err := oci.LookupStoredNativeImage(ctx, backend, selector)
				if err != nil {
					return err
				}
				nativeList, err := image.IsManifestList(ctx)
				if err != nil {
					return err
				}
				copyIndex = nativeList
				if nativeList || digest.Digest(selector).Validate() == nil || strings.Contains(selector, "@") {
					return nil
				}
				// Mutable image names select their native image, not a resident
				// sibling from retained origin metadata, even on another platform.
				selector = image.Digest().String()
				return nil
			}); err != nil {
				return "", fmt.Errorf("resolve stored index %s: %w", source, err)
			}
		}
		if copyIndex {
			return copyStoredIndex(ctx, root, indexData, selections, destination, opts)
		}
	}
	platform := opts.Platform
	if platform.OS == "" && platform.Architecture == "" {
		platform = v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	}
	selection, _, found, err := lookupStoredImage(ctx, opts, selector, platform, opts.PlatformExplicit)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", source, err)
	}
	if !found {
		return "", fmt.Errorf("resolve %s: image or platform not found", source)
	}
	storeOptions, err := nativeStoreOptions(opts)
	if err != nil {
		return "", err
	}
	if destination.Transport == "local" {
		if err := buildah.VerifyStoredImageSelectedSupervised(ctx, storeOptions, selection.ImageID, selection.Manifest); err != nil {
			return "", fmt.Errorf("resolve stored image %s: %w", source, err)
		}
		// A single-platform copy names the selected manifest, even when its
		// source was an index. Otherwise the alias would still resolve to the
		// complete index and silently copy every platform later.
		selection.Root = selection.Manifest
		if opts.Push.RemoveSignatures {
			if err := removeStoredManifestSignatures(ctx, storeOptions, selection.ImageID, selection.Manifest.Digest); err != nil {
				return "", err
			}
		}

		if opts.Signing.SignBy != "" {
			if err := signStoredImage(ctx, selection.ImageID, selection.Manifest.Digest, destination.Name, opts); err != nil {
				return "", err
			}
		}
		if err := tagStoredSelection(ctx, storeOptions, selection, destination.Name); err != nil {
			return "", fmt.Errorf("tag stored image %s: %w", source, err)
		}
		return destination.Name, nil
	}

	if destination.Transport == "registry" {
		return publishStoredImage(ctx, selection.ImageID, selection.Manifest, destination.Name, opts)
	}
	if destination.Transport == "oci-dir" {
		destination.Transport = "oci"
	}
	if (destination.Transport == "oci" || destination.Transport == "oci-archive") && !opts.Push.RemoveSignatures {
		if err := rejectStoredArchiveSignatures(ctx, storeOptions, selection.Manifest, map[string]oci.StoredSelection{"selected": selection}, destination.Transport); err != nil {
			return "", err
		}
	}
	transferOptions, cleanup, err := storedTransferOptions(selection.ImageID, selection.Manifest.Digest, opts)
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, cleanup()) }()
	transferOptions.ArchiveUncompressed = opts.ArchiveUncompressed
	if destination.Transport == "oci-archive" || destination.Transport == "oci" {
		destination.ArchiveReference = opts.ArchiveReference
	}
	transferOptions.Destination = destination.String()
	return buildah.TransferStoredImageSupervised(ctx, transferOptions)
}

// Native local copies can share a config ID. Remove only the selected
// manifest's signatures, retaining siblings and unrelated native metadata.
func removeStoredManifestSignatures(ctx context.Context, options buildah.StoreOptions, imageID string, selected digest.Digest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return buildah.WithStore(options, func(backend storage.Store) error {
		return removeStoredManifestSignaturesInStore(backend, imageID, selected)
	})
}

func removeStoredManifestSignaturesInStore(backend storage.Store, imageID string, selected digest.Digest) error {
	lock, err := manifests.LockerForImage(backend, imageID)
	if err != nil {
		return err
	}
	lock.Lock()
	defer lock.Unlock()
	raw, err := backend.ImageBigData(imageID, storage.ImageDigestBigDataKey)
	if err != nil {
		return err
	}
	previous, err := oci.CaptureStoredSignatures(backend, imageID)
	if err != nil {
		return err
	}
	return oci.WriteStoredManifestSignatures(backend, imageID, selected, digest.FromBytes(raw) == selected, nil, previous)
}

// OCI archive/layout destinations cannot store native signatures. Check before
// an unsigned export can erase that information; explicit removal allows it.
func rejectStoredArchiveSignatures(ctx context.Context, options buildah.StoreOptions, root v1.Descriptor, selections map[string]oci.StoredSelection, transport string) error {
	return buildah.WithStore(options, func(backend storage.Store) error {

		roots, err := backend.ImagesByDigest(root.Digest)
		if err != nil && !errors.Is(err, storage.ErrImageUnknown) {
			return err
		}
		for _, image := range roots {
			// The same native record may hold both a selected leaf and a root.
			// Check the root separately instead of overwriting its leaf entry.
			if err := rejectStoredManifestSignatures(ctx, backend, image.ID, root.Digest, transport); err != nil {
				return err
			}
		}
		for _, selected := range selections {
			if err := rejectStoredManifestSignatures(ctx, backend, selected.ImageID, selected.Manifest.Digest, transport); err != nil {
				return err
			}
		}
		return nil
	})
}

func rejectStoredManifestSignatures(ctx context.Context, backend storage.Store, imageID string, selected digest.Digest, transport string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := manifests.LockerForImage(backend, imageID)
	if err != nil {
		return err
	}
	lock.Lock()
	defer lock.Unlock()
	raw, err := backend.ImageBigData(imageID, storage.ImageDigestBigDataKey)
	if err != nil {
		return err
	}
	state, err := oci.CaptureStoredSignatures(backend, imageID)
	if err != nil {
		return err
	}
	blobs, err := oci.StoredSignatureBlobs(state, selected, digest.FromBytes(raw) == selected)
	if err != nil {
		return err
	}
	if len(blobs) != 0 {
		return fmt.Errorf("cannot copy signatures to %s: storing signatures for OCI images is not supported; explicitly remove signatures to continue", transport)
	}
	return nil
}

func tagStoredSelection(ctx context.Context, storeOptions buildah.StoreOptions, selection oci.StoredSelection, name string) error {
	return buildah.WithStore(storeOptions, func(backend storage.Store) error {
		_, err := imagestore.FromStore(backend).TagSelected(ctx, selection.ImageID, selection.Manifest, name)
		return err
	})
}

func copyStoredIndex(ctx context.Context, root v1.Descriptor, indexData []byte, selections map[string]oci.StoredSelection, destination Destination, opts Options) (result string, retErr error) {
	storeOptions, err := nativeStoreOptions(opts)
	if err != nil {
		return "", err
	}
	if destination.Transport == "local" {
		for _, selection := range selections {
			if err := buildah.VerifyStoredImageSelectedSupervised(ctx, storeOptions, selection.ImageID, selection.Manifest); err != nil {
				return "", fmt.Errorf("verify stored image %s: %w", selection.Manifest.Digest, err)
			}
			if opts.Push.RemoveSignatures {
				if err := removeStoredManifestSignatures(ctx, storeOptions, selection.ImageID, selection.Manifest.Digest); err != nil {
					return "", err
				}
			}

			if opts.Signing.SignBy != "" {
				if err := signStoredImage(ctx, selection.ImageID, selection.Manifest.Digest, destination.Name, opts); err != nil {
					return "", err
				}
			}
		}
		imageIDs := make(map[digest.Digest]string, len(selections))
		for _, selection := range selections {
			imageIDs[selection.Manifest.Digest] = selection.ImageID
		}
		if err := buildah.WithStore(storeOptions, func(backend storage.Store) error {
			_, err := imagestore.FromStore(backend).WriteStoredIndex(ctx, root, indexData, imageIDs, destination.Name)
			if err == nil && opts.Push.RemoveSignatures {
				// Index export collects exact-root signatures from every native
				// record retaining this digest, including historical roots.
				// Clear the same contributors so a later alias export cannot
				// resurrect signatures from an older record's metadata.
				records, lookupErr := backend.ImagesByDigest(root.Digest)
				if lookupErr != nil {
					return lookupErr
				}
				for _, record := range records {
					if err := removeStoredManifestSignaturesInStore(backend, record.ID, root.Digest); err != nil {
						return err
					}
				}
			}
			return err
		}); err != nil {
			return "", fmt.Errorf("tag stored multi-platform image: %w", err)
		}
		return destination.Name, nil
	}

	if destination.Transport == "oci-dir" {
		destination.Transport = "oci"
	}
	if (destination.Transport == "oci" || destination.Transport == "oci-archive") && !opts.Push.RemoveSignatures {
		if err := rejectStoredArchiveSignatures(ctx, storeOptions, root, selections, destination.Transport); err != nil {
			return "", err
		}
	}

	transferOptions, cleanup, err := storedTransferOptions("", root.Digest, opts)
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, cleanup()) }()
	transferOptions.Index = &buildah.StoredIndexSnapshot{Root: root, Data: indexData, Selections: selections}
	transferOptions.ArchiveUncompressed = opts.ArchiveUncompressed
	if destination.Transport == "oci-archive" || destination.Transport == "oci" {
		destination.ArchiveReference = opts.ArchiveReference
	}
	if destination.Transport == "registry" {
		transferOptions.RegistryDestination = destination.Name
	} else {
		transferOptions.Destination = destination.String()
	}
	return buildah.TransferStoredImageSupervised(ctx, transferOptions)
}

func storedSelections(ctx context.Context, opts Options, selector string) (root v1.Descriptor, data []byte, selections map[string]oci.StoredSelection, err error) {
	options, err := nativeStoreOptions(opts)
	if err != nil {
		return root, nil, nil, err
	}
	err = buildah.WithStore(options, func(backend storage.Store) error {
		root, data, selections, err = oci.StoredImageSelections(ctx, backend, selector)
		return err
	})
	return root, data, selections, err
}

func lookupStoredImage(ctx context.Context, opts Options, selector string, platform v1.Platform, explicitPlatform bool) (oci.StoredSelection, v1.Platform, bool, error) {
	root, _, selections, err := storedSelections(ctx, opts, selector)
	if errors.Is(err, storage.ErrImageUnknown) {
		return oci.StoredSelection{}, platform, false, nil
	}
	if err != nil {
		return oci.StoredSelection{}, platform, false, err
	}
	key := platforms.Format(platforms.Normalize(platform))
	if selection, found := selections[key]; found {
		return selection, platform, true, nil
	}
	if !explicitPlatform && len(selections) == 1 && (root.MediaType == v1.MediaTypeImageManifest || root.MediaType == "application/vnd.docker.distribution.manifest.v2+json" || digest.Digest(selector).Validate() != nil && !strings.Contains(selector, "@")) {
		for key, selection := range selections {
			actual, err := platforms.Parse(key)
			return selection, actual, err == nil, err
		}
	}
	return oci.StoredSelection{}, platform, false, nil
}

// CopyRoot transfers an OCI graph from a component store or temporary image
// export layout. Native stored images use Copy.
func CopyRoot(ctx context.Context, kind oci.Kind, storeDir string, root v1.Descriptor, destination Destination, opts Options) (string, error) {
	return copyRoot(ctx, kind, storeDir, root, destination, opts, nil)
}

func copyRoot(ctx context.Context, kind oci.Kind, storeDir string, root v1.Descriptor, destination Destination, opts Options, preparedPolicy *signature.Policy) (_ string, retErr error) {
	if kind == oci.Image {
		var err error
		opts.Push, err = opts.Push.WithDefaults()
		if err != nil {
			return "", err
		}
	}
	if destination.ArchiveReference != "" {
		opts.ArchiveReference = destination.ArchiveReference
	}
	if err := ValidateSigningDestination(kind, destination, opts.Signing); err != nil {
		return "", err
	}
	if root.Digest.Validate() != nil {
		return "", errors.New("invalid local OCI root digest")
	}
	activity, err := storeactivity.AcquireShared(ctx, storeDir)
	if err != nil {
		return "", fmt.Errorf("acquire copy store activity lease: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, activity.Close()) }()
	store, err := localstore.Open(ctx, storeDir)
	if err != nil {
		return "", err
	}
	resolved, err := store.Resolve(ctx, root.Digest.String())
	if err != nil || !content.Equal(resolved, root) {
		return "", fmt.Errorf("local OCI root %s is missing or differs: %v", root.Digest, err)
	}
	switch destination.Transport {
	case "local":
		if err := localstore.TagExisting(ctx, storeDir, root, destination.Name); err != nil {
			return "", err
		}
		return LocalReference(destination.Name, root), nil
	case "oci-archive", "oci-dir", "dir", "oci", "docker-archive", "containers-storage":

		if opts.ArchiveReference != "" {
			root.Annotations = maps.Clone(root.Annotations)
			if root.Annotations == nil {
				root.Annotations = make(map[string]string)
			}
			root.Annotations[v1.AnnotationRefName] = opts.ArchiveReference
		}
		if kind == oci.Image {
			if destination.Transport != "oci-archive" && destination.Transport != "oci-dir" {
				return copyImageDestination(ctx, storeDir, root, destination, opts, preparedPolicy)
			}
			return copyImageArchive(ctx, storeDir, root, destination, opts, preparedPolicy)
		}
		componentStore, err := storeDirectory(oci.Component, opts)
		if err != nil {
			return "", err
		}
		if err := validateComponentArchiveOutput(destination.Name, storeDir, componentStore); err != nil {
			return "", err
		}
		if err := localstore.WriteArchive(ctx, store, root, destination.Name); err != nil {
			return "", err
		}
		return destination.Name, nil
	case "registry":
		if kind == oci.Image {
			return publishImage(ctx, storeDir, root, destination.Name, opts)
		}
		resolver, err := oci.NewResolver(opts.RegistryOptions())
		if err != nil {
			return "", err
		}
		return resolver.PublishLayout(ctx, destination.Name, storeDir, root)
	case "docker":
		if kind != oci.Image {
			return "", fmt.Errorf("%s cannot store Coopr components", destination.Transport)
		}
		var result string
		err := localstore.WithReadLock(ctx, storeDir, func() error {
			var copyErr error
			if root.MediaType == v1.MediaTypeImageIndex || root.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json" {
				if !opts.Signing.Enabled() && !opts.Push.ChangesManifest() && !opts.Push.RemoveSignatures {
					result, copyErr = enginecopy.CopyIndex(ctx, storeDir, root, destination.Name)
					result += "@" + root.Digest.String()
					return copyErr
				}
			}
			nativeDestination := destination
			nativeDestination.Transport = "docker-daemon"
			result, copyErr = copyImageDestination(ctx, storeDir, root, nativeDestination, opts, preparedPolicy)
			return copyErr
		})
		if err != nil {
			return "", err
		}
		return result, nil
	default:
		if kind == oci.Image {
			return copyImageDestination(ctx, storeDir, root, destination, opts, preparedPolicy)
		}
		return "", fmt.Errorf("unsupported destination transport %q", destination.Transport)
	}
}

func storeDirectory(kind oci.Kind, opts Options) (string, error) {
	switch kind {
	case oci.Image:
		options, err := nativeStoreOptions(opts)
		return options.GraphRoot, err
	case oci.Component:
		if opts.ComponentStoreDir != "" {
			return opts.ComponentStoreDir, nil
		}
		return localstore.DefaultComponentDir()
	default:
		return "", fmt.Errorf("unsupported artifact kind %q", kind)
	}
}

func localSelector(value string, kind oci.Kind) (string, error) {
	if strings.HasPrefix(value, "sha256:") {
		parsed := digest.Digest(value)
		if parsed.Algorithm() != digest.SHA256 || parsed.Validate() != nil {
			return "", fmt.Errorf("invalid local digest %q", value)
		}
		return value, nil
	}
	selector := value
	if selector == "" {
		return "", errors.New("local tag is empty")
	}
	if kind == oci.Component {
		return componentstore.LocalSelector(selector)
	}
	if strings.TrimSpace(selector) != selector || strings.ContainsAny(selector, " \t\r\n") {
		return "", fmt.Errorf("invalid image reference %q", selector)
	}
	return selector, nil
}
