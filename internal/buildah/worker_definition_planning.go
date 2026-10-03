package buildah

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"coopr/internal/buildcontext"
	"coopr/internal/oci"
	"coopr/internal/planner"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// planDefinitionInWorker resolves external base metadata in the same rootless
// worker and storage namespace that will execute the resulting plan.
func planDefinitionInWorker(ctx context.Context, request planWorkerRequest) (*planner.Plan, *oci.Resolver, *selectedBaseState, error) {
	if request.Definition == nil || request.PlannerOptions == nil {
		return nil, nil, nil, errors.New("raw worker request lacks definition or planner options")
	}
	var resolver *oci.Resolver
	planning := *request.PlannerOptions
	planning.SourceDateEpochResolver = sourceDateEpochResolver(ctx, buildCredentialSource{secretSpecs: request.Secrets, sshSpecs: request.SSH})
	system := &types.SystemContext{
		SignaturePolicyPath:         request.SignaturePolicyPath,
		BigFilesTemporaryDir:        filepath.Dir(request.ResultPath),
		AuthFilePath:                request.AuthFile,
		DockerCertPath:              request.CertDir,
		DockerInsecureSkipTLSVerify: optionalBool(request.SkipTLSVerify),
	}
	ensureResolver := func() error {
		if resolver == nil {
			var err error
			resolver, err = oci.NewResolver(oci.Options{
				PlainHTTP: request.PlainHTTP, PlainHTTPRegistries: request.PlainHTTPRegistries,
				AuthFile: request.AuthFile, CertDir: request.CertDir, SkipTLSVerify: request.SkipTLSVerify,
				Credentials: request.Credentials, Retry: request.Retry, RetrySet: request.RetrySet, RetryDelay: request.RetryDelay, DecryptionKeys: request.DecryptionKeys, SignaturePolicyPath: request.SignaturePolicyPath,
				Pull: request.Pull, PullPolicy: request.PullPolicy, ImageStoreDir: request.ImageStoreDir, ComponentStoreDir: request.ComponentStoreDir, NativeStore: NativeStoreOptions(request.Store), NativeStoreShared: request.Store.Shared,
			})
			if err != nil {
				return fmt.Errorf("create build worker resolver: %w", err)
			}
		}
		return nil
	}
	withStore := func(resolve func(storage.Store) (ResolvedImageSource, error)) (ResolvedImageSource, error) {
		lease, err := acquireStore(request.Store)
		if err != nil {
			return ResolvedImageSource{}, fmt.Errorf("open build worker store for base binding: %w", err)
		}
		selected, resolveErr := resolve(lease.store)
		return selected, errors.Join(resolveErr, lease.Close())
	}
	plan, selected, planErr := planDefinitionWithExternalBaseState(ctx, request.Definition, planning, func(ctx context.Context, reference string, platform v1.Platform) (ResolvedImageSource, error) {
		if err := ensureResolver(); err != nil {
			return ResolvedImageSource{}, err
		}
		if request.Mode == "publish" && resolver.PullPolicy() != oci.PullNewer && !resolver.NativeStoreShared() {
			return SelectImageSource(ctx, resolver, reference, platform)
		}
		return withStore(func(store storage.Store) (ResolvedImageSource, error) {
			return ResolveImageSource(ctx, resolver, reference, platform, store, platformSystemContext(system, platform))
		})
	}, func(ctx context.Context, spec buildcontext.Spec, platform v1.Platform) (ResolvedImageSource, error) {
		if spec.Kind == buildcontext.DockerImage {
			if err := ensureResolver(); err != nil {
				return ResolvedImageSource{}, err
			}
		}
		return withStore(func(store storage.Store) (ResolvedImageSource, error) {
			return MaterializeNamedContext(ctx, spec, platform, store, platformSystemContext(system, platform), resolver, request.Secrets, request.SSH, request.ContextArtifacts...)
		})
	})
	if planErr != nil {
		return nil, nil, nil, planErr
	}
	return plan, resolver, selected, nil
}
