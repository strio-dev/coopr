package transfer

import (
	"context"
	"errors"
	"io"
	"os"

	"coopr/internal/buildah"
	"coopr/internal/oci"
	"coopr/internal/storeactivity"

	"github.com/opencontainers/go-digest"
	"go.podman.io/common/libimage"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/storage"
)

// PushManifestList uses the native manifest instance references, including remote
// members, and the same signing credentials as ordinary Coopr transfers.
func PushManifestList(ctx context.Context, name string, destination Destination, opts Options, all bool, writer io.Writer) (result digest.Digest, retErr error) {
	if err := ValidateSigningDestination(oci.Image, destination, opts.Signing); err != nil {
		return "", err
	}
	store, err := nativeStoreOptions(opts)
	if err != nil {
		return "", err
	}
	lease, err := storeactivity.AcquireShared(ctx, buildah.ActivityRoots(store, "")...)
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, lease.Close()) }()
	auth, cert, err := oci.NormalizeRegistryPaths(opts.AuthFile, opts.CertDir)
	if err != nil {
		return "", err
	}
	registriesDir := ""
	if opts.Signing.SigstorePrivateKeyFile != "" {
		registriesDir, err = sigstoreAttachmentsConfig()
		if err != nil {
			return "", err
		}
		defer func() { retErr = errors.Join(retErr, os.RemoveAll(registriesDir)) }()
	}
	system, err := signedTransferSystemContext(auth, cert, registriesDir, opts)
	if err != nil {
		return "", err
	}
	sigstorePassphrase, gpgPassphrase, err := opts.Signing.passphrases()
	if err != nil {
		return "", err
	}
	selection := imagecopy.CopySpecificImages
	if all {
		selection = imagecopy.CopyAllImages
	}
	options := &libimage.ManifestListPushOptions{ImageListSelection: selection, CopyOptions: libimage.CopyOptions{
		Writer: writer, SignaturePolicyPath: opts.SignaturePolicyPath,
		SignBy: opts.Signing.SignBy, SignPassphrase: gpgPassphrase,
		SignBySigstorePrivateKeyFile: opts.Signing.SigstorePrivateKeyFile, SignSigstorePrivateKeyPassphrase: sigstorePassphrase,
	}}
	if opts.RetrySet {
		options.MaxRetries = &opts.Retry
	}
	if opts.RetryDelay != 0 {
		options.RetryDelay = &opts.RetryDelay
	}
	retErr = buildah.WithStore(store, func(backend storage.Store) error {
		native, err := libimage.RuntimeFromStore(backend, &libimage.RuntimeOptions{SystemContext: system})
		if err != nil {
			return err
		}
		list, err := native.LookupManifestList(name)
		if err != nil {
			return err
		}
		result, err = list.Push(ctx, "docker://"+destination.Name, options)
		return err
	})
	return result, retErr
}
