package buildah

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/strio-dev/coopr/internal/stateidentity"
	"go.podman.io/buildah/copier"
	storagearchive "go.podman.io/storage/pkg/archive"
)

// capturedContext belongs to one private copier result. Its identity is consumed
// before any workload receives the directory; it is never a memo for a path or
// for a writable ContextPrepared input.
type capturedContext struct {
	directory string
	identity  digest.Digest
	index     *stateidentity.CapturedIndex
	cleanup   func() error
}

type contextPayload struct {
	digest digest.Digest
	size   int64
	link   string
}

func (capture capturedContext) runMountIdentity(ctx context.Context, mounts []RunMount) (digest.Digest, error) {
	if capture.index == nil {
		return "", errors.New("RUN context selection requires a retained capture index")
	}
	sources := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		if mount.Type != "bind" || mount.BoundFrom || mount.HostPath != "" || mount.Artifact != nil {
			continue
		}
		properties, err := normalizeRunMountProperties(mount.Type, mount.Properties)
		if err != nil {
			return "", err
		}
		readonly := true
		if value := properties["readonly"]; value != "" {
			readonly, err = strconv.ParseBool(value)
			if err != nil {
				return "", err
			}
		}
		ownership, _ := strconv.ParseBool(properties["u"])
		if !readonly || ownership {
			return capture.identity, nil
		}
		source, err := normalizeContextMountSource(properties["source"])
		if err != nil {
			return "", err
		}
		// Match GetBindMount's coordinates exactly, including final symlinks.
		resolved, err := copier.Eval(capture.directory, capture.directory+string(filepath.Separator)+source, copier.EvalOptions{})
		if err != nil {
			return "", fmt.Errorf("resolve captured RUN bind source %q: %w", source, err)
		}
		relative, err := filepath.Rel(capture.directory, resolved)
		if err != nil || !filepath.IsLocal(relative) {
			return "", fmt.Errorf("resolved RUN bind source %q is outside its capture", source)
		}
		sources = append(sources, filepath.ToSlash(relative))
	}
	if len(sources) == 0 || len(sources) == 1 && sources[0] == "." {
		return capture.identity, nil
	}
	return capture.index.Select(ctx, sources)
}

// observeContextPayloads hashes only bytes already consumed by native Put. The
// caller must finish after Put, even on failure, to close and join the observer.
func observeContextPayloads(ctx context.Context, input io.Reader, source string) (io.Reader, func(error) (map[string]contextPayload, error)) {
	reader, writer := io.Pipe()
	stop := context.AfterFunc(ctx, func() {
		_ = reader.CloseWithError(ctx.Err())
		_ = writer.CloseWithError(ctx.Err())
	})
	type result struct {
		payloads map[string]contextPayload
		err      error
	}
	finished := make(chan result, 1)
	go func() {
		payloads, err := readContextPayloads(reader, source)
		_ = reader.CloseWithError(err)
		finished <- result{payloads, err}
	}()
	return io.TeeReader(input, writer), func(putErr error) (map[string]contextPayload, error) {
		_ = writer.CloseWithError(putErr)
		result := <-finished
		stop()
		if err := errors.Join(ctx.Err(), putErr, result.err); err != nil {
			return nil, err
		}
		return result.payloads, nil
	}
}

func readContextPayloads(input io.Reader, source string) (map[string]contextPayload, error) {
	payloads := make(map[string]contextPayload)
	reader := tar.NewReader(input)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			// Next stops at tar's end marker. Drain the original stream's padding
			// so Get cannot block while Put waits for stream completion.
			// Native Get supplies uncompressed tar; owner cancellation closes this pipe.
			// nosemgrep: cache.semgrep-rules.go.lang.security.potential-dos-via-decompression-bomb
			_, err = io.Copy(io.Discard, input)
			return payloads, err
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeLink {
			continue
		}
		name, err := contextSnapshotName(header.Name, source)
		if err != nil {
			return nil, err
		}
		if _, duplicate := payloads[name]; duplicate {
			return nil, fmt.Errorf("duplicate captured payload %q", name)
		}
		payload := contextPayload{size: header.Size}
		if header.Typeflag == tar.TypeLink {
			payload.link, err = contextSnapshotName(header.Linkname, source)
		} else {
			hash := digest.Canonical.Digester()
			// Native Get emits regular, uncompressed payloads bounded by each tar entry.
			// nosemgrep: cache.semgrep-rules.go.lang.security.potential-dos-via-decompression-bomb
			_, err = io.Copy(hash.Hash(), reader)
			payload.digest = hash.Digest()
		}
		if err != nil {
			return nil, err
		}
		payloads[name] = payload
	}
}

// Native Get prefixes the root basename, or "/" when chrooted. Native Put
// removes that single prefix from both file names and hardlink targets.
func contextSnapshotName(name, source string) (string, error) {
	name = strings.TrimSuffix(name, "/")
	var relative string
	if strings.HasPrefix(name, "/") {
		relative = strings.TrimPrefix(name, "/")
	} else {
		var ok bool
		relative, ok = strings.CutPrefix(name, filepath.Base(source)+"/")
		if !ok {
			return "", fmt.Errorf("captured payload %q is outside its native root", name)
		}
	}
	if !filepath.IsLocal(filepath.FromSlash(relative)) || relative == "." {
		return "", fmt.Errorf("captured payload %q escapes its native root", name)
	}
	return relative, nil
}

func indexCapturedContext(ctx context.Context, root string, payloads map[string]contextPayload) (*stateidentity.CapturedIndex, error) {
	records := make([]stateidentity.CapturedEntry, 0, len(payloads)+1)
	seen := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := errors.Join(ctx.Err(), walkErr); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(name)
		header.Uname, header.Gname = "", ""
		// Native Get's default tar writer discards access/change times. They
		// describe capture infrastructure and would change on every read/copy.
		header.AccessTime, header.ChangeTime = time.Time{}, time.Time{}
		attrs, err := copier.Lgetxattrs(path)
		if err != nil {
			return err
		}
		header.PAXRecords = make(map[string]string, len(attrs))
		for key, value := range attrs {
			header.PAXRecords["SCHILY.xattr."+key] = value
		}
		if err := storagearchive.ReadFileFlagsToTarHeader(path, header); err != nil {
			return err
		}
		record := stateidentity.CapturedEntry{Header: header}
		if info.Mode().IsRegular() {
			payload, found := payloads[header.Name]
			if !found {
				return fmt.Errorf("materialized file %q has no captured payload", header.Name)
			}
			seen++
			if payload.link != "" {
				target, err := os.Lstat(filepath.Join(root, filepath.FromSlash(payload.link)))
				if err != nil {
					return fmt.Errorf("inspect captured hardlink target %q: %w", payload.link, err)
				}
				if !os.SameFile(info, target) {
					return fmt.Errorf("materialized hardlink %q differs from captured target %q", header.Name, payload.link)
				}
				header.Typeflag, header.Size, header.Linkname = tar.TypeLink, 0, payload.link
			} else {
				if info.Size() != payload.size {
					return fmt.Errorf("materialized file %q has length %d, expected %d", header.Name, info.Size(), payload.size)
				}
				record.Content = payload.digest
			}
		}
		records = append(records, record)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if seen != len(payloads) {
		return nil, errors.New("captured payloads are absent from materialized context")
	}
	return stateidentity.NewCapturedIndex(ctx, records)
}
