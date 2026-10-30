package oci

import (
	"errors"
	"fmt"
	"os"
	"strings"

	buildahutil "go.podman.io/buildah/pkg/util"
	"go.podman.io/common/pkg/auth"
)

// MaterializeRegistryAuthFile makes descriptor credentials reusable across native
// subprocesses. The caller owns the returned file only when owned is true.
// Ordinary paths, including an omitted implicit default, remain unchanged.
func MaterializeRegistryAuthFile(input string) (path string, owned bool, err error) {
	path = input
	if path == "" {
		path = auth.GetDefaultAuthFile()
	}
	if !strings.HasPrefix(path, "/dev/fd/") {
		return input, false, nil
	}

	materialized, created := buildahutil.MirrorToTempFileIfPathIsDescriptor(path)
	if !created {
		return "", false, fmt.Errorf("materialize registry auth file %q: native descriptor mirroring failed", path)
	}
	if err := validateCredentialFile(materialized); err != nil {
		return "", false, errors.Join(err, os.Remove(materialized))
	}
	return materialized, true, nil
}
