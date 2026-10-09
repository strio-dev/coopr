package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"coopr/internal/oci"
	"github.com/containerd/platforms"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah/pkg/cli"
	"go.podman.io/buildah/pkg/sourcepolicy"
	"go.podman.io/common/libimage"
	libconfig "go.podman.io/common/pkg/config"
	"go.podman.io/image/v5/image"
	"go.podman.io/image/v5/pkg/shortnames"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// normalizeBaseSource follows containers/image's transport registry. Plain
// names keep the registry/native-store path, including the image docker:latest.
func normalizeBaseSource(reference, contextDir string) (string, bool, error) {
	transport := alltransports.TransportFromImageName(reference)
	if transport != nil {
		switch transport.Name() {
		case "docker", "docker-daemon", "atomic", "containers-storage", "oci", "oci-archive", "docker-archive", "dir":
		default:
			return "", false, fmt.Errorf("unexpected container image transport %q", transport.Name())
		}
	}
	if transport == nil || transport.Name() == "docker" && !strings.HasPrefix(reference, "docker://") {
		return reference, false, nil
	}
	if transport.Name() == "docker" {
		return strings.TrimPrefix(reference, "docker://"), false, nil
	}
	prefix, value, _ := strings.Cut(reference, ":")
	switch prefix {
	case "oci", "oci-archive", "docker-archive", "dir":
		path, suffix, hasSuffix := strings.Cut(value, ":")
		if prefix == "dir" {
			path = value
			suffix = ""
			hasSuffix = false
		}
		if contextDir != "" {
			path = filepath.Join(contextDir, strings.TrimPrefix(filepath.Clean("/"+path), "/"))
		}
		value = path
		if hasSuffix {
			value += ":" + suffix
		}
		reference = prefix + ":" + value
	}
	if transport.Name() == "containers-storage" {
		return reference, true, nil
	}
	if _, err := alltransports.ParseImageName(reference); err != nil {
		return "", false, err
	}
	return reference, true, nil
}

func policyBaseSource(reference, policyFile string) (string, error) {
	policy, err := loadSourcePolicy(policyFile)
	if err != nil {
		return "", err
	}
	return policyImageSource(reference, policy)
}

func policyImageSource(reference string, policy *sourcepolicy.Policy) (string, error) {
	decision, matched, err := policy.Evaluate(sourcepolicy.ImageSourceIdentifier(reference))
	if err != nil {
		return "", err
	}
	if !matched {
		return reference, nil
	}
	switch decision.Action {
	case sourcepolicy.ActionDeny:
		return "", fmt.Errorf("source policy denied image %q: %s", reference, decision.Reason)
	case sourcepolicy.ActionConvert:
		return sourcepolicy.ExtractImageRef(decision.TargetRef), nil
	default:
		return reference, nil
	}
}

func resolveBaseSource(ctx context.Context, resolver *oci.Resolver, reference string, platform v1.Platform, store storage.Store, system *types.SystemContext, contextDir, policyFile string) (ResolvedImageSource, error) {
	policy, err := loadSourcePolicy(policyFile)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	return resolveBaseSourceWithPolicy(ctx, resolver, reference, platform, store, system, contextDir, policy)
}

func resolveBaseSourceWithPolicy(ctx context.Context, resolver *oci.Resolver, reference string, platform v1.Platform, store storage.Store, system *types.SystemContext, contextDir string, policy *sourcepolicy.Policy) (ResolvedImageSource, error) {
	reference, err := policyImageSource(reference, policy)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	reference, transport, err := normalizeBaseSource(reference, contextDir)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	if !transport {
		return ResolveImageSource(ctx, resolver, reference, platform, store, system)
	}
	clean, cleanup, err := sanitizeTransportSource(ctx, reference, contextDir)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	defer cleanup()
	selected, err := resolveTransportSource(ctx, resolver, clean, platform, store, system)
	if err == nil {
		selected.Reference = reference
	}
	return selected, err
}

