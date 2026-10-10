package transfer

import (
	"context"
	"errors"
	"fmt"
	"os"

	"coopr/internal/imagestore"
	"coopr/internal/localstore"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/image"
	"go.podman.io/image/v5/manifest"
	archive "go.podman.io/image/v5/oci/archive"
	"go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/types"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func copyImageArchive(ctx context.Context, layoutPath string, root v1.Descriptor, destination Destination, opts Options, policy *signature.Policy) (_ string, retErr error) {
	source, err := imagestore.LayoutReference(layoutPath, root)
	if err != nil {
		return "", err
	}
	system := &types.SystemContext{SignaturePolicyPath: opts.SignaturePolicyPath, BigFilesTemporaryDir: os.TempDir(), OCIAcceptUncompressedLayers: true}
	if opts.ArchiveUncompressed != nil {
		system.OCIAcceptUncompressedLayers = *opts.ArchiveUncompressed
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
	if opts.ArchiveUncompressed == nil || *opts.ArchiveUncompressed {
		store, err := orasoci.NewWithContext(ctx, layoutPath)
		if err != nil {
			return "", err
		}
		exact, err := archiveNeedsExactGraph(ctx, store, root)
		if err != nil {
			return "", err
		}
		if exact {
			imageSource, err := source.NewImageSource(ctx, system)
			if err != nil {
				return "", err
			}
			policyErr := approveArchiveGraph(ctx, store, imageSource, root, policyContext)
			if err := errors.Join(policyErr, imageSource.Close()); err != nil {
				return "", err
			}
			// The native OCI destination converts Docker lists to OCI indexes,
			// which cannot preserve the selected rooted graph's digests.
			if opts.ProgressWriter != nil {
				if _, err := fmt.Fprintln(opts.ProgressWriter, "Writing manifest graph to OCI destination"); err != nil {
					return "", err
				}
			}
			if destination.Transport == "oci-dir" {
				err = localstore.CopyGraphLayout(ctx, destination.Name, store, root)
			} else {
				err = localstore.WriteArchive(ctx, store, root, destination.Name)
			}
			return destination.Name, err
		}
	}
	var target types.ImageReference
	if destination.Transport == "oci-dir" {
		target, err = layout.NewReference(destination.Name, opts.ArchiveReference)
	} else {
		target, err = archive.NewReference(destination.Name, opts.ArchiveReference)
	}
	if err != nil {
		return "", err
	}
	_, err = imagecopy.Image(ctx, policyContext, target, source, &imagecopy.Options{SourceCtx: system, DestinationCtx: system, PreserveDigests: system.OCIAcceptUncompressedLayers, ImageListSelection: imagecopy.CopyAllImages, ReportWriter: opts.ProgressWriter})
	return destination.Name, err
}

// Native copying evaluates the policy for each image instance. Keep that same
// boundary when preserving a list that the native OCI destination would convert.
func approveArchiveGraph(ctx context.Context, graph content.Fetcher, source types.ImageSource, root v1.Descriptor, policy *signature.PolicyContext) error {
	pending := []v1.Descriptor{root}
	visited := make(map[digest.Digest]bool)
	for len(pending) > 0 {
		descriptor := pending[0]
		pending = pending[1:]
		if visited[descriptor.Digest] {
			continue
		}
		visited[descriptor.Digest] = true
		if manifest.MIMETypeIsMultiImage(descriptor.MediaType) {
			children, err := content.Successors(ctx, graph, descriptor)
			if err != nil {
				return err
			}
			pending = append(pending, children...)
			continue
		}
		switch descriptor.MediaType {
		case v1.MediaTypeImageManifest, manifest.DockerV2Schema2MediaType, manifest.DockerV2Schema1MediaType, manifest.DockerV2Schema1SignedMediaType:
			allowed, err := policy.IsRunningImageAllowed(ctx, image.UnparsedInstance(source, &descriptor.Digest))
			if err != nil {
				return err
			}
			if !allowed {
				return fmt.Errorf("signature policy rejects image %s", descriptor.Digest)
			}
		}
	}
	return nil
}

// Ordinary Docker manifests are copied unchanged by the native library. Docker
// lists require conversion, including when nested below an OCI index.
func archiveNeedsExactGraph(ctx context.Context, source content.Fetcher, root v1.Descriptor) (bool, error) {
	if root.MediaType == manifest.DockerV2ListMediaType {
		return true, nil
	}
	if root.MediaType != v1.MediaTypeImageIndex {
		return false, nil
	}
	children, err := content.Successors(ctx, source, root)
	if err != nil {
		return false, err
	}
	for _, child := range children {
		exact, err := archiveNeedsExactGraph(ctx, source, child)
		if exact || err != nil {
			return exact, err
		}
	}
	return false, nil
}
