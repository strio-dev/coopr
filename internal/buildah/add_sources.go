package buildah

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	opencontainersdigest "github.com/opencontainers/go-digest"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/copier"
)

const (
	httpAuthHeaderSecretPrefix = "HTTP_AUTH_HEADER_"
	httpAuthTokenSecretPrefix  = "HTTP_AUTH_TOKEN_"
)

// addSourceCredentialProvider exposes request-scoped build secrets without
// placing their contents in the build plan or instruction cache key.
type addSourceCredentialProvider interface {
	addSourceSecret(id string) (value []byte, found bool, err error)
}

func applyAddSources(ctx context.Context, builder operationBuilder, policy contextPolicy, operation Add, options upstream.AddAndCopyOptions) error {
	if len(operation.Sources) == 0 || operation.Destination == "" {
		return errors.New("ADD requires at least one source and a destination")
	}
	if operation.Checksum != "" && len(operation.Sources) != 1 {
		return errors.New("ADD checksum requires exactly one source")
	}
	if !hasSpecialAddSource(operation.Sources) {
		applied, sources, err := policy.applyLocalCopy(options, operation.Sources)
		if err != nil {
			return err
		}
		extract := operation.Unpack == nil || *operation.Unpack
		return builder.add(operation.Destination, extract, applied, sources...)
	}

	for _, source := range operation.Sources {
		switch {
		case isGitAddSource(source):
			if err := applyGitAddSource(ctx, builder, operation, options, source); err != nil {
				return err
			}
		case isHTTPAddSource(source):
			if err := applyRemoteAddSource(ctx, builder, operation, options, source); err != nil {
				return err
			}
		default:
			applied, sources, err := policy.applyLocalCopy(options, []string{source})
			if err != nil {
				return err
			}
			extract := operation.Unpack == nil || *operation.Unpack
			if err := builder.add(operation.Destination, extract, applied, sources...); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasSpecialAddSource(sources []string) bool {
	for _, source := range sources {
		if isGitAddSource(source) || isHTTPAddSource(source) {
			return true
		}
	}
	return false
}

func isHTTPAddSource(source string) bool {
	return strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
}

func isGitAddSource(source string) bool {
	withoutFragment := source
	if index := strings.IndexByte(withoutFragment, '#'); index >= 0 {
		withoutFragment = withoutFragment[:index]
	}
	return strings.HasPrefix(withoutFragment, "git://") ||
		strings.HasPrefix(withoutFragment, "ssh://") ||
		strings.HasPrefix(withoutFragment, "git@") ||
		(isHTTPAddSource(withoutFragment) && strings.HasSuffix(withoutFragment, ".git"))
}

func applyGitAddSource(ctx context.Context, builder operationBuilder, operation Add, options upstream.AddAndCopyOptions, source string) (retErr error) {
	temporary, err := os.MkdirTemp("", "coopr-add-git-")
	if err != nil {
		return fmt.Errorf("create ADD Git clone directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(temporary); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove cloned ADD Git source: %w", err))
		}
	}()

	repository := filepath.Join(temporary, "download")
	contextDir, err := cloneGitAddSource(ctx, builder, source, repository, operation.Checksum)
	if err != nil {
		return err
	}
	if options.Hasher != nil {
		commit, err := resolveGitCommit(repository)
		if err != nil {
			return err
		}
		// The verified commit tree and its gitlinks identify the source. Clone
		// timestamps and generated .git metadata are not instruction inputs.
		if _, err := io.WriteString(options.Hasher, "coopr.strio.dev/add-git-input/v1\x00"+commit); err != nil {
			return fmt.Errorf("hash ADD Git commit: %w", err)
		}
		options.Hasher = nil
	}
	options = gitAddOptions(options, contextDir, operation.KeepGitDir)
	return builder.add(operation.Destination, false, options, ".")
}

func cloneGitAddSource(ctx context.Context, builder any, source, repository, checksum string) (contextDir string, retErr error) {
	return cloneGitSource(ctx, builder, source, repository, gitCloneOptions{submodules: true, checksum: checksum})
}

type gitCloneOptions struct {
	submodules  bool
	fetchCommit string
	checksum    string
	mtime       string
}

func cloneGitSource(ctx context.Context, builder any, source, repository string, options gitCloneOptions) (contextDir string, retErr error) {
	if ctx == nil {
		return "", errors.New("git clone context is nil")
	}
	remote, subdirectory, reference, err := parseGitAddSource(source)
	if err != nil {
		return "", err
	}
	executable, err := gitAddExecutable()
	if err != nil {
		return "", err
	}
	environment, cleanup, authenticated, err := gitAddEnvironment(builder, remote)
	if err != nil {
		if cleanup != nil {
			err = errors.Join(err, cleanup())
		}
		return "", fmt.Errorf("authenticate ADD Git source %q: %w", redactGitSource(source), err)
	}
	defer func() {
		if cleanup != nil {
			retErr = errors.Join(retErr, cleanup())
		}
	}()
	if options.fetchCommit != "" {
		reference = options.fetchCommit
	}
	commands := [][]string{
		{"init", repository},
		{"-C", repository, "remote", "add", "origin", remote},
		{"-C", repository, "fetch", "-u", "--depth=1", "origin", "--", reference},
		{"-C", repository, "checkout", "FETCH_HEAD"},
	}
	for _, command := range commands {
		process := gitCommandContext(ctx, executable, command...)
		process.Env = environment
		if output, runErr := process.CombinedOutput(); runErr != nil {
			if authenticated {
				return "", fmt.Errorf("clone authenticated ADD Git source %q: git %s: %w", redactGitSource(source), command[0], runErr)
			}
			return "", fmt.Errorf("clone ADD Git source %q: git %s: %w\n%s", redactGitSource(source), command[0], runErr, output)
		}
	}
	if options.checksum != "" {
		if err := verifyGitCommit(repository, options.checksum); err != nil {
			return "", fmt.Errorf("verify Git source %q: %w", redactGitSource(source), err)
		}
	}
	if options.submodules {
		command := []string{"-C", repository, "submodule", "update", "--init", "--recursive", "--depth=1"}
		process := gitCommandContext(ctx, executable, command...)
		process.Env = environment
		if output, runErr := process.CombinedOutput(); runErr != nil {
			if authenticated {
				return "", fmt.Errorf("clone authenticated ADD Git source %q: git %s: %w", redactGitSource(source), command[0], runErr)
			}
			return "", fmt.Errorf("clone ADD Git source %q: git %s: %w\n%s", redactGitSource(source), command[0], runErr, output)
		}
	}
	if options.mtime == "commit" {
		commitTime, err := gitCommitTime(ctx, executable, environment, repository)
		if err != nil {
			return "", fmt.Errorf("resolve ADD Git commit time for %q: %w", redactGitSource(source), err)
		}
		if err := resetGitCheckoutMTimes(repository, commitTime); err != nil {
			return "", fmt.Errorf("normalize ADD Git mtimes for %q: %w", redactGitSource(source), err)
		}
	}
	contextDir = repository
	if subdirectory != "" {
		contextDir, err = copier.Eval(repository, filepath.Join(repository, filepath.FromSlash(subdirectory)), copier.EvalOptions{})
		if err != nil {
			return "", fmt.Errorf("resolve ADD Git subdirectory %q: %w", subdirectory, err)
		}
		info, err := os.Stat(contextDir)
		if err != nil {
			return "", fmt.Errorf("stat ADD Git subdirectory %q: %w", subdirectory, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("ADD Git subdirectory %q is not a directory", subdirectory)
		}
	}
	return contextDir, nil
}

func gitCommitTime(ctx context.Context, executable string, environment []string, repository string) (time.Time, error) {
	process := gitCommandContext(ctx, executable, "-C", repository, "log", "-1", "--format=%ct", "HEAD^{commit}")
	process.Env = environment
	output, err := process.Output()
	if err != nil {
		return time.Time{}, err
	}
	timestamp, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse commit timestamp %q: %w", strings.TrimSpace(string(output)), err)
	}
	return time.Unix(timestamp, 0), nil
}

func parseGitAddSource(source string) (remote, subdirectory, reference string, err error) {
	remote, fragment, _ := strings.Cut(source, "#")
	if parsed, parseErr := url.Parse(remote); parseErr == nil && parsed.User != nil {
		_, hasPassword := parsed.User.Password()
		if isHTTPAddSource(remote) || hasPassword {
			return "", "", "", fmt.Errorf("ADD Git source %q must not contain credentials; use build secrets", redactGitSource(source))
		}
	}
	reference = "HEAD"
	if fragment != "" {
		reference, subdirectory, _ = strings.Cut(fragment, ":")
		if reference == "" {
			reference = "HEAD"
		}
	}
	if subdirectory != "" && !filepath.IsLocal(filepath.FromSlash(subdirectory)) {
		return "", "", "", fmt.Errorf("ADD Git subdirectory %q must stay within the repository", subdirectory)
	}
	return remote, subdirectory, reference, nil
}

func gitAddExecutable() (string, error) {
	if executable := os.Getenv(realGitExecutable); filepath.IsAbs(executable) {
		return executable, nil
	}
	executable, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("find Git executable for ADD: %w", err)
	}
	return executable, nil
}