// Native libimage owns transport copying and platform selection. Inspect the
// imported immutable image before admitting its configuration to the planner.
func resolveTransportSource(ctx context.Context, resolver *oci.Resolver, reference string, platform v1.Platform, store storage.Store, system *types.SystemContext) (_ ResolvedImageSource, retErr error) {
	if system == nil {
		system = &types.SystemContext{}
	}
	if resolver != nil {
		configured := resolver.SystemContext()
		configured.OSChoice = platform.OS
		configured.ArchitectureChoice = platform.Architecture
		configured.VariantChoice = platform.Variant
		if system.SignaturePolicyPath != "" {
			configured.SignaturePolicyPath = system.SignaturePolicyPath
		}
		if system.BigFilesTemporaryDir != "" {
			configured.BigFilesTemporaryDir = system.BigFilesTemporaryDir
		}
		system = configured
	}
	copiedSystem := *system
	copiedSystem.OSChoice = platform.OS
	copiedSystem.ArchitectureChoice = platform.Architecture
	copiedSystem.VariantChoice = platform.Variant
	system = &copiedSystem
	runtime, err := libimage.RuntimeFromStore(store, &libimage.RuntimeOptions{SystemContext: system})
	if err != nil {
		return ResolvedImageSource{}, err
	}
	var id string
	if strings.HasPrefix(reference, "containers-storage:") {
		ref, err := imagestorage.Transport.ParseStoreReference(store, strings.TrimPrefix(reference, "containers-storage:"))
		if err != nil {
			return ResolvedImageSource{}, err
		}
		_, native, err := imagestorage.ResolveReference(ref)
		if err != nil {
			return ResolvedImageSource{}, err
		}
		id = native.ID
	} else {
		opts := &libimage.PullOptions{CopyOptions: libimage.CopyOptions{Architecture: platform.Architecture, OS: platform.OS, Variant: platform.Variant}}
		if resolver != nil {
			registry := resolver.RegistryOptions()
			opts.Credentials = registry.Credentials
			if registry.RetrySet {
				opts.MaxRetries = &registry.Retry
			}
			if registry.RetryDelay != 0 {
				opts.RetryDelay = &registry.RetryDelay
			}
			opts.OciDecryptConfig, err = cli.DecryptConfig(registry.DecryptionKeys)
			if err != nil {
				return ResolvedImageSource{}, err
			}
		}
		pullPolicy := libconfig.PullPolicyMissing
		if resolver != nil {
			pullPolicy, err = libconfig.ParsePullPolicy(string(resolver.PullPolicy()))
			if err != nil {
				return ResolvedImageSource{}, err
			}
		}
		images, err := runtime.Pull(ctx, reference, pullPolicy, opts)
		if err != nil {
			return ResolvedImageSource{}, fmt.Errorf("import base image %q: %w", reference, err)
		}
		if len(images) != 1 {
			return ResolvedImageSource{}, fmt.Errorf("base image %q selected %d images; expected one", reference, len(images))
		}
		id = images[0].ID()
	}
	ref, err := imagestorage.Transport.NewStoreReference(store, nil, id)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	src, err := ref.NewImageSource(ctx, system)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	defer func() { retErr = errors.Join(retErr, src.Close()) }()
	native, err := image.FromSource(ctx, system, src)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	raw, mediaType, err := native.Manifest(ctx)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	config, err := native.ConfigBlob(ctx)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	var parsed v1.Image
	if err := json.Unmarshal(config, &parsed); err != nil {
		return ResolvedImageSource{}, err
	}
	actualPlatform := platforms.Normalize(parsed.Platform)
	requestedPlatform := platforms.Normalize(platform)
	if actualPlatform.OS != requestedPlatform.OS || actualPlatform.Architecture != requestedPlatform.Architecture || requestedPlatform.Variant != "" && actualPlatform.Variant != requestedPlatform.Variant {
		return ResolvedImageSource{}, fmt.Errorf("base image %q platform %s/%s/%s differs from requested %s/%s/%s", reference, parsed.OS, parsed.Architecture, parsed.Variant, platform.OS, platform.Architecture, platform.Variant)
	}
	descriptor := oci.Descriptor(mediaType, raw)
	return ResolvedImageSource{ImageID: id, Root: descriptor, Selected: descriptor, ConfigData: config, Reference: reference}, nil
}

func loadSourcePolicy(file string) (*sourcepolicy.Policy, error) {
	if file == "" {
		return nil, nil
	}
	return sourcepolicy.LoadFromFile(file)
}

func validateSourcePolicy(file string) error {
	_, err := loadSourcePolicy(file)
	return err
}

func deferredPolicySource(policy *sourcepolicy.Policy) func(string) (bool, error) {
	return func(reference string) (bool, error) {
		converted, err := policyImageSource(reference, policy)
		if err != nil {
			return false, err
		}
		prefix, _, _ := strings.Cut(converted, ":")
		switch prefix {
		case "oci", "oci-archive", "docker-archive", "dir":
			return true, nil
		}
		return false, nil
	}
}

// sourceBaseName recovers the Docker identity separately from the immutable
// storage reference used for execution. Filesystem transports have no Docker
// identity and must not acquire a made-up registry name.
func sourceBaseName(store storage.Store, source ResolvedImageSource) (string, error) {
	reference := source.Reference
	if reference == "" {
		return "", nil
	}
	var ref types.ImageReference
	var err error
	if strings.HasPrefix(reference, "containers-storage:") {
		ref, err = imagestorage.Transport.ParseStoreReference(store, strings.TrimPrefix(reference, "containers-storage:"))
	} else if alltransports.TransportFromImageName(reference) != nil {
		ref, err = alltransports.ParseImageName(reference)
	} else {
		ref, err = alltransports.ParseImageName("docker://" + reference)
	}
	if err != nil {
		return "", err
	}
	named := ref.DockerReference()
	if named == nil && !strings.HasPrefix(reference, "containers-storage:") {
		return "", nil
	}
	name := ""
	if named != nil {
		name = named.String()
	}
	selected, err := store.Image(source.ImageID)
	if err != nil {
		return "", err
	}
	if len(selected.Names) > 0 {
		// Match Buildah new.go's getImageName selection against the already
		// pinned image, without resolving the mutable source name again.
		prefix, _, _ := strings.Cut(reference, ":")
		parts := strings.Split(prefix, "/")
		prefix, _, _ = strings.Cut(parts[len(parts)-1], "@")
		name = selected.Names[0]
		for _, candidate := range selected.Names {
			if strings.Contains(candidate, prefix) {
				name = candidate
				break
			}
		}
	}
	if shortnames.IsShortName(name) {
		return "", nil
	}
	return name, nil
}
