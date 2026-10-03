package buildah

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"coopr/internal/buildcontext"
	"coopr/internal/planner"
	opencontainersdigest "github.com/opencontainers/go-digest"
	storagearchive "go.podman.io/storage/pkg/archive"
)

func sourceDateEpochResolver(ctx context.Context, credentials buildCredentialSource) planner.SourceDateEpochResolver {
	return func(source planner.SourceDateEpochSource) (*int64, error) {
		switch source.Kind {
		case planner.SourceDateEpochNamedContext:
			if source.Context == nil {
				return nil, errors.New("named context metadata is missing")
			}
			return resolveNamedContextSourceDateEpoch(ctx, *source.Context, credentials)
		case planner.SourceDateEpochGit:
			return resolveGitSourceDateEpoch(ctx, source.Reference, source.Checksum, credentials)
		case planner.SourceDateEpochHTTP:
			return resolveHTTPSourceDateEpoch(ctx, source.Reference, source.Checksum, credentials)
		default:
			return nil, fmt.Errorf("unsupported source kind %q", source.Kind)
		}
	}
}

func resolveNamedContextSourceDateEpoch(ctx context.Context, spec buildcontext.Spec, credentials buildCredentialSource) (*int64, error) {
	switch spec.Kind {
	case buildcontext.Local, buildcontext.DockerImage, buildcontext.OCILayout:
		return nil, nil
	case buildcontext.Git:
		primary, cleanup, err := materializePrimaryGitContext(ctx, spec, credentials)
		if cleanup != nil {
			defer func() { _ = cleanup() }()
		}
		if err != nil {
			return nil, err
		}
		return primary.SourceDateEpoch, nil
	case buildcontext.HTTPArchive:
		_, epoch, cleanup, err := downloadHTTPContext(ctx, spec)
		if cleanup != nil {
			defer func() { _ = cleanup() }()
		}
		return epoch, err
	default:
		return nil, fmt.Errorf("unsupported named context kind %q", spec.Kind)
	}
}

func resolveGitSourceDateEpoch(ctx context.Context, reference, checksum string, credentials buildCredentialSource) (_ *int64, retErr error) {
	directory, err := os.MkdirTemp("", "coopr-source-date-epoch-git-")
	if err != nil {
		return nil, fmt.Errorf("create Git metadata directory: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(directory)) }()
	repository := filepath.Join(directory, "repository")
	if _, err := cloneGitSource(ctx, credentials, reference, repository, gitCloneOptions{submodules: false, checksum: checksum}); err != nil {
		return nil, err
	}
	epoch, err := clonedGitCommitEpoch(ctx, repository)
	if err != nil {
		return nil, err
	}
	return &epoch, nil
}

func resolveHTTPSourceDateEpoch(ctx context.Context, reference, checksum string, credentials buildCredentialSource) (_ *int64, retErr error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, reference, nil)
	if err != nil {
		return nil, fmt.Errorf("create source metadata request: %w", err)
	}
	if err := setRemoteAddAuthorization(request, credentials); err != nil {
		return nil, fmt.Errorf("authorize source metadata request: %w", err)
	}
	response, err := remoteAddHTTPClient(credentials).Do(request) //nolint:gosec // The authored remote ADD URL is intentional.
	if err != nil {
		return nil, fmt.Errorf("download source metadata: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, response.Body.Close()) }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("download source metadata: HTTP status %s", response.Status)
	}

	file, err := os.CreateTemp("", "coopr-source-date-epoch-http-*")
	if err != nil {
		return nil, fmt.Errorf("stage source metadata response: %w", err)
	}
	fileOpen := true
	defer func() {
		if fileOpen {
			retErr = errors.Join(retErr, file.Close())
		}
		retErr = errors.Join(retErr, os.Remove(file.Name()))
	}()
	var writer io.Writer = file
	var verifier opencontainersdigest.Digester
	var expected opencontainersdigest.Digest
	if checksum != "" {
		expected, err = opencontainersdigest.Parse(checksum)
		if err != nil {
			return nil, fmt.Errorf("invalid ADD checksum: %w", err)
		}
		if expected.Algorithm() != opencontainersdigest.SHA256 {
			return nil, fmt.Errorf("ADD remote checksum must use sha256, got %s", expected.Algorithm())
		}
		verifier = expected.Algorithm().Digester()
		writer = io.MultiWriter(file, verifier.Hash())
	}
	if _, err := io.Copy(writer, response.Body); err != nil {
		return nil, fmt.Errorf("stage source metadata response: %w", err)
	}
	if checksum != "" && verifier.Digest() != expected {
		return nil, fmt.Errorf("unexpected response digest for %q: %s, want %s", reference, verifier.Digest(), expected)
	}
	if modified := response.Header.Get("Last-Modified"); modified != "" {
		if value, err := http.ParseTime(modified); err == nil {
			epoch := value.Unix()
			return &epoch, nil
		}
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close source metadata response: %w", err)
	}
	fileOpen = false
	if !storagearchive.IsArchivePath(file.Name()) {
		return nil, nil
	}
	return sourceDateEpochFromArchive(file.Name())
}

func sourceDateEpochFromArchive(path string) (_ *int64, retErr error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	stream, err := storagearchive.DecompressStream(file)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, stream.Close()) }()
	reader := tar.NewReader(stream)
	var newest time.Time
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if (header.Typeflag == tar.TypeReg || header.Typeflag == 0) && header.ModTime.After(newest) {
			newest = header.ModTime
		}
	}
	if newest.IsZero() {
		return nil, nil
	}
	epoch := newest.Unix()
	return &epoch, nil
}
