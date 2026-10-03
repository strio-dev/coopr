// Package buildcontext defines named inputs supplied independently of the
// primary build context.
package buildcontext

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

type Kind string

const (
	Local       Kind = "local"
	DockerImage Kind = "docker-image"
	OCILayout   Kind = "oci-layout"
	Git         Kind = "git"
	HTTPArchive Kind = "http-archive"
)

// Spec is the normalized, serializable form of one --build-context value.
type Spec struct {
	Name      string `json:"name"`
	Kind      Kind   `json:"kind"`
	Path      string `json:"path,omitempty"`
	Reference string `json:"reference,omitempty"`
}

var contextNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)

// Parse parses repeated NAME=VALUE arguments, using the last value for each
// normalized name. Relative paths are resolved from the working directory.
func Parse(values []string) ([]Spec, error) {
	result := make([]Spec, 0, len(values))
	indexes := make(map[string]int, len(values))
	for _, value := range values {
		spec, err := parseOne(value)
		if err != nil {
			return nil, err
		}
		if index, ok := indexes[spec.Name]; ok {
			result[index] = spec
			continue
		}
		indexes[spec.Name] = len(result)
		result = append(result, spec)
	}
	return result, nil
}

// ParsePrimary classifies the primary filesystem context accepted by
// --context. An empty value selects defaultPath. Unlike named contexts, the
// primary context only accepts filesystem-producing sources.
func ParsePrimary(value, defaultPath string) (Spec, error) {
	if value == "" {
		value = defaultPath
	}
	spec, err := parseOne("context=" + value)
	if err != nil {
		return Spec{}, err
	}
	if spec.Kind != Local && spec.Kind != Git && spec.Kind != HTTPArchive {
		return Spec{}, fmt.Errorf("primary build context %q must be a local directory, Git URL, or HTTP(S) tar archive", value)
	}
	return spec, nil
}

func parseOne(value string) (Spec, error) {
	name, source, ok := strings.Cut(value, "=")
	if !ok || name == "" || source == "" {
		return Spec{}, fmt.Errorf("invalid build context %q: expected NAME=VALUE", value)
	}
	name = strings.ToLower(name)
	if !contextNamePattern.MatchString(name) {
		return Spec{}, fmt.Errorf("invalid build context name %q", name)
	}
	if name == "scratch" {
		return Spec{}, fmt.Errorf("build context name %q is reserved", name)
	}

	switch {
	case strings.HasPrefix(source, "docker-image://"), strings.HasPrefix(source, "container-image://"):
		_, reference, _ := strings.Cut(source, "://")
		if reference == "" || strings.ContainsAny(reference, " \t\r\n") || strings.Contains(reference, "://") {
			return Spec{}, fmt.Errorf("invalid docker-image build context %q", source)
		}
		return Spec{Name: name, Kind: DockerImage, Reference: reference}, nil
	case strings.HasPrefix(source, "oci-layout://"):
		path, reference, err := parseOCILayout(strings.TrimPrefix(source, "oci-layout://"))
		if err != nil {
			return Spec{}, fmt.Errorf("invalid OCI layout build context %q: %w", source, err)
		}
		return Spec{Name: name, Kind: OCILayout, Path: path, Reference: reference}, nil
	case isUnsupportedGitSource(source):
		return Spec{}, fmt.Errorf("unsupported remote build context %q", source)
	case remoteURLHasCredentials(source):
		return Spec{}, errors.New("remote build context URL must not contain credentials")
	case isGitSource(source):
		return Spec{Name: name, Kind: Git, Reference: source}, nil
	case isHTTPSource(source):
		return Spec{Name: name, Kind: HTTPArchive, Reference: source}, nil
	case strings.Contains(source, "://"):
		scheme, _, _ := strings.Cut(source, "://")
		return Spec{}, fmt.Errorf("unsupported build context scheme %q", scheme)
	default:
		path, err := filepath.Abs(source)
		if err != nil {
			return Spec{}, fmt.Errorf("normalize build context path %q: %w", source, err)
		}
		return Spec{Name: name, Kind: Local, Path: filepath.Clean(path)}, nil
	}
}

func remoteURLHasCredentials(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.User == nil {
		return false
	}
	_, hasPassword := parsed.User.Password()
	return hasPassword || parsed.Scheme == "http" || parsed.Scheme == "https"
}

func parseOCILayout(value string) (string, string, error) {
	if value == "" {
		return "", "", fmt.Errorf("layout path and tag or digest are required")
	}
	path, reference := "", ""
	if index := strings.LastIndex(value, "@"); index > 0 {
		path, reference = value[:index], value[index+1:]
	} else if index := strings.LastIndex(value, ":"); index > strings.LastIndexAny(value, `/\\`) {
		path, reference = value[:index], value[index+1:]
	}
	if path == "" || reference == "" {
		return "", "", fmt.Errorf("layout path must include a tag or digest")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("normalize layout path %q: %w", path, err)
	}
	return filepath.Clean(abs), reference, nil
}

func isGitSource(value string) bool {
	if isSCPGitSource(value) {
		return true
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}
	return parsed.Scheme == "git" || parsed.Scheme == "ssh" ||
		((parsed.Scheme == "http" || parsed.Scheme == "https") && strings.HasSuffix(parsed.Path, ".git"))
}

func isSCPGitSource(value string) bool {
	userHost, path, found := strings.Cut(value, ":")
	if !found || path == "" || strings.ContainsAny(userHost, "/\\\t\r\n ") {
		return false
	}
	user, host, found := strings.Cut(userHost, "@")
	return found && user != "" && host != ""
}

func isHTTPSource(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func isUnsupportedGitSource(value string) bool {
	for _, prefix := range []string{"git+http://", "git+https://", "git+ssh://"} {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
