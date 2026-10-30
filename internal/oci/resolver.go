package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/strio-dev/coopr/internal/componentstore"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/imageconfig"
	"github.com/strio-dev/coopr/internal/planner"

	dockerreference "github.com/distribution/reference"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/pkg/auth"
	"go.podman.io/common/pkg/retry"
	nativemanifest "go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry"
)

const (
	ComponentArtifactType = "application/vnd.coopr.strio.dev.component.v1+json"
	ComponentConfigType   = "application/vnd.coopr.strio.dev.component.config.v1+json"
	ComponentPackageType  = "application/vnd.coopr.strio.dev.package.v1+tar"
	ComponentVersion      = "coopr.strio.dev/component/v1"
	maxMetadataBytes      = 8 << 20
	dockerManifestType    = "application/vnd.docker.distribution.manifest.v2+json"
	dockerIndexType       = "application/vnd.docker.distribution.manifest.list.v2+json"
	dockerConfigType      = "application/vnd.docker.container.image.v1+json"
)

type Kind string

const (
	Image     Kind = "image"
	Component Kind = "component"
	Artifact  Kind = "artifact"
	fileInput Kind = "file-input"
)

type PullPolicy string

const (
	PullMissing PullPolicy = "missing"
	PullAlways  PullPolicy = "always"
	PullNever   PullPolicy = "never"
	PullNewer   PullPolicy = "newer"
)

// ComponentMetadata is the verified config body of a published component.
// Package blobs must also be present as manifest layers.
type ComponentMetadata struct {
	Version   string                     `json:"version"`
	Platform  v1.Platform                `json:"platform"`
	Component planner.PublishedComponent `json:"component"`
	Packages  []Package                  `json:"packages,omitempty"`
}

type Package struct {
	Stage      string          `json:"stage"`
	Descriptor v1.Descriptor   `json:"descriptor"`
	Config     json.RawMessage `json:"config"` // Full OCI config for from=stage inheritance.
}

type Options struct {
	ProgressWriter      io.Writer `json:"-"`
	TLSVerify           *bool     // nil inherits native registry configuration.
	Pull                bool      // Resolve mutable image tags from their registry instead of the local image store.
	PullPolicy          string    // always, missing, never, or newer; Pull=true is an alias for always.
	AuthFile            string    // Explicit containers-auth.json or Docker config.json credentials file.
	CertDir             string    // Direct certificate directory containing ca.crt and optional client certificates.
	Credentials         string    // Explicit username[:password], overriding credential files for image pulls.
	Retry               uint
	RetrySet            bool
	RetryDelay          time.Duration
	DecryptionKeys      []string
	SignaturePolicyPath string
	ComponentStoreDir   string               // Override the local component OCI layout.
	NativeStore         storage.StoreOptions // Exact containers/storage selection for image inputs and outputs.
}

type Resolver struct {
	progressWriter    io.Writer
	pullPolicy        PullPolicy
	componentStoreDir string
	system            *types.SystemContext
	credentials       string
	retry             uint
	retrySet          bool
	retryDelay        time.Duration
	decryptionKeys    []string
	nativeStore       storage.StoreOptions
	retryOptions      *retry.Options
}

type Resolved struct {
	Reference      string             `json:"reference"`
	Repository     string             `json:"repository"`
	Kind           Kind               `json:"kind"`
	Platform       v1.Platform        `json:"platform"`
	Root           v1.Descriptor      `json:"root"`
	SourceManifest *v1.Descriptor     `json:"source_manifest,omitempty"`
	Selected       v1.Descriptor      `json:"selected"`
	Manifest       v1.Manifest        `json:"manifest"`
	Config         v1.Descriptor      `json:"config"`
	ConfigData     json.RawMessage    `json:"-"` // Verified bytes, before any typed image decoding.
	Layers         []v1.Descriptor    `json:"layers"`
	Component      *ComponentMetadata `json:"component,omitempty"`
	StorageImageID string             `json:"-"`
	Local          bool               `json:"local,omitempty"`
	Delivery       *ComponentDelivery `json:"delivery,omitempty"`
	source         content.ReadOnlyStorage
}

