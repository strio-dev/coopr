package buildah

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/opencontainers/go-digest"
	upstream "go.podman.io/buildah"
)

type copyAddDigestObservation func(dryRun bool)

var copyAddDigestObserver atomic.Pointer[copyAddDigestObservation]

type simpleCopyCacheCandidate struct {
	key   digest.Digest
	input digest.Digest
	entry instructionCacheEntry
}

// uniqueSimpleCopyCacheCandidate distinguishes a unique reusable cache entry
// from a miss and from an ambiguous match.  Ambiguity requires Buildah's
// authoritative DryRun because destination state decides the actual tar name.
func uniqueSimpleCopyCacheCandidate(candidates []simpleCopyCacheCandidate) (*simpleCopyCacheCandidate, bool) {
	if len(candidates) == 1 {
		return &candidates[0], false
	}
	return nil, len(candidates) > 1
}

// copyAddProbeEligible reports whether Buildah can compute the exact input
// stream digest without introducing work or state that makes a preflight more
// expensive or observably different from the real operation. Remote ADD would
// fetch its source twice on a miss. Buildah's --link path records a linked
// layer even with DryRun, so linked operations retain the apply-first path.
func copyAddProbeEligible(operation Operation) bool {
	switch copied := operation.(type) {
	case Copy:
		return !copied.Link
	case Add:
		if copied.Link {
			return false
		}
		for _, source := range copied.Sources {
			if isHTTPAddSource(source) || isGitAddSource(source) {
				return false
			}
		}
		return true
	case copyFromImageOperation:
		return !copied.options.Link
	default:
		return false
	}
}

// pinnedHTTPAddInputDigest returns the authored content identity for a remote
// HTTP ADD. The response mtime affects the copied file but, as with other ADD
// sources, does not participate in the cache key.
func pinnedHTTPAddInputDigest(operation Operation) (digest.Digest, bool, error) {
	added, ok := operation.(Add)
	if !ok || len(added.Sources) != 1 || added.Checksum == "" ||
		!isHTTPAddSource(added.Sources[0]) || isGitAddSource(added.Sources[0]) {
		return "", false, nil
	}
	expected, err := digest.Parse(added.Checksum)
	if err != nil {
		return "", false, fmt.Errorf("invalid ADD checksum: %w", err)
	}
	if expected.Algorithm() != digest.SHA256 {
		return "", false, fmt.Errorf("ADD remote checksum must use sha256, got %s", expected.Algorithm())
	}
	return expected, true, nil
}

// probeCopyAddDigest computes the exact stream digest Buildah would consume
// without writing file payloads into the destination rootfs.
func probeCopyAddDigest(builder operationBuilder, contextDir string, artifacts []string, operation Operation) (digest.Digest, error) {
	return probeCopyAddDigestContext(context.Background(), builder, contextDir, artifacts, operation)
}

func probeCopyAddDigestContext(ctx context.Context, builder operationBuilder, contextDir string, artifacts []string, operation Operation) (digest.Digest, error) {
	if !copyAddProbeEligible(operation) {
		return "", fmt.Errorf("operation %T is not eligible for COPY/ADD cache probing", operation)
	}
	return copyAddWithDigest(ctx, builder, contextDir, artifacts, operation, true)
}

// applyCopyAddWithDigest applies a lowered COPY or ADD exactly once and
// returns the digest of the content stream that Buildah consumed.
func applyCopyAddWithDigest(builder operationBuilder, contextDir string, artifacts []string, operation Operation) (digest.Digest, error) {
	return applyCopyAddWithDigestContext(context.Background(), builder, contextDir, artifacts, operation)
}

func applyCopyAddWithDigestContext(ctx context.Context, builder operationBuilder, contextDir string, artifacts []string, operation Operation) (digest.Digest, error) {
	return copyAddWithDigest(ctx, builder, contextDir, artifacts, operation, false)
}

func copyAddWithDigest(ctx context.Context, builder operationBuilder, contextDir string, artifacts []string, operation Operation, dryRun bool) (digest.Digest, error) {
	if observer := copyAddDigestObserver.Load(); observer != nil {
		(*observer)(dryRun)
	}
	digester := digest.SHA256.Digester()

	switch copied := operation.(type) {
	case Copy:
		if len(copied.Sources) == 0 && len(copied.InlineFiles) == 0 || copied.Destination == "" {
			return "", errors.New("COPY requires at least one source and a destination")
		}
		if len(copied.Sources) != 0 {
			if err := requireLocalCopySources(copied.Sources); err != nil {
				return "", err
			}
			policy, err := prepareContextPolicyWithIgnore(contextDir, artifacts, copied.IgnoreFile)
			if err != nil {
				return "", err
			}
			options, sources, err := policy.applyLocalCopy(upstream.AddAndCopyOptions{Chown: copied.Chown, Chmod: copied.Chmod, Link: copied.Link, Parents: copied.Parents, Excludes: slices.Clone(copied.Excludes), Hasher: digester.Hash(), DryRun: dryRun}, copied.Sources)
			if err != nil {
				return "", err
			}
			if err := builder.add(copied.Destination, false, options, sources...); err != nil {
				return "", err
			}
		}
		for _, source := range copied.InlineFiles {
			if err := addInlineData(builder, copied.Destination, source, upstream.AddAndCopyOptions{Chown: copied.Chown, Chmod: copied.Chmod, Link: copied.Link, Excludes: slices.Clone(copied.Excludes), Hasher: digester.Hash(), DryRun: dryRun}); err != nil {
				return "", err
			}
		}
	case Add:
		if len(copied.Sources) == 0 && len(copied.InlineFiles) == 0 || copied.Destination == "" {
			return "", errors.New("ADD requires at least one source and a destination")
		}
		if len(copied.Sources) != 0 {
			policy, err := prepareContextPolicyWithIgnore(contextDir, artifacts, copied.IgnoreFile)
			if err != nil {
				return "", err
			}
			if err := applyAddSources(ctx, builder, policy, copied, upstream.AddAndCopyOptions{
				Chown: copied.Chown, Chmod: copied.Chmod, Checksum: copied.Checksum,
				Link: copied.Link, Parents: copied.Parents, Excludes: slices.Clone(copied.Excludes),
				Hasher: digester.Hash(), DryRun: dryRun,
			}); err != nil {
				return "", err
			}
		}
		for _, source := range copied.InlineFiles {
			if err := addInlineData(builder, copied.Destination, source, upstream.AddAndCopyOptions{Chown: copied.Chown, Chmod: copied.Chmod, Link: copied.Link, Excludes: slices.Clone(copied.Excludes), Hasher: digester.Hash(), DryRun: dryRun}); err != nil {
				return "", err
			}
		}
	case copyFromImageOperation:
		native, ok := builder.(nativeBuilder)
		if !ok {
			return "", errors.New("COPY --from requires a native Buildah builder")
		}
		options := copied.options
		options.Excludes = slices.Clone(copied.options.Excludes)
		options.Hasher = digester.Hash()
		options.DryRun = dryRun
		if len(copied.sources) != 0 {
			if err := CopyFromImage(copied.store, copied.imageID, native.Builder, copied.destination, copied.extract, options, copied.sources...); err != nil {
				return "", err
			}
		}
		for _, source := range copied.inlineFiles {
			if err := addInlineData(builder, copied.destination, source, inlineCopyOptions(options)); err != nil {
				return "", err
			}
		}
	default:
		return "", fmt.Errorf("operation %T is not COPY or ADD", operation)
	}

	return digester.Digest(), nil
}
