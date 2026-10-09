package buildah

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"go.podman.io/buildah/copier"
	"go.podman.io/image/v5/pkg/compression"
)

// Freeze filesystem transports through Buildah's confined copier. Valid links
// are rewritten to copied files or snapshot-relative directory links, so image
// readers cannot follow them into the host filesystem outside the context.
func sanitizeTransportSource(ctx context.Context, reference, contextDir string) (sanitizedReference string, cleanupSource func(), retErr error) {
	transport, value, _ := strings.Cut(reference, ":")
	switch transport {
	case "oci", "oci-archive", "docker-archive", "dir":
	default:
		return reference, func() {}, nil
	}
	source, selector, tagged := strings.Cut(value, ":")
	if transport == "dir" {
		source = value
		selector = ""
		tagged = false
	}
	if contextDir == "" {
		contextDir = filepath.Dir(source)
	}
	root, err := os.MkdirTemp("", "coopr-base-source-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	defer func() {
		if retErr != nil {
			cleanup()
			sanitizedReference = ""
			cleanupSource = nil
		}
	}()
	fail := func(err error) (string, func(), error) { cleanup(); return "", nil, err }
	stream, writer := io.Pipe()
	result := make(chan error, 1)
	go func() {
		err := copier.Get(contextDir, contextDir, copier.GetOptions{DisallowWildcard: true}, []string{source}, writer)
		_ = writer.CloseWithError(err)
		result <- err
	}()
	finished := false
	defer func() {
		_ = stream.Close()
		if !finished {
			<-result
		}
	}()
	var input io.Reader = stream
	archive := transport == "oci-archive" || transport == "docker-archive"
	if archive {
		outer := tar.NewReader(stream)
		header, err := outer.Next()
		if err != nil {
			return fail(err)
		}
		if header.Typeflag != tar.TypeReg {
			return fail(fmt.Errorf("image archive %q is not a regular file", source))
		}
		decompressed, _, err := compression.AutoDecompress(outer)
		if err != nil {
			return fail(err)
		}
		defer func() { retErr = errors.Join(retErr, decompressed.Close()) }()
		input = decompressed
	}
	sanitized, err := os.Create(filepath.Join(root, "image.tar"))
	if err != nil {
		return fail(err)
	}
	defer func() { retErr = errors.Join(retErr, sanitized.Close()) }()
	if err := filterImageArchive(ctx, input, sanitized); err != nil {
		return fail(err)
	}
	// Archives may leave the enclosing copier tar padding unread. Drain that
	// stream before joining the producer so a valid inner tar cannot hide a
	// failed source copy and the producer cannot remain blocked writing it.
	if _, err := io.Copy(io.Discard, contextReader{ctx: ctx, reader: stream}); err != nil {
		return fail(err)
	}
	copyErr := <-result
	finished = true
	if copyErr != nil {
		return fail(copyErr)
	}
	if _, err := sanitized.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	output := sanitized.Name()
	if !archive {
		output = filepath.Join(root, "image")
		if err := os.Mkdir(output, 0700); err != nil {
			return fail(err)
		}
		if err := copier.Put(output, output, copier.PutOptions{IgnoreDevices: true}, sanitized); err != nil {
			return fail(err)
		}
	}
	ref := transport + ":" + output
	if tagged {
		ref += ":" + selector
	}
	return ref, cleanup, nil
}

func filterImageArchive(ctx context.Context, input io.Reader, output io.Writer) error {
	reader := tar.NewReader(contextReader{ctx: ctx, reader: input})
	writer := tar.NewWriter(output)
	seen := map[string]byte{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return writer.Close()
		}
		if err != nil {
			return err
		}
		if !filepath.IsLocal(header.Name) {
			return fmt.Errorf("image source contains non-local path %q", header.Name)
		}
		header.Name = path.Clean(header.Name)
		switch header.Typeflag {
		case tar.TypeDir, tar.TypeReg:
		case tar.TypeLink, tar.TypeSymlink:
			link := header.Linkname
			if header.Typeflag == tar.TypeSymlink {
				if !path.IsAbs(link) {
					link = path.Join(path.Dir(header.Name), link)
				}
				link = strings.TrimPrefix(path.Clean("/"+link), "/")
			}
			kind, ok := seen[link]
			if !ok || kind == tar.TypeDir && header.Typeflag != tar.TypeSymlink {
				return fmt.Errorf("image source link %q targets uncopied file %q", header.Name, header.Linkname)
			}
			if kind == tar.TypeDir {
				// Upstream keeps directory links inside its frozen snapshot. A
				// relative target remains confined after the snapshot is moved.
				relative, err := filepath.Rel(path.Dir(header.Name), link)
				if err != nil {
					return err
				}
				header.Linkname = filepath.ToSlash(relative)
			} else {
				header.Typeflag = tar.TypeLink
				header.Linkname = link
			}
			header.Size = 0
		default:
			return fmt.Errorf("image source contains unsupported entry %q of type %d", header.Name, header.Typeflag)
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if _, err := io.Copy(writer, reader); err != nil {
			return err
		}
		seen[header.Name] = header.Typeflag
	}
}
