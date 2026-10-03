package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"coopr/internal/buildah"
	"coopr/internal/definition"
)

// Context-contained definitions are read after extraction. Explicit local or
// stdin definitions keep their original ordering before context preparation.
func prepareDefinitionContext(ctx context.Context, file, contextValue string, inContext bool, secrets, ssh []string, stdin io.Reader) (*definition.Definition, buildah.PrimaryContext, string, func() error, error) {
	var def *definition.Definition
	var err error
	if !inContext {
		def, err = readDefinition(ctx, file, stdin)
		if err != nil {
			return nil, buildah.PrimaryContext{}, "", nil, err
		}
	}
	primary, cleanup, err := preparePrimaryContext(ctx, file, contextValue, secrets, ssh, stdin)
	if err != nil {
		return nil, buildah.PrimaryContext{}, "", nil, err
	}
	if inContext {
		file, err = contextDefinitionPath(primary.Path, file)
		if err != nil {
			_ = cleanup()
			return nil, buildah.PrimaryContext{}, "", nil, err
		}
		def, err = readDefinition(ctx, file, nil)
		if err != nil {
			_ = cleanup()
			return nil, buildah.PrimaryContext{}, "", nil, err
		}
	}
	return def, primary, file, cleanup, nil
}

func contextDefinitionPath(contextDir, name string) (string, error) {
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("definition path %q must be a local relative path", name)
	}
	root, err := filepath.EvalSymlinks(contextDir)
	if err != nil {
		return "", fmt.Errorf("resolve build context: %w", err)
	}
	definitionPath, err := filepath.EvalSymlinks(filepath.Join(root, filepath.Clean(name)))
	if err != nil {
		return "", fmt.Errorf("resolve context definition %q: %w", name, err)
	}
	relative, err := filepath.Rel(root, definitionPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("definition path %q resolves outside build context", name)
	}
	info, err := os.Stat(definitionPath)
	if err != nil {
		return "", fmt.Errorf("inspect context definition %q: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("context definition %q is not a regular file", name)
	}
	return definitionPath, nil
}

func readDefinition(ctx context.Context, path string, stdin io.Reader) (*definition.Definition, error) {
	if path == "-" {
		if stdin == nil {
			stdin = os.Stdin
		}
		return definition.Parse(stdin)
	}
	if isHTTPDefinition(path) {
		parsed, err := url.Parse(path)
		if err != nil {
			var parseError *url.Error
			if errors.As(err, &parseError) {
				err = parseError.Err
			}
			return nil, fmt.Errorf("parse definition URL: %w", err)
		}
		return readHTTPDefinition(ctx, parsed)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open definition: %w", err)
	}
	defer func() { _ = file.Close() }()
	return definition.Parse(file)
}

func definitionDisplayName(name string) string {
	if isHTTPDefinition(name) {
		if source, err := url.Parse(name); err == nil {
			return source.Redacted()
		}
		return "HTTP definition"
	}
	return name
}

func isHTTPDefinition(name string) bool {
	return strings.HasPrefix(name, "http://") || strings.HasPrefix(name, "https://")
}

func readHTTPDefinition(ctx context.Context, source *url.URL) (_ *definition.Definition, retErr error) {
	if source.Host == "" {
		return nil, fmt.Errorf("definition URL %q has no host", source.Redacted())
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create definition request: %w", err)
	}
	response, err := http.DefaultClient.Do(request) //nolint:gosec // --file explicitly permits an authored remote definition URL.
	if err != nil {
		return nil, fmt.Errorf("download definition %q: %w", source.Redacted(), err)
	}
	defer func() { retErr = errors.Join(retErr, response.Body.Close()) }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("download definition %q: HTTP status %s", source.Redacted(), response.Status)
	}
	return definition.Parse(response.Body)
}

// Explicit --ignorefile paths are resolved from the invoking directory.
func selectIgnoreFile(contextDir, explicit string) (string, error) {
	if explicit != "" {
		absolute, err := filepath.Abs(explicit)
		if err != nil {
			return "", err
		}
		explicit = absolute
	}
	_, selected, err := buildah.ReadContextIgnore(contextDir, explicit)
	if err != nil {
		return "", fmt.Errorf("select ignore file: %w", err)
	}
	return selected, nil
}

func buildJobLimits(requested, platformCount int) (platformJobs, stageJobs int) {
	if requested == 0 {
		return max(1, platformCount), 0
	}
	platformJobs = min(max(1, platformCount), max(1, requested))
	return platformJobs, max(1, requested/platformJobs)
}