// Source exposes the read-only OCI content source selected by Resolve so a
// storage backend can import the verified descriptors into its own content
// store. Callers must still verify every copied blob before publishing names.
func (r *Resolved) Source() content.ReadOnlyStorage {
	if r == nil {
		return nil
	}
	return r.source
}

func NewResolver(opts Options) (*Resolver, error) {
	var err error
	pullPolicy, err := NormalizePullPolicy(opts.PullPolicy, opts.Pull)
	if err != nil {
		return nil, err
	}
	opts.AuthFile, opts.CertDir, err = NormalizeRegistryPaths(opts.AuthFile, opts.CertDir)
	if err != nil {
		return nil, err
	}
	system := &types.SystemContext{AuthFilePath: opts.AuthFile, DockerCertPath: opts.CertDir, SignaturePolicyPath: opts.SignaturePolicyPath, DockerDaemonHost: os.Getenv("DOCKER_HOST")}
	ApplyTLSVerify(system, opts.TLSVerify)
	if opts.Credentials != "" {
		username, password, _ := strings.Cut(opts.Credentials, ":")
		system.DockerAuthConfig = &types.DockerAuthConfig{Username: username, Password: password}
	}
	componentStoreDir := opts.ComponentStoreDir
	if componentStoreDir == "" {
		componentStoreDir, err = componentstore.DefaultDir()
		if err != nil {
			return nil, err
		}
	}
	retryOptions, err := RegistryRetryOptions(opts)
	if err != nil {
		return nil, err
	}
	return &Resolver{
		progressWriter:    opts.ProgressWriter,
		pullPolicy:        pullPolicy,
		componentStoreDir: componentStoreDir,
		system:            system, credentials: opts.Credentials, retry: opts.Retry, retrySet: opts.RetrySet,
		retryDelay: opts.RetryDelay, decryptionKeys: slices.Clone(opts.DecryptionKeys),
		nativeStore:  cloneNativeStoreOptions(opts.NativeStore),
		retryOptions: retryOptions,
	}, nil
}

// NormalizePullPolicy resolves the legacy --pull switch and the explicit
// policy without allowing contradictory behavior.
func NormalizePullPolicy(policy string, pull bool) (PullPolicy, error) {
	if policy == "" {
		policy = string(PullMissing)
	}
	switch PullPolicy(strings.ToLower(policy)) {
	case PullAlways:
		return PullAlways, nil
	case PullMissing:
		if pull {
			return PullAlways, nil
		}
		return PullMissing, nil
	case PullNever:
		if pull {
			return "", errors.New("pull=true conflicts with pull policy never")
		}
		return PullNever, nil
	case PullNewer, "ifnewer":
		if pull {
			return "", errors.New("pull=true conflicts with pull policy newer")
		}
		return PullNewer, nil
	default:
		return "", fmt.Errorf("unsupported pull policy %q: expected always, missing, never, or newer", policy)
	}
}

// NormalizeRegistryPaths resolves native auth defaults, validates registry inputs,
// and makes paths safe to pass across worker processes.
func NormalizeRegistryPaths(authFile, certDir string) (string, string, error) {
	var err error
	explicitAuthFile := authFile != "" || os.Getenv("REGISTRY_AUTH_FILE") != ""
	if authFile == "" {
		authFile = auth.GetDefaultAuthFile()
	}
	if authFile != "" {
		authFile, err = filepath.Abs(authFile)
		if err != nil {
			return "", "", fmt.Errorf("resolve registry auth file: %w", err)
		}
	}
	if certDir != "" {
		certDir, err = filepath.Abs(certDir)
		if err != nil {
			return "", "", fmt.Errorf("resolve registry certificate directory: %w", err)
		}
	}
	if err := validateCredentialFile(authFile); err != nil {
		if explicitAuthFile || !errors.Is(err, os.ErrNotExist) {
			return "", "", err
		}
		// An absent Docker search file is optional, unlike an explicit authfile.
		authFile = ""
	}
	if err := validateCertificateDirectory(certDir); err != nil {
		return "", "", err
	}
	return authFile, certDir, nil
}

