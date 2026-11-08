package buildah

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/opencontainers/go-digest"
	upstream "go.podman.io/buildah"
)

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

// applyCopyAddWithDigestContext applies a lowered COPY or ADD exactly once and
// returns the digest of the content stream that Buildah consumed.
func applyCopyAddWithDigestContext(ctx context.Context, builder operationBuilder, contextDir string, artifacts []string, operation Operation) (digest.Digest, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	digester := digest.SHA256.Digester()
	hasher := digester.Hash()
	native, nativeDigest := builder.(nativeBuilder)
	nativeDigest = nativeDigest && native.Builder != nil
	if added, ok := operation.(Add); ok {
		for _, source := range added.Sources {
			// Remote ADD has an explicit HTTP checksum / verified Git commit
			// identity. Do not hash generated checkout metadata instead.
			if isHTTPAddSource(source) || isGitAddSource(source) {
				nativeDigest = false
			}
		}
	}
	if native.Builder != nil {
		previous := native.ContentDigester
		native.ContentDigester = upstream.CompositeDigester{}
		defer func() {
			native.ContentDigester.Restart()
			native.ContentDigester = previous
		}()
		// Native Add records normalized tar content, including modes/xattrs
		// and excluding timestamps, in its CompositeDigester automatically.
		if nativeDigest {
			hasher = nil
		}
	}

	switch copied := operation.(type) {
	case Copy:
		var policy contextPolicy
		if len(copied.Sources) != 0 {
			var err error
			policy, err = prepareContextPolicyWithIgnore(contextDir, artifacts, copied.IgnoreFile)
			if err != nil {
				return "", err
			}
		}
		if err := copied.applyWithPolicy(builder, policy, hasher); err != nil {
			return "", err
		}
	case Add:
		var policy contextPolicy
		if len(copied.Sources) != 0 {
			var err error
			policy, err = prepareContextPolicyWithIgnore(contextDir, artifacts, copied.IgnoreFile)
			if err != nil {
				return "", err
			}
		}
		if err := copied.applyWithPolicy(ctx, builder, policy, hasher); err != nil {
			return "", err
		}
	case copyFromImageOperation:
		native, ok := builder.(nativeBuilder)
		if !ok {
			return "", errors.New("COPY --from requires a native Buildah builder")
		}
		options := copied.options
		options.Excludes = slices.Clone(copied.options.Excludes)
		options.Hasher = hasher
		options.DryRun = false
		if err := addInlineData(builder, copied.destination, copied.inlineFiles, inlineCopyOptions(options)); err != nil {
			return "", err
		}
		if len(copied.sources) != 0 {
			if err := copied.copyFiles(ctx, native.Builder, options); err != nil {
				return "", err
			}
		}
	default:
		return "", fmt.Errorf("operation %T is not COPY or ADD", operation)
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}
	if nativeDigest {
		kind, input := native.ContentDigester.Digest()
		if input == "" {
			return digest.FromString(kind), nil
		}
		// Match the native content summary framing; file and directory inputs
		// remain distinct even when their tar payload happens to match.
		if kind != "" {
			return digest.FromString(kind + ":" + input.Encoded()), nil
		}
		return input, nil
	}
	return digester.Digest(), nil
}
