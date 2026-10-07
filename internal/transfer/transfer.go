// Package transfer copies already-built Coopr artifacts to named destinations.
package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"go.podman.io/storage"

	"coopr/internal/buildah"
	"coopr/internal/componentstore"
	"coopr/internal/enginecopy"
	"coopr/internal/imagestore"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	"coopr/internal/storeactivity"

	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

type Destination struct {
	Transport string
	Name      string
}

type Options struct {
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

func (opts Options) RegistryOptions() oci.Options {
	return oci.Options{
		AuthFile: opts.AuthFile, CertDir: opts.CertDir, TLSVerify: opts.TLSVerify,
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

// ParseDestination treats an unprefixed build tag as a Coopr-local name.
// Explicit transport prefixes have the same meaning for build tags and copy.
func ParseDestination(value string, kind oci.Kind) (Destination, error) {
	if value == "" {
		return Destination{}, errors.New("empty copy destination")
	}
	transport, name, explicit := strings.Cut(value, ":")
	if !explicit || !knownTransport(transport) {
		transport, name = "local", value
	}
	if name == "" {
		return Destination{}, fmt.Errorf("%s destination requires a name", transport)
	}
	switch transport {
	case "local":
		if kind == oci.Component {
			if err := componentstore.ValidateTag(name); err != nil {
				return Destination{}, err
			}
		} else {
			var err error
			name, err = localstore.NormalizeImageTag(name)
			if err != nil {
				return Destination{}, err
			}
		}
	case "registry":
		if _, err := oci.ParseReference(name); err != nil {
			return Destination{}, fmt.Errorf("registry destination: %w", err)
		}
	case "oci-archive":
		// The path is checked against build inputs by the build command.
	case "podman", "docker":
		if kind == oci.Component {
			return Destination{}, fmt.Errorf("%s cannot store Coopr component artifacts; use local, registry, or oci-archive", transport)
		}
		if transport == "podman" {
			var err error
			name, err = imagestore.NormalizeTag(name)
			if err != nil {
				return Destination{}, err
			}
		} else {
			parsed, err := reference.ParseNormalizedNamed(name)
			if err != nil {
				return Destination{}, fmt.Errorf("invalid %s image tag %q: %w", transport, name, err)
			}
			if _, ok := parsed.(reference.Digested); ok {
				return Destination{}, fmt.Errorf("%s image tag %q must not include a digest", transport, name)
			}
		}
	default:
		return Destination{}, fmt.Errorf("unsupported destination transport %q", transport)
	}
	return Destination{Transport: transport, Name: name}, nil
}

// ParsePushDestination maps --push with a tag to a registry destination and
// rejects explicit non-registry transports.
func ParsePushDestination(value string, kind oci.Kind) (Destination, error) {
	transport, name, hasPrefix := strings.Cut(value, ":")
	if hasPrefix && knownTransport(transport) {
		if transport != "registry" {
			return Destination{}, fmt.Errorf("--push cannot use %s:; use --tag %s:NAME without --push", transport, transport)
		}
		value = name
	}
	return ParseDestination("registry:"+value, kind)
}

func knownTransport(value string) bool {
	switch value {
	case "local", "registry", "oci-archive", "podman", "docker":
		return true
	}
	return false
}

func LocalReference(tag string, root v1.Descriptor) string {
	if tag == "" {
		return root.Digest.String()
	}
	return tag
}

// Copy resolves a Coopr-local source and applies the destination without
// running any build step. A bare digest remains immutable if a mutable name
// is retargeted later.
func Copy(ctx context.Context, kind oci.Kind, source string, destination Destination, opts Options) (result string, retErr error) {
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
		if destination.Transport == "podman" || strings.HasPrefix(source, "podman:") {
			defaults, err := buildah.DefaultStoreOptions()
			if err != nil {
				return "", err
			}
			roots = append(roots, buildah.ActivityRoots(defaults, "")...)
			selectedID, err := buildah.StoreIdentity(options)
			if err != nil {
				return "", err
			}
			defaultID, err := buildah.StoreIdentity(defaults)
			if err != nil {
				return "", err
			}
			if selectedID == defaultID {
				source = strings.TrimPrefix(source, "podman:")
				if destination.Transport == "podman" {
					destination.Transport = "local"
					defer func() {
						if retErr == nil {
							result = "podman:" + result
						}
					}()
				}
			}
		}
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
		if engine, name, explicit := strings.Cut(source, ":"); explicit && (engine == "podman" || engine == "docker") {
			return copyEngineImage(ctx, engine, name, destination, opts)
		}
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

func copyStoredImage(ctx context.Context, source string, destination Destination, opts Options) (string, error) {
	storeDir, err := storeDirectory(oci.Image, opts)
	if err != nil {
		return "", err
	}
	selector, err := localSelector(source, oci.Image)
	if err != nil {
		return "", err
	}
	if !opts.PlatformExplicit {
		root, indexData, selections, err := storedSelections(ctx, opts, selector)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", source, err)
		}
		var index v1.Index
		complete := len(indexData) != 0 && json.Unmarshal(indexData, &index) == nil && len(index.Manifests) == len(selections)
		if complete {
			return copyStoredIndex(ctx, storeDir, root, indexData, selections, destination, opts)
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
	if destination.Transport == "oci-archive" {
		if err := rejectStoreArchivePath(storeDir, destination.Name); err != nil {
			return "", err
		}
	}
	if destination.Transport == "registry" {
		return publishStoredImage(ctx, selection.ImageID, selection.Manifest, destination.Name, opts)
	}
	stageDir, err := os.MkdirTemp("", "coopr-image-copy-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(stageDir) }()
	layout := filepath.Join(stageDir, "layout")
	exported, err := buildah.ExportStoredImageSelectedSupervised(ctx, storeOptions, selection.ImageID, selection.Manifest.Digest, layout)
	if err != nil {
		return "", fmt.Errorf("export stored image %s: %w", source, err)
	}
	root, err := oci.LayoutRoot(exported.Layout)
	if err != nil {
		return "", err
	}
	if root.Digest != selection.Manifest.Digest || root.Size != selection.Manifest.Size || root.MediaType != selection.Manifest.MediaType {
		return "", fmt.Errorf("stored image %s manifest changed from %s/%s/%d to %s/%s/%d", source,
			selection.Manifest.Digest, selection.Manifest.MediaType, selection.Manifest.Size,
			root.Digest, root.MediaType, root.Size)
	}
	return CopyRoot(ctx, oci.Image, exported.Layout, root, destination, opts)
}

func tagStoredSelection(ctx context.Context, storeOptions buildah.StoreOptions, selection oci.StoredSelection, name string) error {
	return buildah.WithStore(storeOptions, func(backend storage.Store) error {
		_, err := imagestore.FromStore(backend).TagSelected(ctx, selection.ImageID, selection.Manifest, name)
		return err
	})
}

func copyStoredIndex(ctx context.Context, storeDir string, root v1.Descriptor, indexData []byte, selections map[string]oci.StoredSelection, destination Destination, opts Options) (string, error) {
	storeOptions, err := nativeStoreOptions(opts)
	if err != nil {
		return "", err
	}
	if destination.Transport == "local" {
		for _, selection := range selections {
			if err := buildah.VerifyStoredImageSelectedSupervised(ctx, storeOptions, selection.ImageID, selection.Manifest); err != nil {
				return "", fmt.Errorf("verify stored image %s: %w", selection.Manifest.Digest, err)
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
			return err
		}); err != nil {
			return "", fmt.Errorf("tag stored multi-platform image: %w", err)
		}
		return destination.Name, nil
	}
	if destination.Transport == "oci-archive" {
		if err := rejectStoreArchivePath(storeDir, destination.Name); err != nil {
			return "", err
		}
	}
	if destination.Transport == "registry" {
		unsigned := opts
		unsigned.Signing = SigningOptions{}
		for _, selection := range selections {
			instanceDestination, err := registryDigestDestination(destination.Name, selection.Manifest.Digest)
			if err != nil {
				return "", err
			}
			if _, err := publishStoredImage(ctx, selection.ImageID, selection.Manifest, instanceDestination, unsigned); err != nil {
				return "", fmt.Errorf("publish stored platform %s: %w", selection.Manifest.Digest, err)
			}
		}
	}
	stageDir, err := os.MkdirTemp("", "coopr-index-copy-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(stageDir) }()
	var index v1.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		return "", fmt.Errorf("decode stored image index: %w", err)
	}
	variants := make([]oci.ImageVariant, 0, len(index.Manifests))
	for i, manifestInIndex := range index.Manifests {
		if manifestInIndex.Platform == nil {
			return "", fmt.Errorf("stored image index manifest %d has no platform", i)
		}
		platform := platforms.Normalize(*manifestInIndex.Platform)
		key := platforms.Format(platform)
		selection, found := selections[key]
		if !found {
			return "", fmt.Errorf("stored image index lacks platform %s", key)
		}
		output := filepath.Join(stageDir, fmt.Sprintf("image-%d", i))
		exported, err := buildah.ExportStoredImageSelectedSupervised(ctx, storeOptions, selection.ImageID, selection.Manifest.Digest, output)
		if err != nil {
			return "", fmt.Errorf("export stored platform %s: %w", key, err)
		}
		manifest, err := oci.LayoutRoot(exported.Layout)
		if err != nil {
			return "", fmt.Errorf("inspect exported platform %s: %w", key, err)
		}
		if manifest.Digest != selection.Manifest.Digest || manifest.Size != selection.Manifest.Size || manifest.MediaType != selection.Manifest.MediaType {
			return "", fmt.Errorf("stored platform %s manifest changed from %s to %s", key, selection.Manifest.Digest, manifest.Digest)
		}
		variants = append(variants, oci.ImageVariant{Layout: exported.Layout, Manifest: manifestInIndex, Platform: platform})
	}
	layout := filepath.Join(stageDir, "index")
	if err := oci.RestoreImageIndex(ctx, layout, root, indexData, variants); err != nil {
		return "", fmt.Errorf("restore multi-platform image: %w", err)
	}
	return CopyRoot(ctx, oci.Image, layout, root, destination, opts)
}

func registryDigestDestination(destination string, manifest digest.Digest) (string, error) {
	named, err := reference.ParseNormalizedNamed(destination)
	if err != nil {
		return "", fmt.Errorf("parse registry destination %q: %w", destination, err)
	}
	return reference.TrimNamed(named).String() + "@" + manifest.String(), nil
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
func CopyRoot(ctx context.Context, kind oci.Kind, storeDir string, root v1.Descriptor, destination Destination, opts Options) (_ string, retErr error) {
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
	if err != nil || resolved.Digest != root.Digest || resolved.Size != root.Size || resolved.MediaType != root.MediaType {
		return "", fmt.Errorf("local OCI root %s is missing or differs: %v", root.Digest, err)
	}
	switch destination.Transport {
	case "local":
		if err := localstore.TagExisting(ctx, storeDir, root, destination.Name); err != nil {
			return "", err
		}
		return LocalReference(destination.Name, root), nil
	case "oci-archive":
		if err := rejectStoreArchivePath(storeDir, destination.Name); err != nil {
			return "", err
		}
		if kind == oci.Image {
			options, err := nativeStoreOptions(opts)
			if err != nil {
				return "", err
			}
			for _, boundary := range []string{options.GraphRoot, options.RunRoot, options.ImageStore} {
				if boundary == "" {
					continue
				}
				if err := rejectStoreArchivePath(boundary, destination.Name); err != nil {
					return "", err
				}
			}
		}
		if opts.ArchiveReference != "" {
			root.Annotations = maps.Clone(root.Annotations)
			if root.Annotations == nil {
				root.Annotations = make(map[string]string)
			}
			root.Annotations[v1.AnnotationRefName] = opts.ArchiveReference
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
	case "podman", "docker":
		if kind != oci.Image {
			return "", fmt.Errorf("%s cannot store Coopr components", destination.Transport)
		}
		var result string
		err := localstore.WithReadLock(ctx, storeDir, func() error {
			var copyErr error
			result, copyErr = enginecopy.Copy(ctx, storeDir, root, destination.Transport, destination.Name)
			return copyErr
		})
		return result, err
	default:
		return "", fmt.Errorf("unsupported destination transport %q", destination.Transport)
	}
}

func rejectStoreArchivePath(storeDir, output string) error {
	storePath, err := canonicalArchiveBoundary(storeDir)
	if err != nil {
		return err
	}
	parent, err := canonicalArchiveBoundary(filepath.Dir(output))
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(storePath, filepath.Join(parent, filepath.Base(output)))
	if err != nil {
		return err
	}
	if relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("archive output %q would overwrite the Coopr store", output)
	}
	return nil
}

// A configured native root or output parent need not exist yet. Resolve existing
// ancestors without creating directories while checking the archive boundary.
func canonicalArchiveBoundary(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(abs)
		if err == nil {
			return filepath.Join(append([]string{resolved}, suffix...)...), nil
		}
		if !errors.Is(err, os.ErrNotExist) || abs == filepath.Dir(abs) {
			return "", err
		}
		suffix = append([]string{filepath.Base(abs)}, suffix...)
		abs = filepath.Dir(abs)
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
	selector := strings.TrimPrefix(value, "local:")
	if selector == "" {
		return "", errors.New("local tag is empty")
	}
	if kind == oci.Component {
		if err := componentstore.ValidateTag(selector); err != nil {
			return "", err
		}
	} else {
		if strings.TrimSpace(selector) != selector || strings.ContainsAny(selector, " \t\r\n") {
			return "", fmt.Errorf("invalid image reference %q", selector)
		}
	}
	return selector, nil
}
