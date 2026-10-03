package buildah

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"coopr/internal/definition"
	upstream "go.podman.io/buildah"
)

// addInlineData gives Buildah an ordinary local file source so inline COPY and
// ADD retain its ownership, mode, link, and destination semantics. The source
// lives outside the build context and has stable metadata for repeatable layers.
func addInlineData(builder operationBuilder, destination string, source definition.InlineFile, options upstream.AddAndCopyOptions) (retErr error) {
	if destination == "" {
		return errors.New("inline COPY/ADD requires a destination")
	}
	if !filepath.IsLocal(source.Path) || source.Path == "." {
		return fmt.Errorf("inline COPY/ADD source path %q is not local", source.Path)
	}
	directory, err := os.MkdirTemp("", "coopr-inline-*")
	if err != nil {
		return fmt.Errorf("create inline source directory: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(directory)) }()
	file := filepath.Join(directory, filepath.FromSlash(source.Path))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return fmt.Errorf("create inline source parent: %w", err)
	}
	if err := os.WriteFile(file, []byte(source.Data), 0o600); err != nil {
		return fmt.Errorf("write inline source: %w", err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		return fmt.Errorf("set inline source mode: %w", err)
	}
	epoch := time.Unix(0, 0)
	if err := os.Chtimes(file, epoch, epoch); err != nil {
		return fmt.Errorf("set inline source timestamp: %w", err)
	}
	options.ContextDir = directory
	return builder.add(destination, false, options, filepath.FromSlash(source.Path))
}

func inlineCopyOptions(options upstream.AddAndCopyOptions) upstream.AddAndCopyOptions {
	options.Parents = false
	options.Checksum = ""
	return options
}

func materializeRunInlineFiles(sources []definition.InlineFile) (string, func() error, error) {
	directory, err := os.MkdirTemp("", "coopr-run-inline-*")
	if err != nil {
		return "", nil, fmt.Errorf("create RUN inline directory: %w", err)
	}
	cleanup := func() error { return os.RemoveAll(directory) }
	for _, source := range sources {
		if !filepath.IsLocal(source.Path) || source.Path == "." {
			_ = cleanup()
			return "", nil, fmt.Errorf("RUN inline source path %q is not local", source.Path)
		}
		file := filepath.Join(directory, filepath.FromSlash(source.Path))
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			_ = cleanup()
			return "", nil, fmt.Errorf("create RUN inline source parent: %w", err)
		}
		if err := os.WriteFile(file, []byte(source.Data), 0o755); err != nil {
			_ = cleanup()
			return "", nil, fmt.Errorf("write RUN inline source: %w", err)
		}
	}
	epoch := time.Unix(0, 0)
	if err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if err := os.Chmod(path, 0o755); err != nil {
				return err
			}
		}
		return os.Chtimes(path, epoch, epoch)
	}); err != nil {
		_ = cleanup()
		return "", nil, fmt.Errorf("normalize RUN inline source metadata: %w", err)
	}
	return directory, cleanup, nil
}
