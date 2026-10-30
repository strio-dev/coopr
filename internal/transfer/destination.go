package transfer

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/enginecopy"
	"github.com/strio-dev/coopr/internal/imagestore"
	"github.com/strio-dev/coopr/internal/oci"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/pkg/retry"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/signature"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// Docker's index loader bypasses native image copying, so evaluate the same
// per-image policy used for exact OCI graph exports before contacting it.
func copyImageIndexToDocker(ctx context.Context, layout string, root v1.Descriptor, name string, opts Options, policy *signature.Policy) (_ string, retErr error) {
	system := &types.SystemContext{SignaturePolicyPath: opts.SignaturePolicyPath, BigFilesTemporaryDir: os.TempDir()}
	reference, err := imagestore.LayoutReference(layout, root)
	if err != nil {
		return "", err
	}
	if policy == nil {
		policy, err = signature.DefaultPolicy(system)
		if err != nil {
			return "", err
		}
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, policyContext.Destroy()) }()
	source, err := reference.NewImageSource(ctx, system)
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, source.Close()) }()
	graph, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		return "", err
	}
	if err := approveArchiveGraph(ctx, graph, source, root, policyContext); err != nil {
		return "", err
	}
	result, err := enginecopy.CopyIndex(ctx, layout, root, name)
	if err != nil {
		return "", err
	}
	return result + "@" + root.Digest.String(), nil
}

// Copy a complete prepared graph through the same native destination and copy
// options used for individual native-storage images.
func copyImageDestination(ctx context.Context, layout string, root v1.Descriptor, destination Destination, opts Options, policy *signature.Policy) (_ string, retErr error) {
	if needsSequoiaHelper(opts) {
		return transferWithSequoiaHelper(ctx, "copy-destination", layout, root, destination, opts, policy != nil, imagecopy.CopyAllImages)
	}

	src, err := imagestore.LayoutReference(layout, root)
	if err != nil {
		return "", err
	}
	var dst types.ImageReference
	if destination.Transport == "containers-storage" {
		store, err := nativeStoreOptions(opts)
		if err != nil {
			return "", err
		}
		err = buildah.WithStore(store, func(backend storage.Store) error {
			var err error
			if strings.HasPrefix(destination.Name, "[") {
				dst, err = alltransports.ParseImageName(destination.String())
			} else {
				dst, err = imagestorage.Transport.ParseStoreReference(backend, destination.Name)
			}
			return err
		})
		if err != nil {
			return "", err
		}
	} else {
		dst, err = alltransports.ParseImageName(destination.String())
		if err != nil {
			return "", err
		}
	}
	auth, cert, err := oci.NormalizeRegistryPaths(opts.AuthFile, opts.CertDir)
	if err != nil {
		return "", err
	}
	system, err := signedTransferSystemContext(auth, cert, "", opts)
	if err != nil {
		return "", err
	}
	system.BigFilesTemporaryDir = os.TempDir()
	if policy == nil {
		policy, err = signature.DefaultPolicy(system)
		if err != nil {
			return "", err
		}
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, policyContext.Destroy()) }()
	sigstore, gpg, err := opts.Signing.passphrases()
	if err != nil {
		return "", err
	}
	signers, cleanupSigners, err := buildah.NativeSigners(opts.Signing.SigstoreParameterFile, opts.Signing.SequoiaFingerprint, sigstore, gpg, opts.signingInteractionOutput())
	if err != nil {
		return "", err
	}
	defer cleanupSigners()
	options := &imagecopy.Options{Signers: signers, SourceCtx: system, DestinationCtx: system, ReportWriter: opts.ProgressWriter, ImageListSelection: imagecopy.CopyAllImages, SignBy: opts.Signing.SignBy, SignPassphrase: gpg, SignBySigstorePrivateKeyFile: opts.Signing.SigstorePrivateKeyFile, SignSigstorePrivateKeyPassphrase: sigstore}
	if err := opts.Push.Apply(options); err != nil {
		return "", err
	}
	retries, err := nativeRetryOptions(opts)
	if err != nil {
		return "", err
	}
	var copied []byte
	err = retry.IfNecessary(ctx, func() error {
		var err error
		copied, err = imagecopy.Image(ctx, policyContext, dst, src, options)
		return err
	}, retries)
	if err != nil {
		return "", err
	}
	return destination.String() + "@" + digest.FromBytes(copied).String(), nil
}