func redactGitSource(source string) string {
	remote, fragment, hasFragment := strings.Cut(source, "#")
	parsed, err := url.Parse(remote)
	if err == nil && parsed.User != nil {
		_, hasPassword := parsed.User.Password()
		if isHTTPAddSource(remote) {
			parsed.User = url.User("REDACTED")
			remote = parsed.String()
		} else if hasPassword {
			parsed.User = url.User(parsed.User.Username())
			remote = parsed.String()
		}
	}
	if hasFragment {
		return remote + "#" + fragment
	}
	return remote
}

func gitAddOptions(options upstream.AddAndCopyOptions, contextDir string, keepGitDir *bool) upstream.AddAndCopyOptions {
	options.Checksum = ""
	options.ContextDir = contextDir
	options.IgnoreFile = ""
	if keepGitDir == nil || !*keepGitDir {
		options.Excludes = append(options.Excludes, ".git", ".git/**", "**/.git", "**/.git/**")
	}
	return options
}

func verifyGitCommit(repository, expected string) error {
	if len(expected) > sha256.Size*2 {
		return fmt.Errorf("checksum %q is longer than a Git commit SHA", expected)
	}
	if expected == "" || strings.IndexFunc(expected, func(character rune) bool {
		return (character < '0' || character > '9') && (character < 'a' || character > 'f') && (character < 'A' || character > 'F')
	}) >= 0 {
		return fmt.Errorf("checksum %q must be a hexadecimal commit prefix", expected)
	}
	commit, err := resolveGitCommit(repository)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(commit, strings.ToLower(expected)) {
		return fmt.Errorf("unexpected commit %s, want prefix %s", commit, expected)
	}
	return nil
}

