package buildah

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/strio-dev/coopr/internal/definition"
	upstream "go.podman.io/buildah"
)

// addInlineData batches inline sources into one native Add, matching Buildah's
// heredoc layer semantics. Separate directories preserve duplicate source names.
func addInlineData(builder operationBuilder, destination string, sources []definition.InlineFile, options upstream.AddAndCopyOptions) (retErr error) {
	if len(sources) == 0 {
		return nil
	}
	if destination == "" {
		return errors.New("inline COPY/ADD requires a destination")
	}
	for _, source := range sources {
		if !filepath.IsLocal(source.Path) || source.Path == "." {
			return fmt.Errorf("inline COPY/ADD source path %q is not local", source.Path)
		}
	}
	directory, err := os.MkdirTemp("", "coopr-inline-*")
	if err != nil {
		return fmt.Errorf("create inline source directory: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(directory)) }()
	paths := make([]string, 0, len(sources))
	for index, source := range sources {
		relative := filepath.Join(strconv.Itoa(index), filepath.FromSlash(source.Path))
		file := filepath.Join(directory, relative)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			return fmt.Errorf("create inline source parent: %w", err)
		}
		if err := os.WriteFile(file, []byte(source.Data), 0600); err != nil {
			return fmt.Errorf("write inline source: %w", err)
		}
		if err := os.Chmod(file, 0644); err != nil {
			return fmt.Errorf("set inline source mode: %w", err)
		}
		if err := os.Chtimes(file, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
			return fmt.Errorf("set inline source timestamp: %w", err)
		}
		paths = append(paths, relative)
	}
	// Native Buildah heredoc sources bypass context and instruction exclusions.
	options.Excludes = nil
	options.IgnoreFile = ""
	options.ContextDir = directory
	return builder.add(destination, false, options, paths...)
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
