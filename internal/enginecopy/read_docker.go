package enginecopy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"coopr/internal/localstore"
	"coopr/internal/oci"
	"github.com/containerd/platforms"
	dockerclient "github.com/moby/moby/client"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// Modern Docker's save API includes the original OCI graph. The older
// docker-daemon transport reads Docker's single-image archive representation,
// so using it for an index would lose platform manifests and their digests.
func readDockerOCI(ctx context.Context, name, layout string, platform v1.Platform) (_ v1.Descriptor, handled bool, retErr error) {
	client, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		return v1.Descriptor{}, true, err
	}
	defer func() { retErr = errors.Join(retErr, client.Close()) }()
	info, err := client.Info(ctx, dockerclient.InfoOptions{})
	if err != nil {
		return v1.Descriptor{}, true, fmt.Errorf("inspect Docker image store: %w", err)
	}
	if !dockerUsesContainerdImageStore(info.Info.DriverStatus) {
		return v1.Descriptor{}, false, nil
	}
	stream, err := client.ImageSave(ctx, []string{name})
	if err != nil {
		return v1.Descriptor{}, true, fmt.Errorf("read Docker image %s: %w", name, err)
	}
	defer func() { retErr = errors.Join(retErr, stream.Close()) }()
	file, err := os.CreateTemp("", "coopr-docker-import-*.tar")
	if err != nil {
		return v1.Descriptor{}, true, err
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	if _, err := io.Copy(file, stream); err != nil {
		return v1.Descriptor{}, true, err
	}
	if err := file.Close(); err != nil {
		return v1.Descriptor{}, true, err
	}
	root, err := oci.ArchiveRoot(file.Name())
	if err != nil {
		return v1.Descriptor{}, true, fmt.Errorf("read saved Docker OCI index: %w", err)
	}
	source, err := orasoci.NewFromTar(ctx, file.Name())
	if err != nil {
		return v1.Descriptor{}, true, err
	}
	if platform.Architecture != "" {
		root, err = selectSavedPlatform(ctx, source, root, platform)
		if err != nil {
			return v1.Descriptor{}, true, err
		}
	}
	abs, err := filepath.Abs(layout)
	if err != nil {
		return v1.Descriptor{}, true, err
	}
	if err := localstore.CopyGraphLayout(ctx, abs, source, root); err != nil {
		return v1.Descriptor{}, true, err
	}
	return root, true, nil
}

func selectSavedPlatform(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor, platform v1.Platform) (v1.Descriptor, error) {
	if root.MediaType != v1.MediaTypeImageIndex && root.MediaType != "application/vnd.docker.distribution.manifest.list.v2+json" {
		return root, nil
	}
	data, err := content.FetchAll(ctx, source, root)
	if err != nil {
		return v1.Descriptor{}, err
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return v1.Descriptor{}, err
	}
	for _, manifest := range index.Manifests {
		if manifest.Platform != nil && platforms.Only(platform).Match(*manifest.Platform) {
			return manifest, nil
		}
	}
	return v1.Descriptor{}, fmt.Errorf("docker image has no available platform %s", platforms.Format(platform))
}
