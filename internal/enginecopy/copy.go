// Package enginecopy delivers Coopr image indexes to Docker Engine.
package enginecopy

import (
	"context"
	"fmt"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	dockerreference "go.podman.io/image/v5/docker/reference"
)

// CopyIndex transfers a complete index from a Coopr OCI layout into Docker's
// containerd image store and verifies that the engine preserved its descriptors.
// name is the unprefixed image name.
func CopyIndex(ctx context.Context, layout string, root v1.Descriptor, name string) (string, error) {
	if root.MediaType != v1.MediaTypeImageIndex && root.MediaType != "application/vnd.docker.distribution.manifest.list.v2+json" {
		return "", fmt.Errorf("docker index copy requires an image index, got %q", root.MediaType)
	}
	named, err := dockerreference.ParseNormalizedNamed(name)
	if err != nil {
		return "", fmt.Errorf("invalid Docker image name %q: %w", name, err)
	}
	if _, ok := named.(dockerreference.Digested); ok {
		return "", fmt.Errorf("docker image name %q must not include a digest", name)
	}
	named = dockerreference.TagNameOnly(named)
	if err := copyDockerIndex(ctx, layout, root, named); err != nil {
		return "", err
	}
	return "docker-daemon:" + dockerreference.FamiliarString(named), nil
}