func resolveGitCommit(repository string) (string, error) {
	head, err := os.ReadFile(filepath.Join(repository, ".git", "HEAD"))
	if err != nil {
		return "", fmt.Errorf("read cloned Git HEAD: %w", err)
	}
	commit := strings.TrimSpace(string(head))
	if reference, ok := strings.CutPrefix(commit, "ref: "); ok {
		resolved, err := os.ReadFile(filepath.Join(repository, ".git", filepath.FromSlash(reference)))
		if err != nil {
			return "", fmt.Errorf("resolve cloned Git HEAD %q: %w", reference, err)
		}
		commit = strings.TrimSpace(string(resolved))
	}
	if len(commit) != 40 && len(commit) != sha256.Size*2 || strings.IndexFunc(commit, func(character rune) bool {
		return (character < '0' || character > '9') && (character < 'a' || character > 'f') && (character < 'A' || character > 'F')
	}) >= 0 {
		return "", fmt.Errorf("invalid cloned Git commit %q", commit)
	}
	return strings.ToLower(commit), nil
}

func applyRemoteAddSource(ctx context.Context, builder operationBuilder, operation Add, options upstream.AddAndCopyOptions, source string) (retErr error) {
	directory, err := os.MkdirTemp("", "coopr-add-unpack-")
	if err != nil {
		return fmt.Errorf("create ADD download directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(directory); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove ADD download directory: %w", err))
		}
	}()

	name, err := remoteSourceName(source)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return fmt.Errorf("create ADD request %q: %w", source, err)
	}
	if err := setRemoteAddAuthorization(request, builder); err != nil {
		return fmt.Errorf("authorize ADD source %q: %w", source, err)
	}
	response, err := remoteAddHTTPClient(builder).Do(request) //nolint:gosec // ADD explicitly permits authored remote URLs.
	if err != nil {
		if errors.Is(err, errRemoteAddRedirectCredentials) {
			return errRemoteAddRedirectCredentials
		}
		return fmt.Errorf("download ADD source %q: %w", source, err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close ADD response body: %w", err))
		}
	}()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("download ADD source %q: HTTP status %s", source, response.Status)
	}

	file, err := os.OpenFile(filepath.Join(directory, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create downloaded ADD source: %w", err)
	}
	var writer io.Writer = file
	var verifier opencontainersdigest.Digester
	if operation.Checksum != "" {
		expected, err := opencontainersdigest.Parse(operation.Checksum)
		if err != nil {
			return errors.Join(fmt.Errorf("invalid ADD checksum: %w", err), file.Close())
		}
		if expected.Algorithm() != opencontainersdigest.SHA256 {
			return errors.Join(fmt.Errorf("ADD remote checksum must use sha256, got %s", expected.Algorithm()), file.Close())
		}
		verifier = expected.Algorithm().Digester()
		writer = io.MultiWriter(file, verifier.Hash())
	}
	_, copyErr := io.Copy(writer, response.Body)
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("download ADD source %q: %w", source, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close downloaded ADD source %q: %w", source, closeErr)
	}
	if modified := response.Header.Get("Last-Modified"); modified != "" {
		if parsed, err := http.ParseTime(modified); err == nil {
			if err := os.Chtimes(file.Name(), parsed, parsed); err != nil {
				return fmt.Errorf("preserve Last-Modified for ADD source %q: %w", source, err)
			}
		}
	}
	if operation.Checksum != "" {
		expected, _ := opencontainersdigest.Parse(operation.Checksum)
		if actual := verifier.Digest(); actual != expected {
			return fmt.Errorf("unexpected response digest for %q: %s, want %s", source, actual, expected)
		}
	}

	options.Checksum = ""
	options.ContextDir = directory
	options.IgnoreFile = ""
	extract := operation.Unpack != nil && *operation.Unpack
	return builder.add(operation.Destination, extract, options, name)
}

