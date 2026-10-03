package buildah

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/storage"
)

// CopyFromImage copies paths from an immutable image in Coopr's private
// containers/storage store into destination. An isolated builder mounts the
// source image for the duration of the copy and is always deleted before the
// function returns.
//
// sources use container-root paths. Relative paths are interpreted relative
// to the source image root, matching Containerfile COPY --from semantics.
// extract selects ADD-style local archive extraction. Sources become absolute
// paths beneath the mounted image root, so URL-looking names remain image paths
// and cannot trigger a remote fetch.
func CopyFromImage(store storage.Store, sourceImageID string, destination *upstream.Builder, destinationPath string, extract bool, options upstream.AddAndCopyOptions, sources ...string) (retErr error) {
	if store == nil {
		return errors.New("COPY --from store is nil")
	}
	if sourceImageID == "" {
		return errors.New("COPY --from source image ID is empty")
	}
	if destination == nil {
		return errors.New("COPY --from destination builder is nil")
	}
	if destinationPath == "" {
		return errors.New("COPY --from destination is empty")
	}
	if len(sources) == 0 {
		return errors.New("COPY --from requires at least one source")
	}
	options.Excludes = copySourceExcludes(options.Excludes, sources)
	relativeSources := make([]string, 0, len(sources))
	for _, sourcePath := range sources {
		relative, err := containerSourcePathForCopy(sourcePath, options.Parents)
		if err != nil {
			return err
		}
		relativeSources = append(relativeSources, relative)
	}

	sourceContainer := ""
	if destination.Container != "" {
		// A supervised build can also clean this temporary mount after a forced stop.
		sourceContainer = destination.Container + "-copy-source"
	}
	sourceBuilder, err := upstream.NewBuilder(context.Background(), store, upstream.BuilderOptions{
		FromImage: sourceImageID, PullPolicy: define.PullNever,
		Container:        sourceContainer,
		IDMappingOptions: options.IDMappingOptions,
		ProcessLabel:     destination.ProcessLabel,
		MountLabel:       destination.MountLabel,
	})
	if err != nil {
		return fmt.Errorf("create COPY --from builder for image %q: %w", sourceImageID, err)
	}
	defer func() {
		if deleteErr := sourceBuilder.Delete(); deleteErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("delete COPY --from builder for image %q: %w", sourceImageID, deleteErr))
		}
	}()
	mountPoint, err := sourceBuilder.Mount(sourceBuilder.MountLabel)
	if err != nil {
		return fmt.Errorf("mount COPY --from image %q: %w", sourceImageID, err)
	}
	defer func() {
		if unmountErr := sourceBuilder.Unmount(); unmountErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("unmount COPY --from image %q: %w", sourceImageID, unmountErr))
		}
	}()

	mountedSources := make([]string, 0, len(relativeSources))
	for _, relative := range relativeSources {
		mounted := filepath.Join(mountPoint, filepath.FromSlash(relative))
		if options.Parents && strings.Contains(filepath.ToSlash(relative), "/./") {
			mounted = preserveParentsPivot(mountPoint, true, []string{relative})[0]
		}
		mountedSources = append(mountedSources, mounted)
	}

	options.ContextDir = mountPoint
	options.PreserveOwnership = true
	options.IDMappingOptions = &sourceBuilder.IDMappingOptions
	options.Excludes = append([]string(nil), options.Excludes...)
	if err := destination.Add(destinationPath, extract, options, mountedSources...); err != nil {
		return fmt.Errorf("copy from image %q: %w", sourceImageID, err)
	}
	return nil
}

func containerSourcePath(source string) (string, error) {
	if source == "" {
		return "", errors.New("COPY --from source is empty")
	}
	if strings.IndexByte(source, 0) >= 0 {
		return "", errors.New("COPY --from source contains NUL")
	}
	containerPath := filepath.ToSlash(source)
	cleaned := strings.TrimPrefix(path.Clean("/"+containerPath), "/")
	if cleaned == "." {
		cleaned = ""
	}
	return cleaned, nil
}

func containerSourcePathForCopy(source string, parents bool) (string, error) {
	if !parents {
		return containerSourcePath(source)
	}
	prefix, suffix, found := strings.Cut(filepath.ToSlash(source), "/./")
	if !found {
		return containerSourcePath(source)
	}
	if prefix == "" {
		prefix = "."
	} else {
		var err error
		prefix, err = containerSourcePath(prefix)
		if err != nil {
			return "", err
		}
	}
	if suffix == "" {
		suffix = "."
	} else {
		var err error
		suffix, err = containerSourcePath(suffix)
		if err != nil {
			return "", err
		}
	}
	return prefix + "/./" + suffix, nil
}
