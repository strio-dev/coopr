package buildah

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.podman.io/buildah/copier"
)

// snapshotWritableBindFile gives a RUN its own copy of a file mounted from an
// immutable stage or image. Writes to the bind mount must not change its source
// or become part of the resulting image.
func snapshotWritableBindFile(source string) (string, func() error, error) {
	info, err := os.Stat(source)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("RUN bind source %q is not a regular file", source)
	}
	root, err := os.MkdirTemp("", "coopr-run-bind-file-")
	if err != nil {
		return "", nil, fmt.Errorf("create RUN bind snapshot: %w", err)
	}
	cleanup := func() error {
		if err := os.RemoveAll(root); err != nil {
			return fmt.Errorf("remove RUN bind snapshot: %w", err)
		}
		return nil
	}
	parent, name := filepath.Dir(source), filepath.Base(source)
	reader, writer := io.Pipe()
	got := make(chan error, 1)
	go func() {
		copyErr := copier.Get(parent, parent, copier.GetOptions{}, []string{name}, writer)
		got <- errors.Join(copyErr, writer.CloseWithError(copyErr))
	}()
	putErr := copier.Put(root, root, copier.PutOptions{}, reader)
	if putErr != nil {
		_ = reader.CloseWithError(putErr)
	} else {
		_ = reader.Close()
	}
	if err := errors.Join(<-got, putErr); err != nil {
		return "", nil, errors.Join(fmt.Errorf("copy RUN bind source: %w", err), cleanup())
	}
	path := filepath.Join(root, name)
	if _, err := os.Stat(path); err != nil {
		return "", nil, errors.Join(fmt.Errorf("inspect RUN bind snapshot: %w", err), cleanup())
	}
	return path, cleanup, nil
}