func validateCredentialFile(path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect registry auth file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("registry auth file %q is not a regular file", path)
	}
	return nil
}

func validateCertificateDirectory(path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect registry certificate directory %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("registry certificate path %q is not a directory", path)
	}
	return nil
}

// NativeStoreOptions returns the exact native store associated with the local
// image storage, if the caller selected one explicitly.
func (r *Resolver) NativeStoreOptions() storage.StoreOptions {
	if r == nil {
		return storage.StoreOptions{}
	}
	return cloneNativeStoreOptions(r.nativeStore)
}

func cloneNativeStoreOptions(options storage.StoreOptions) storage.StoreOptions {
	options.GraphDriverOptions = slices.Clone(options.GraphDriverOptions)
	return options
}

// ComponentStoreDir returns the local OCI layout for published components.
func (r *Resolver) ComponentStoreDir() string { return r.componentStoreDir }

// PullImages reports whether mutable image references must be refreshed.
func (r *Resolver) PullImages() bool { return r.pullPolicy == PullAlways }

// PullPolicy reports how mutable remote image references use native local storage.
func (r *Resolver) PullPolicy() PullPolicy { return r.pullPolicy }

// SystemContext returns a private copy of the stock containers/image context
// used for registry resolution and pulls.
func (r *Resolver) SystemContext() *types.SystemContext {
	if r == nil || r.system == nil {
		return &types.SystemContext{}
	}
	copy := *r.system
	return &copy
}

// RegistryOptions returns a copy of the resolver's input policy. The progress
// writer is process-local and excluded when options are serialized for workers.
func (r *Resolver) RegistryOptions() Options {
	if r == nil {
		return Options{}
	}
	system := r.SystemContext()
	return Options{
		ProgressWriter: r.progressWriter,
		TLSVerify:      tlsVerifyOption(system.DockerInsecureSkipTLSVerify),
		PullPolicy:     string(r.pullPolicy), AuthFile: system.AuthFilePath, CertDir: system.DockerCertPath,
		Credentials: r.credentials, Retry: r.retry, RetrySet: r.retrySet, RetryDelay: r.retryDelay,
		DecryptionKeys: slices.Clone(r.decryptionKeys), SignaturePolicyPath: system.SignaturePolicyPath,
		ComponentStoreDir: r.componentStoreDir,
		NativeStore:       cloneNativeStoreOptions(r.nativeStore),
	}
}

// ResolveRemoteImage fetches a registry image after the Buildah backend has
// consulted native image storage.
func (r *Resolver) ResolveRemoteImage(ctx context.Context, reference string, platform v1.Platform) (*Resolved, error) {
	return r.resolveNativeImage(ctx, reference, platform)
}

