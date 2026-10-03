package enginecopy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"

	"coopr/internal/imagestore"
	"coopr/internal/localstore"
	dockerclient "github.com/moby/moby/client"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	dockerreference "go.podman.io/image/v5/docker/reference"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

const dockerImageNameAnnotation = "io.containerd.image.name"

func copyDockerIndex(ctx context.Context, layout string, root v1.Descriptor, named dockerreference.Named) (retErr error) {
	// Require the exact descriptor to be anchored by the source layout before
	// opening any connection to the engine. WriteArchiveTo verifies every blob
	// in the rooted graph while it streams the archive.
	if _, err := imagestore.LayoutReference(layout, root); err != nil {
		return err
	}
	abs, err := filepath.Abs(layout)
	if err != nil {
		return fmt.Errorf("resolve OCI layout path: %w", err)
	}
	source, err := orasoci.NewWithContext(ctx, abs)
	if err != nil {
		return fmt.Errorf("open OCI layout: %w", err)
	}
	expected, err := indexPlatformManifests(ctx, source, root)
	if err != nil {
		return fmt.Errorf("read OCI image index: %w", err)
	}

	client, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		return fmt.Errorf("create Docker client: %w", err)
	}
	defer func() {
		if err := client.Close(); retErr == nil && err != nil {
			retErr = fmt.Errorf("close Docker client: %w", err)
		}
	}()
	info, err := client.Info(ctx, dockerclient.InfoOptions{})
	if err != nil {
		return fmt.Errorf("inspect Docker image store: %w", err)
	}
	if !dockerUsesContainerdImageStore(info.Info.DriverStatus) {
		return errors.New("docker daemon uses the classic image store, which cannot load a multi-platform index; enable the containerd image store, publish to a registry, or copy one platform")
	}

	fullyQualified := named.String()
	tagged, ok := named.(dockerreference.Tagged)
	if !ok {
		return fmt.Errorf("docker image name %q has no tag", fullyQualified)
	}
	archiveRoot := root
	archiveRoot.Annotations = maps.Clone(root.Annotations)
	if archiveRoot.Annotations == nil {
		archiveRoot.Annotations = make(map[string]string, 2)
	}
	archiveRoot.Annotations[dockerImageNameAnnotation] = fullyQualified
	archiveRoot.Annotations[v1.AnnotationRefName] = tagged.Tag()

	if err := loadDockerOCIArchive(ctx, client, source, archiveRoot); err != nil {
		return err
	}
	inspect, err := client.ImageInspect(ctx, fullyQualified, dockerclient.ImageInspectWithManifests(true))
	if err != nil {
		return fmt.Errorf("inspect loaded Docker image %q: %w", fullyQualified, err)
	}
	if inspect.Descriptor == nil || !sameContentDescriptor(*inspect.Descriptor, root) {
		return fmt.Errorf("docker loaded image %q with descriptor %v, want %+v", fullyQualified, inspect.Descriptor, root)
	}
	available := make(map[digest.Digest]v1.Descriptor, len(inspect.Manifests))
	for _, manifest := range inspect.Manifests {
		if manifest.Available {
			available[manifest.Descriptor.Digest] = manifest.Descriptor
		}
	}
	for _, want := range expected {
		if got, ok := available[want.Digest]; !ok || !sameContentDescriptor(got, want) {
			return fmt.Errorf("docker loaded image %q without expected available platform manifest %+v", fullyQualified, want)
		}
	}
	return nil
}

func sameContentDescriptor(got, want v1.Descriptor) bool {
	return got.Digest == want.Digest && got.MediaType == want.MediaType && got.Size == want.Size
}

func dockerUsesContainerdImageStore(status [][2]string) bool {
	for _, pair := range status {
		if pair[0] == "driver-type" && pair[1] == "io.containerd.snapshotter.v1" {
			return true
		}
	}
	return false
}

func indexPlatformManifests(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor) ([]v1.Descriptor, error) {
	stream, err := source.Fetch(ctx, root)
	if err != nil {
		return nil, err
	}
	data, readErr := content.ReadAll(stream, root)
	closeErr := stream.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, err
	}
	wants := make([]v1.Descriptor, 0, len(index.Manifests))
	for _, manifest := range index.Manifests {
		if manifest.Platform != nil {
			wants = append(wants, manifest)
		}
	}
	if len(wants) == 0 {
		return nil, errors.New("image index contains no platform manifests")
	}
	return wants, nil
}

func loadDockerOCIArchive(ctx context.Context, client *dockerclient.Client, source content.ReadOnlyStorage, root v1.Descriptor) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	writeDone := make(chan error, 1)
	go func() {
		err := localstore.WriteArchiveTo(streamCtx, source, root, writer)
		if err != nil {
			_ = writer.CloseWithError(err)
		} else {
			_ = writer.Close()
		}
		writeDone <- err
	}()

	response, loadErr := client.ImageLoad(streamCtx, reader, dockerclient.ImageLoadWithQuiet(true))
	if loadErr != nil {
		cancel()
		_ = reader.CloseWithError(loadErr)
		writeErr := <-writeDone
		if writeErr != nil && !errors.Is(writeErr, context.Canceled) {
			return fmt.Errorf("stream OCI archive to Docker: %w", writeErr)
		}
		return fmt.Errorf("load OCI archive into Docker: %w", loadErr)
	}
	responseErr := decodeDockerLoadResponse(response)
	closeErr := response.Close()
	if responseErr != nil {
		cancel()
		_ = reader.CloseWithError(responseErr)
	} else {
		_ = reader.Close()
	}
	writeErr := <-writeDone
	if responseErr != nil {
		if writeErr != nil && !errors.Is(writeErr, context.Canceled) && !errors.Is(writeErr, io.ErrClosedPipe) {
			return errors.Join(responseErr, fmt.Errorf("stream OCI archive to Docker: %w", writeErr))
		}
		return responseErr
	}
	if writeErr != nil {
		return fmt.Errorf("stream OCI archive to Docker: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close Docker image load response: %w", closeErr)
	}
	return ctx.Err()
}

func decodeDockerLoadResponse(reader io.Reader) error {
	decoder := json.NewDecoder(reader)
	for {
		var message struct {
			Error       string `json:"error"`
			ErrorDetail *struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := decoder.Decode(&message); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode Docker image load response: %w", err)
		}
		if message.ErrorDetail != nil && message.ErrorDetail.Message != "" {
			return fmt.Errorf("docker image load: %s", message.ErrorDetail.Message)
		}
		if message.Error != "" {
			return fmt.Errorf("docker image load: %s", message.Error)
		}
	}
}
