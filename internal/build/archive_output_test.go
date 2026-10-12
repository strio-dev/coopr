package build

import (
	"context"
	"testing"

	"coopr/internal/oci"
	"coopr/internal/transfer"
)

// buildArchive exercises the public workflow: build into native storage, then
// copy the completed image to an OCI archive. Tags remain image names.
func buildArchive(t *testing.T, archive string, ctx context.Context, opts Options) (Result, error) {
	t.Helper()
	source := opts.Tag
	if source == "" && len(opts.Tags) != 0 {
		source = opts.Tags[0]
	}
	if source == "" {
		source = "coopr-test-export:latest"
		opts.Tag = source
	}
	result, err := Run(ctx, opts)
	if err != nil {
		return result, err
	}
	_, err = transfer.Copy(ctx, oci.Image, source, transfer.Destination{Transport: "oci-archive", Name: archive}, transfer.Options{BuildStore: opts.BuildStore, TLSVerify: opts.TLSVerify})
	return result, err
}
