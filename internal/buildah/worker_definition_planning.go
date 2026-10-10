package buildah

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	sourcePolicy := request.SourcePolicy
	var err error
	if sourcePolicy == nil {
		sourcePolicy, err = loadSourcePolicy(request.SourcePolicyFile)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	var resolver *oci.Resolver
	planning := *request.PlannerOptions
	planning.DeferImageSource = deferredPolicySource(sourcePolicy)
	planning.TransientRunMounts = TransientMountInstructions(request.TransientRunMounts)
	planning.SourceDateEpochResolver = sourceDateEpochResolver(ctx, buildCredentialSource{secretSpecs: request.Secrets, sshSpecs: request.SSH})
	system := &types.SystemContext{
		SignaturePolicyPath:  request.SignaturePolicyPath,
		BigFilesTemporaryDir: filepath.Dir(request.ResultPath),
		AuthFilePath:         request.AuthFile,
		DockerCertPath:       request.CertDir,
	}
	oci.ApplyTLSVerify(system, request.TLSVerify)
	ensureResolver := func() error {
		if resolver == nil {
			var err error
			resolver, err = oci.NewResolver(oci.Options{
				ProgressWriter: os.Stderr,
				AuthFile:       request.AuthFile, CertDir: request.CertDir, TLSVerify: request.TLSVerify,
				Credentials: request.Credentials, Retry: request.Retry, RetrySet: request.RetrySet, RetryDelay: request.RetryDelay, DecryptionKeys: request.DecryptionKeys, SignaturePolicyPath: request.SignaturePolicyPath,
				Pull: request.Pull, PullPolicy: request.PullPolicy, ComponentStoreDir: request.ComponentStoreDir, NativeStore: NativeStoreOptions(request.Store),
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
		return withStore(func(store storage.Store) (ResolvedImageSource, error) {
			// Publication checks package results before materializing bases. Keep
			// newer eager so a failed pull binds the cached config before planning.
			if request.Mode == "publish" && resolver.PullPolicy() != oci.PullNewer {
				converted, err := policyImageSource(reference, sourcePolicy)
				if err != nil {
					return ResolvedImageSource{}, err
				}
				normalized, transport, err := normalizeBaseSource(converted, request.ContextDir)
				if err != nil {
					return ResolvedImageSource{}, err
				}
				if !transport {
					return selectImageSource(ctx, resolver, normalized, platform, store)
				}
				return resolveBaseSourceWithPolicy(ctx, resolver, converted, platform, store, platformSystemContext(system, platform), request.ContextDir, nil)
			}
			return resolveBaseSourceWithPolicy(ctx, resolver, reference, platform, store, platformSystemContext(system, platform), request.ContextDir, sourcePolicy)
		})
	}, func(ctx context.Context, spec buildcontext.Spec, platform v1.Platform) (ResolvedImageSource, error) {
		if spec.Kind == buildcontext.DockerImage {
			if err := ensureResolver(); err != nil {
				return ResolvedImageSource{}, err
			}
		}
		return withStore(func(store storage.Store) (ResolvedImageSource, error) {
			if spec.Kind == buildcontext.DockerImage {
				return resolveBaseSourceWithPolicy(ctx, resolver, spec.Reference, platform, store, platformSystemContext(system, platform), request.ContextDir, sourcePolicy)
			}
			return MaterializeNamedContext(ctx, spec, platform, store, platformSystemContext(system, platform), resolver, request.Secrets, request.SSH, request.ContextArtifacts...)
		})
	})
	if planErr != nil {
		return nil, nil, nil, planErr
	}
	if resolver == nil {
		for _, input := range plan.Inputs {
			if input.Kind == "image" {
				if err := ensureResolver(); err != nil {
					return nil, nil, nil, err
				}
				break
			}
		}
	}
	return plan, resolver, selected, nil
}
