package buildah

import (
	"context"
	"errors"
	"os"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage"
	imagecopy "go.podman.io/image/v5/copy"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/storage"
)

// Native artifact references consult the global storage transport even when the
// reference names a store. Bind it only in the isolated transfer worker and
// restore it before the worker's store lease closes.
func pushStoredManifestList(ctx context.Context, backend storage.Store, options StoredTransferOptions) (_ StoredTransferResult, retErr error) {
	previous := imagestorage.Transport.GetStoreIfSet()
	imagestorage.Transport.SetStore(backend)
	defer imagestorage.Transport.SetStore(previous)
	system, err := storedTransferSystemContext(options, "")
	if err != nil {
		return StoredTransferResult{}, err
	}
	destination, err := alltransports.ParseImageName(options.Destination)
	if err != nil {
		return StoredTransferResult{}, err
	}
	if destination.Transport().Name() == "docker" && (options.SigstorePrivateKeyFile != "" || options.SigstoreParameterFile != "") {
		directory, err := SigstoreAttachmentsConfig(system, destination)
		if err != nil {
			return StoredTransferResult{}, err
		}
		defer func() { retErr = errors.Join(retErr, os.RemoveAll(directory)) }()
		system.RegistriesDirPath = directory
	}
	sigstorePassphrase, gpgPassphrase, err := storedTransferPassphrases(options)
	if err != nil {
		return StoredTransferResult{}, err
	}
	signers, cleanup, err := NativeSigners(options.SigstoreParameterFile, options.SequoiaFingerprint, sigstorePassphrase, gpgPassphrase, os.Stdout)
	if err != nil {
		return StoredTransferResult{}, err
	}
	defer cleanup()
	copyOptions := &imagecopy.Options{DestinationCtx: system}
	if err := options.Push.Apply(copyOptions); err != nil {
		return StoredTransferResult{}, err
	}
	selection := imagecopy.CopySpecificImages
	if options.ManifestListAll {
		selection = imagecopy.CopyAllImages
	}
	pushOptions := &libimage.ManifestListPushOptions{ImageListSelection: selection, AddCompression: options.Push.AddCompression, CopyOptions: libimage.CopyOptions{
		Writer: options.ProgressWriter, SignaturePolicyPath: options.SignaturePolicyPath,
		ManifestMIMEType: copyOptions.ForceManifestMIMEType, CompressionFormat: system.CompressionFormat, CompressionLevel: system.CompressionLevel,
		ForceCompressionFormat: copyOptions.ForceCompressionFormat, RemoveSignatures: copyOptions.RemoveSignatures,
		Signers: signers, SignBy: options.SignBy, SignPassphrase: gpgPassphrase,
		SignBySigstorePrivateKeyFile: options.SigstorePrivateKeyFile, SignSigstorePrivateKeyPassphrase: sigstorePassphrase,
	}}
	if options.RetrySet {
		pushOptions.MaxRetries = &options.Retry
	}
	if options.RetryDelay != 0 {
		pushOptions.RetryDelay = &options.RetryDelay
	}
	native, err := libimage.RuntimeFromStore(backend, &libimage.RuntimeOptions{SystemContext: system})
	if err != nil {
		return StoredTransferResult{}, err
	}
	list, err := native.LookupManifestList(options.ManifestList)
	if err != nil {
		return StoredTransferResult{}, err
	}
	pushed, err := list.Push(ctx, options.Destination, pushOptions)
	if err != nil {
		return StoredTransferResult{}, err
	}
	return StoredTransferResult{Manifest: v1.Descriptor{Digest: pushed}}, nil
}
