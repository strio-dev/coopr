//go:build !linux

package buildah

import "github.com/opencontainers/go-digest"

func simpleLocalCopyDigestCandidates(_ string, _ []string, _ Copy) ([]digest.Digest, bool) {
	return nil, false
}
