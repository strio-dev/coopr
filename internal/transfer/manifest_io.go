package transfer

import (
	"context"
	"errors"
	"io"

	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/storeactivity"

	"github.com/opencontainers/go-digest"
)

// PushManifestList keeps native manifest references and artifact bookkeeping in
// the existing isolated storage worker. The parent owns the activity lease for
// the complete worker lifetime, including native copy and store cleanup.
func PushManifestList(ctx context.Context, name string, destination Destination, opts Options, all bool, writer io.Writer) (result digest.Digest, retErr error) {
	if err := ValidateSigningDestination(oci.Image, destination, opts.Signing); err != nil {
		return "", err
	}
	options, cleanup, err := storedTransferOptions("", "", opts)
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, cleanup()) }()
	options.ManifestList, options.ManifestListAll = name, all
	options.Destination = destination.String()
	options.ProgressWriter = writer
	lease, err := storeactivity.AcquireShared(ctx, buildah.ActivityRoots(options.Store, "")...)
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, lease.Close()) }()
	pushed, err := buildah.TransferStoredImageResultSupervised(ctx, options)
	if err != nil {
		return "", err
	}
	return pushed.Manifest.Digest, nil
}