// ResolveLayoutImage selects and verifies one platform image from an existing
// OCI image layout. The returned content source remains backed by the layout;
// callers which need an immutable execution input must copy or import the
// selected graph before allowing the layout to change.
func ResolveLayoutImage(ctx context.Context, layoutPath, selector string, platform v1.Platform) (*Resolved, error) {
	if ctx == nil {
		return nil, errors.New("OCI layout resolve context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if selector == "" {
		return nil, errors.New("OCI layout selector is empty")
	}
	absolute, err := filepath.Abs(layoutPath)
	if err != nil {
		return nil, fmt.Errorf("resolve OCI layout path: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve OCI layout links: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return nil, fmt.Errorf("inspect OCI layout: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("OCI layout %q is not a directory", canonical)
	}
	store, err := orasoci.NewWithContext(ctx, canonical)
	if err != nil {
		return nil, fmt.Errorf("open OCI layout %q: %w", canonical, err)
	}
	root, err := store.Resolve(ctx, selector)
	if err != nil {
		return nil, fmt.Errorf("resolve OCI layout selector %q: %w", selector, err)
	}
	resolver := &Resolver{}
	return resolver.resolveRoot(ctx, store, "oci-layout://"+canonical+"@"+selector, "oci-layout", root, platform, Image)
}

func (r *Resolver) resolveRemote(ctx context.Context, reference string, platform v1.Platform, kind Kind) (*Resolved, error) {
	ref, err := ParseReference(reference)
	if err != nil {
		return nil, err
	}
	repo, err := r.repository(ref)
	if err != nil {
		return nil, err
	}
	root, err := repo.Resolve(ctx, ref.Reference)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", reference, err)
	}
	return r.resolveRoot(ctx, repo, ref.String(), ref.Registry+"/"+ref.Repository, root, platform, kind)
}

// ParseReference canonicalizes familiar Docker-style references. Mutable tags
// resolve at build time, while digest references select immutable content.
func ParseReference(value string) (registry.Reference, error) {
	if value == "" {
		return registry.Reference{}, fmt.Errorf("empty OCI reference")
	}
	named, err := dockerreference.ParseNormalizedNamed(value)
	if err != nil {
		return registry.Reference{}, err
	}
	ref, err := registry.ParseReference(dockerreference.TagNameOnly(named).String())
	if err != nil {
		return ref, err
	}
	if err := ref.ValidateReference(); err != nil {
		return ref, fmt.Errorf("reference must include a tag or digest: %w", err)
	}
	return ref, nil
}

// ValidateComponentReference accepts conventional named component references
// and immutable local digests.
func ValidateComponentReference(value string) error {
	_, err := componentstore.NormalizeReference(value)
	return err
}

func (r *Resolver) Resolve(ctx context.Context, reference string, platform v1.Platform, kind Kind) (*Resolved, error) {
	if kind == Component {
		return r.ResolveComponent(ctx, reference, platform, nil, nil)
	}
	if kind == Image {
		return r.resolveNativeImage(ctx, reference, platform)
	}
	return r.resolveRemote(ctx, reference, platform, kind)
}

func (r *Resolver) resolveRoot(ctx context.Context, source content.ReadOnlyStorage, reference, repository string, root v1.Descriptor, platform v1.Platform, kind Kind) (*Resolved, error) {
	if err := checkPlatform(platform); err != nil {
		return nil, err
	}
	if kind != Image && kind != Component && kind != fileInput {
		return nil, fmt.Errorf("unsupported artifact kind %q", kind)
	}
	selected := root
	switch root.MediaType {
	case v1.MediaTypeImageIndex, dockerIndexType:
		data, err := fetchMetadata(ctx, source, root)
		if err != nil {
			return nil, fmt.Errorf("fetch root index: %w", err)
		}
		var index v1.Index
		if err := json.Unmarshal(data, &index); err != nil || index.SchemaVersion != 2 || index.MediaType != "" && index.MediaType != root.MediaType {
			return nil, fmt.Errorf("invalid OCI index: %v", err)
		}
		if kind != Component {
			list, err := nativemanifest.ListFromBlob(data, root.MediaType)
			if err != nil {
				return nil, fmt.Errorf("parse image index: %w", err)
			}
			system := r.SystemContext()
			system.OSChoice, system.ArchitectureChoice, system.VariantChoice = platform.OS, platform.Architecture, platform.Variant
			chosen, err := list.ChooseInstance(system)
			if err != nil {
				return nil, fmt.Errorf("select image index instance: %w", err)
			}
			found := false
			for _, desc := range index.Manifests {
				if desc.Digest == chosen {
					selected = desc
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("selected image instance %s is absent from index", chosen)
			}
		} else {
			var matches []v1.Descriptor
			for _, desc := range index.Manifests {
				if desc.Platform != nil && platformEqual(*desc.Platform, platform) {
					matches = append(matches, desc)
				}
			}
			if len(matches) != 1 {
				return nil, fmt.Errorf("index has %d manifests for platform %s/%s/%s; expected one", len(matches), platform.OS, platform.Architecture, platform.Variant)
			}
			selected = matches[0]
		}
	case v1.MediaTypeImageManifest, dockerManifestType:
	default:
		return nil, fmt.Errorf("unsupported root media type %q", root.MediaType)
	}
	if selected.MediaType != v1.MediaTypeImageManifest && selected.MediaType != dockerManifestType {
		return nil, fmt.Errorf("selected descriptor has unsupported media type %q", selected.MediaType)
	}
	data, err := fetchMetadata(ctx, source, selected)
	if err != nil {
		return nil, fmt.Errorf("fetch selected manifest: %w", err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil || manifest.SchemaVersion != 2 || manifest.MediaType != "" && manifest.MediaType != selected.MediaType {
		return nil, fmt.Errorf("invalid OCI manifest: %v", err)
	}
	if err := checkDescriptor(manifest.Config); err != nil {
		return nil, fmt.Errorf("manifest config: %w", err)
	}
	for i, layer := range manifest.Layers {
		if err := checkDescriptor(layer); err != nil {
			return nil, fmt.Errorf("manifest layer %d: %w", i, err)
		}
	}
	resolved := &Resolved{Reference: reference, Repository: repository, Kind: kind, Platform: platform, Root: root, Selected: selected, Manifest: manifest, Config: manifest.Config, Layers: manifest.Layers, source: source}
	configData, err := fetchMetadata(ctx, source, manifest.Config)
	if err != nil {
		return nil, fmt.Errorf("fetch config: %w", err)
	}
	resolved.ConfigData = json.RawMessage(configData)
	if kind == fileInput {
		resolved.Kind = Artifact
		if manifest.ArtifactType == "" && (manifest.Config.MediaType == v1.MediaTypeImageConfig || manifest.Config.MediaType == dockerConfigType) {
			resolved.Kind = Image
		}
	}
	switch resolved.Kind {
	case Image:
		if manifest.ArtifactType != "" || manifest.Config.MediaType != v1.MediaTypeImageConfig && manifest.Config.MediaType != dockerConfigType {
			return nil, fmt.Errorf("expected OCI image config, got %q", manifest.Config.MediaType)
		}
		var config v1.Image
		if err := json.Unmarshal(configData, &config); err != nil {
			return nil, fmt.Errorf("decode image config: %w", err)
		}
		// Native image pulling admits configurations with omitted or different
		// platform fields and warns during materialization. Descriptor selection
		// belongs to the native manifest-list chooser, not config admission.
	case Component:
		if manifest.ArtifactType != ComponentArtifactType || manifest.Config.MediaType != ComponentConfigType {
			return nil, fmt.Errorf("expected Coopr component artifact/config, got %q/%q", manifest.ArtifactType, manifest.Config.MediaType)
		}
		var meta ComponentMetadata
		if err := json.Unmarshal(configData, &meta); err != nil {
			return nil, fmt.Errorf("decode component metadata: %w", err)
		}
		if err := validateComponent(meta, manifest.Layers, platform); err != nil {
			return nil, err
		}
		resolved.Component = &meta
	}
	return resolved, nil
}

func validateComponent(meta ComponentMetadata, layers []v1.Descriptor, platform v1.Platform) error {
	if meta.Version != ComponentVersion || meta.Component.Output == "" || meta.Component.Definition == nil || !componentPlatformMatches(meta.Component.Platform, platform) || !platformEqual(meta.Platform, platform) {
		return fmt.Errorf("invalid component metadata version, output, definition, or platform")
	}
	if err := definition.Validate(meta.Component.Definition); err != nil {
		return fmt.Errorf("invalid component definition: %w", err)
	}
	required, err := planner.ValidatePublished(&meta.Component)
	if err != nil {
		return err
	}
	wanted := make(map[string]bool, len(required))
	for _, name := range required {
		wanted[name] = true
	}
	if len(meta.Packages) != len(layers) {
		return fmt.Errorf("component package count does not match manifest layers")
	}
	if len(wanted) != len(meta.Packages) {
		return fmt.Errorf("component packages do not match retained definition")
	}
	seen := make(map[string]bool, len(meta.Packages))
	for i, pkg := range meta.Packages {
		if pkg.Stage == "" || !wanted[pkg.Stage] || seen[pkg.Stage] || pkg.Descriptor.MediaType != ComponentPackageType || !content.Equal(pkg.Descriptor, layers[i]) {
			return fmt.Errorf("component package %d is invalid or unreachable", i)
		}
		seen[pkg.Stage] = true
		if len(pkg.Config) == 0 {
			return fmt.Errorf("component package %q lacks image configuration", pkg.Stage)
		}
		if _, err := imageconfig.Parse(pkg.Config); err != nil {
			return fmt.Errorf("component package %q configuration: %w", pkg.Stage, err)
		}
	}
	return nil
}

func componentPlatformMatches(value string, platform v1.Platform) bool {
	parts := strings.Split(value, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return false
	}
	component := v1.Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) == 3 {
		component.Variant = parts[2]
	}
	return component.OS == platform.OS && component.Architecture == platform.Architecture && normalizeVariant(component) == normalizeVariant(platform)
}

func fetchMetadata(ctx context.Context, source content.ReadOnlyStorage, desc v1.Descriptor) ([]byte, error) {
	if err := checkDescriptor(desc); err != nil {
		return nil, err
	}
	if desc.Size > maxMetadataBytes {
		return nil, fmt.Errorf("metadata descriptor too large: %d bytes", desc.Size)
	}
	return content.FetchAll(ctx, source, desc)
}

func checkDescriptor(desc v1.Descriptor) error {
	if desc.MediaType == "" || desc.Size < 0 || desc.Digest.Algorithm() != digest.SHA256 || desc.Digest.Validate() != nil {
		return fmt.Errorf("invalid descriptor media type, size, or digest")
	}
	return nil
}

func checkPlatform(platform v1.Platform) error {
	if platform.OS != "linux" || platform.Architecture == "" {
		return fmt.Errorf("unsupported platform %q/%q", platform.OS, platform.Architecture)
	}
	return nil
}

func platformEqual(a, b v1.Platform) bool {
	if a.OS != b.OS || a.Architecture != b.Architecture || normalizeVariant(a) != normalizeVariant(b) || a.OSVersion != b.OSVersion {
		return false
	}
	aFeatures := slices.Clone(a.OSFeatures)
	bFeatures := slices.Clone(b.OSFeatures)
	slices.Sort(aFeatures)
	slices.Sort(bFeatures)
	return slices.Equal(aFeatures, bFeatures)
}

func normalizeVariant(platform v1.Platform) string {
	if platform.Architecture == "arm64" && (platform.Variant == "" || platform.Variant == "v8") {
		return ""
	}
	return platform.Variant
}

// Download writes a verified blob atomically. It accepts only descriptors
// reachable from the resolved manifest, and leaves dest untouched on failure.
func (r *Resolver) Download(ctx context.Context, resolved *Resolved, desc v1.Descriptor, dest string) error {
	if resolved == nil {
		return fmt.Errorf("nil resolution")
	}
	if err := checkDescriptor(desc); err != nil {
		return err
	}
	reachable := content.Equal(desc, resolved.Config)
	for _, layer := range resolved.Layers {
		reachable = reachable || content.Equal(desc, layer)
	}
	if !reachable {
		return fmt.Errorf("descriptor is not reachable from selected manifest")
	}
	source := resolved.source
	if source == nil {
		ref, err := ParseReference(resolved.Reference)
		if err != nil {
			return err
		}
		repo, err := r.repository(ref)
		if err != nil {
			return err
		}
		source = repo
	}
	stream, err := source.Fetch(ctx, desc)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	file, err := os.CreateTemp(filepath.Dir(dest), ".coopr-blob-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	defer func() { _ = file.Close() }()
	verified := content.NewVerifyReader(stream, desc)
	if _, err := io.Copy(file, verified); err != nil {
		return err
	}
	if err := verified.Verify(); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), dest)
}

// Descriptor verifies a byte slice before it is used as registry content.
func Descriptor(mediaType string, data []byte) v1.Descriptor {
	descriptor := content.NewDescriptorFromBytes(strings.TrimSpace(mediaType), data)
	descriptor.MediaType = strings.TrimSpace(mediaType)
	return descriptor
}

// VersionedManifest is a convenience for test and future publisher code.
func VersionedManifest(config v1.Descriptor, layers []v1.Descriptor, artifactType string) v1.Manifest {
	return v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: artifactType, Config: config, Layers: layers}
}
