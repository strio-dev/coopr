// Package enginecopy transfers Coopr images to and from Docker Engine.
package enginecopy

import (
	"context"
	"fmt"
	"os"

	"coopr/internal/imagestore"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	dockerdaemon "go.podman.io/image/v5/docker/daemon"
	dockerreference "go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/types"
)

// Copy transfers root from a Coopr OCI layout into an explicit engine store.
// Engine must be docker; name is the unprefixed image name.
func Copy(ctx context.Context, layout string, root v1.Descriptor, engine, name string) (string, error) {
	if engine != "docker" {
		return "", fmt.Errorf("unsupported image engine %q", engine)
	}
	result, err := copyDocker(ctx, layout, root, name)
	if err != nil {
		return "", err
	}
	return engine + ":" + result, nil
}

func copyDocker(ctx context.Context, layout string, root v1.Descriptor, name string) (string, error) {
	named, err := dockerreference.ParseNormalizedNamed(name)
	if err != nil {
		return "", fmt.Errorf("invalid Docker image name %q: %w", name, err)
	}
	if _, ok := named.(dockerreference.Digested); ok {
		return "", fmt.Errorf("docker image name %q must not include a digest", name)
	}
	named = dockerreference.TagNameOnly(named)
	if root.MediaType == v1.MediaTypeImageIndex || root.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json" {
		if err := copyDockerIndex(ctx, layout, root, named); err != nil {
			return "", err
		}
		return dockerreference.FamiliarString(named), nil
	}
	src, err := imagestore.LayoutReference(layout, root)
	if err != nil {
		return "", err
	}
	dst, err := dockerdaemon.NewReference("", named)
	if err != nil {
		return "", fmt.Errorf("docker image destination: %w", err)
	}
	// docker-daemon stores Docker schema-2 manifests, so an OCI source may be
	// converted and cannot promise preservation of the manifest digest.
	system := &types.SystemContext{DockerDaemonHost: os.Getenv("DOCKER_HOST")}
	if err := imagestore.Copy(ctx, system, src, dst, false); err != nil {
		return "", fmt.Errorf("copy image to Docker: %w", err)
	}
	return dst.StringWithinTransport(), nil
}
