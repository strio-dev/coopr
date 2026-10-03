package enginecopy

import (
	"context"
	"errors"
	"fmt"
	"os"

	"coopr/internal/imagestore"
	"coopr/internal/oci"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	imagecopy "go.podman.io/image/v5/copy"
	dockerdaemon "go.podman.io/image/v5/docker/daemon"
	ocilayout "go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/types"
)

// Read copies an existing engine image into a caller-owned OCI layout. It uses
// the same library transports as image output; it never invokes an engine CLI.
func Read(ctx context.Context, engine, name, layout string, platform v1.Platform) (_ v1.Descriptor, retErr error) {
	switch engine {
	case "podman":
		store, err := imagestore.New()
		if err != nil {
			return v1.Descriptor{}, err
		}
		defer func() { retErr = errors.Join(retErr, store.Close()) }()
		return store.ReadLayout(ctx, name, layout, platform)
	case "docker":
		if root, handled, err := readDockerOCI(ctx, name, layout, platform); handled || err != nil {
			return root, err
		}
		source, err := dockerdaemon.ParseReference(name)
		if err != nil {
			return v1.Descriptor{}, fmt.Errorf("docker image source: %w", err)
		}
		target, err := ocilayout.NewReference(layout, "")
		if err != nil {
			return v1.Descriptor{}, err
		}
		system := &types.SystemContext{DockerDaemonHost: os.Getenv("DOCKER_HOST")}
		selection := imagecopy.CopyAllImages
		if platform.Architecture != "" {
			system.OSChoice, system.ArchitectureChoice, system.VariantChoice = platform.OS, platform.Architecture, platform.Variant
			selection = imagecopy.CopySystemImage
		}
		if err := imagestore.CopyImages(ctx, system, source, target, false, selection); err != nil {
			return v1.Descriptor{}, fmt.Errorf("read Docker image %s: %w", name, err)
		}
		return oci.LayoutRoot(layout)
	default:
		return v1.Descriptor{}, fmt.Errorf("unsupported image engine %q", engine)
	}
}