var errRemoteAddRedirectCredentials = errors.New("ADD redirect URL must not include credentials")

func remoteAddHTTPClient(builder any) *http.Client {
	return &http.Client{CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("ADD source exceeded 10 HTTP redirects")
		}
		if request.URL.User != nil {
			return errRemoteAddRedirectCredentials
		}
		// net/http can preserve sensitive headers for same-origin and subdomain
		// redirects. Always clear and select credentials for the destination host.
		request.Header.Del("Authorization")
		request.Header.Del("Referer")
		return setRemoteAddAuthorization(request, builder)
	}}
}

func setRemoteAddAuthorization(request *http.Request, builder any) error {
	provider, ok := builder.(addSourceCredentialProvider)
	if !ok {
		return nil
	}
	hostname := request.URL.Hostname()
	if hostname == "" {
		return nil
	}
	for _, candidate := range []struct {
		id    string
		token bool
	}{
		{id: httpAuthHeaderSecretPrefix + hostname},
		{id: httpAuthTokenSecretPrefix + hostname, token: true},
	} {
		value, found, err := provider.addSourceSecret(candidate.id)
		if err != nil {
			return fmt.Errorf("resolve secret %q: %w", candidate.id, err)
		}
		if !found {
			continue
		}
		authorization := string(value)
		if candidate.token {
			authorization = "Bearer " + authorization
		}
		request.Header.Set("Authorization", authorization)
	}
	return nil
}

func remoteSourceName(source string) (string, error) {
	parsed, err := url.Parse(source)
	if err != nil {
		return "", errors.New("invalid ADD source URL")
	}
	if parsed.User != nil {
		return "", errors.New("ADD source URL must not include credentials")
	}
	name := path.Base(parsed.Path)
	if name == "." || name == "/" || name == "" {
		return "", fmt.Errorf("ADD URL %q has no filename", source)
	}
	return name, nil
}
