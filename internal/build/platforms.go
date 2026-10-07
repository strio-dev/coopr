package build

import (
	"context"
	"errors"
	"fmt"
	"go/types"
	"slices"
	"strings"

	"coopr/internal/buildah"
	"coopr/internal/buildcontext"
	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"github.com/containerd/platforms"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
)

func discoverBuildPlatforms(ctx context.Context, def *definition.Definition, planning planner.Options, opts Options, componentStoreDir string) ([]string, error) {
	from, err := planner.ResolveFromSources(def, planning)
	if err != nil {
		return nil, err
	}
	resolver, err := oci.NewResolver(oci.Options{
		ComponentStoreDir: componentStoreDir,
		NativeStore:       buildah.NativeStoreOptions(opts.BuildStore),
		Pull:              opts.Pull, PullPolicy: opts.PullPolicy, PlainHTTP: opts.PlainHTTP, PlainHTTPRegistries: opts.PlainHTTPRegistries,
		AuthFile: opts.AuthFile, CertDir: opts.CertDir, SkipTLSVerify: opts.SkipTLSVerify,
		Credentials: opts.Credentials, Retry: opts.Retry, RetrySet: opts.RetrySet, RetryDelay: opts.RetryDelay, DecryptionKeys: opts.DecryptionKeys, SignaturePolicyPath: opts.SignaturePolicyPath,
	})
	if err != nil {
		return nil, err
	}
	var common map[string]bool
	for _, base := range from {
		var available []v1.Platform
		switch base.Kind {
		case planner.FromSourceImage:
			available, err = availableImagePlatforms(ctx, resolver, opts.BuildStore, base.Source)
		case planner.FromSourceContext:
			switch base.Context.Kind {
			case buildcontext.DockerImage:
				available, err = availableImagePlatforms(ctx, resolver, opts.BuildStore, base.Context.Reference)
			case buildcontext.OCILayout:
				available, err = oci.LayoutImagePlatforms(ctx, base.Context.Path, base.Context.Reference)
			default:
				continue // A filesystem context does not constrain image platforms.
			}
		default:
			continue // Stage references and scratch have no image index to inspect.
		}
		if err != nil {
			return nil, fmt.Errorf("discover platforms for base %s: %w", base.Source, err)
		}
		current := map[string]bool{}
		for _, platform := range available {
			if platform.OS == "linux" && types.SizesFor("gc", platform.Architecture) != nil {
				current[platforms.Format(platforms.Normalize(platform))] = true
			}
		}
		if common == nil {
			common = current
		} else {
			for key := range common {
				if !current[key] {
					delete(common, key)
				}
			}
		}
	}
	if common == nil {
		return nil, fmt.Errorf("--all-platforms requires at least one non-scratch image base")
	}
	result := make([]string, 0, len(common))
	for key := range common {
		result = append(result, key)
	}
	slices.Sort(result)
	if len(result) == 0 {
		return nil, fmt.Errorf("base images have no common runnable Linux platforms")
	}
	return result, nil
}

func availableImagePlatforms(ctx context.Context, resolver *oci.Resolver, store buildah.StoreOptions, reference string) ([]v1.Platform, error) {
	var err error
	pinned := strings.Contains(reference, "@")
	var local []v1.Platform
	err = buildah.WithStore(store, func(backend storage.Store) error {
		_, _, selections, nativeErr := oci.StoredImageSelections(ctx, backend, reference)
		if errors.Is(nativeErr, storage.ErrImageUnknown) {
			return nil
		}
		if nativeErr != nil {
			return nativeErr
		}
		for key := range selections {
			platform, err := platforms.Parse(key)
			if err != nil {
				return err
			}
			local = append(local, platform)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(local) > 0 && (pinned || resolver.PullPolicy() == oci.PullMissing || resolver.PullPolicy() == oci.PullNever) {
		return local, nil
	}
	if resolver.PullPolicy() == oci.PullNever {
		return nil, fmt.Errorf("base image %s is not available locally and pull policy is never", reference)
	}
	remote, err := resolver.ImagePlatforms(ctx, reference)
	if err != nil && resolver.PullPolicy() == oci.PullNewer && len(local) > 0 && ctx.Err() == nil {
		return local, nil
	}
	return remote, err
}
