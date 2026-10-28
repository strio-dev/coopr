package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	nativeerrcode "github.com/docker/distribution/registry/api/errcode"
	"github.com/docker/distribution/registry/client/auth/challenge"
	"github.com/docker/go-connections/tlsconfig"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"go.podman.io/common/pkg/retry"
	"go.podman.io/image/v5/docker"
	nativereference "go.podman.io/image/v5/docker/reference"
	nativecredentials "go.podman.io/image/v5/pkg/docker/config"
	"go.podman.io/image/v5/pkg/sysregistriesv2"
	"go.podman.io/image/v5/pkg/tlsclientconfig"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage/pkg/configfile"
	"go.podman.io/storage/pkg/unshare"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"
	orasretry "oras.land/oras-go/v2/registry/remote/retry"
)

// orasArtifactRepository keeps native endpoint selection at the boundary while
// ORAS owns registry content operations. Its primary is always the write target.
type orasArtifactRepository struct {
	resolver *Resolver
	ref      registry.Reference
	mu       sync.Mutex
	primary  *remote.Repository
	sources  map[digest.Digest]orasReadSource
}

type orasEndpoint func(context.Context) (*remote.Repository, error)

type orasReadSource struct {
	repository *remote.Repository
	remaining  []orasEndpoint
}

type orasNamespaceClient struct {
	client    remote.Client
	host      string
	namespace string
}

func (c orasNamespaceClient) Do(request *http.Request) (*http.Response, error) {
	if request.URL.Host != c.host || !strings.HasPrefix(request.URL.Path, "/v2/") {
		return c.client.Do(request)
	}
	request = request.Clone(request.Context())
	query := request.URL.Query()
	query.Set("ns", c.namespace)
	request.URL.RawQuery = query.Encode()
	return c.client.Do(request)
}

// orasRegistryHeaders binds configured headers to a single repository client.
// The upstream HTTP client owns redirects and token exchanges.
type orasRegistryHeaders struct {
	base           http.RoundTripper
	userAgent      string
	origin         string
	repositoryPath string
	token          string
	pingChallenges []string
}

func (h *orasRegistryHeaders) apply(request *http.Request) *http.Request {
	request = request.Clone(request.Context())
	if h.userAgent != "" {
		request.Header.Set("User-Agent", h.userAgent)
	}
	if h.token != "" && h.initialRepositoryRequest(request) {
		request.Header.Set("Authorization", "Bearer "+h.token)
	}
	return request
}

func (h *orasRegistryHeaders) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := h.base.RoundTrip(h.apply(request))
	if err != nil || response.StatusCode != http.StatusUnauthorized || response.Header.Get("WWW-Authenticate") != "" || len(h.pingChallenges) == 0 || !h.initialRepositoryRequest(request) {
		return response, err
	}
	if response.Request != nil && !h.initialRepositoryRequest(response.Request) {
		return response, nil
	}
	// Native registries reuse the ping challenge when a resource omits it.
	// Seed the upstream auth parser without changing the response or its body.
	copied := *response
	copied.Header = response.Header.Clone()
	copied.Header[http.CanonicalHeaderKey("WWW-Authenticate")] = append([]string(nil), h.pingChallenges...)
	return &copied, nil
}

func (h *orasRegistryHeaders) initialRepositoryRequest(request *http.Request) bool {
	return request.Response == nil && request.URL.Scheme+"://"+request.URL.Host == h.origin && strings.HasPrefix(request.URL.Path, h.repositoryPath)
}

type orasHeaderClient struct {
	client  remote.Client
	headers *orasRegistryHeaders
}

func (c orasHeaderClient) Do(request *http.Request) (*http.Response, error) {
	return c.client.Do(c.headers.apply(request))
}

type orasPingClient struct {
	client     *http.Client
	bearer     bool
	challenges []string
}

func (c *orasPingClient) Do(request *http.Request) (*http.Response, error) {
	response, err := c.client.Do(request)
	c.bearer = false
	c.challenges = nil
	if err == nil {
		// Native ping selects the first supported challenge, including on 200.
		// Preserve its original value while the public parser owns the grammar.
		parsed := *response
		parsed.StatusCode = http.StatusUnauthorized
		for _, value := range response.Header.Values("WWW-Authenticate") {
			parsed.Header = http.Header{"Www-Authenticate": []string{value}}
			for _, candidate := range challenge.ResponseChallenges(&parsed) {
				if candidate.Scheme == "bearer" || candidate.Scheme == "basic" {
					c.bearer = candidate.Scheme == "bearer"
					c.challenges = []string{value}
					return response, nil
				}
			}
		}
	}
	return response, err
}

func isArtifactManifest(desc v1.Descriptor) bool {
	switch desc.MediaType {
	case v1.MediaTypeImageManifest, v1.MediaTypeImageIndex, dockerManifestType, dockerIndexType:
		return true
	}
	return false
}

var _ oras.Target = (*orasArtifactRepository)(nil)

func (r *Resolver) repository(ref registry.Reference) (*orasArtifactRepository, error) {
	return &orasArtifactRepository{resolver: r, ref: ref, sources: make(map[digest.Digest]orasReadSource)}, nil
}

func (r *orasArtifactRepository) primaryRepository(ctx context.Context) (*remote.Repository, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.primary != nil {
		return r.primary, nil
	}
	repo, err := r.resolver.orasRepository(ctx, r.ref.Registry+"/"+r.ref.Repository, nil, r.resolver.SystemContext(), 0)
	if err != nil {
		return nil, err
	}
	r.primary = repo
	return repo, nil
}

func (r *orasArtifactRepository) readRepositories(selector string) ([]orasEndpoint, error) {
	separator := ":"
	if digest.Digest(selector).Validate() == nil {
		separator = "@"
	}
	name, err := nativereference.ParseNormalizedNamed(r.ref.Registry + "/" + r.ref.Repository + separator + selector)
	if err != nil {
		return nil, err
	}
	system := r.resolver.SystemContext()
	configuration, err := sysregistriesv2.FindRegistry(system, name.Name())
	if err != nil {
		return nil, err
	}
	if configuration == nil {
		return []orasEndpoint{r.primaryRepository}, nil
	}
	if configuration.Blocked {
		return nil, fmt.Errorf("registry %s is blocked", configuration.Prefix)
	}
	endpoints, err := configuration.PullSourcesFromReference(name)
	if err != nil {
		return nil, err
	}
	var repositories []orasEndpoint
	for _, endpoint := range endpoints {
		endpointSystem := *system
		if nativereference.Domain(name) != nativereference.Domain(endpoint.Reference) {
			endpointSystem.DockerAuthConfig = nil
			endpointSystem.DockerBearerRegistryToken = ""
		}
		repositories = append(repositories, func(ctx context.Context) (*remote.Repository, error) {
			repo, err := r.resolver.orasRepository(ctx, endpoint.Reference.Name(), &endpoint.Endpoint.Insecure, &endpointSystem, 0)
			if err == nil && endpoint.Endpoint.NamespaceProxy != "" {
				repo.Client = orasNamespaceClient{client: repo.Client, host: repo.Reference.Host(), namespace: endpoint.Endpoint.NamespaceProxy}
			}
			return repo, err
		})
	}
	return repositories, nil
}

func (r *orasArtifactRepository) Resolve(ctx context.Context, selector string) (v1.Descriptor, error) {
	repositories, err := r.readRepositories(selector)
	if err != nil {
		return v1.Descriptor{}, err
	}
	var failures []error
	for index, open := range repositories {
		var repo *remote.Repository
		var descriptor v1.Descriptor
		err := retry.IfNecessary(ctx, func() error {
			var err error
			repo, err = open(ctx)
			if err != nil {
				return err
			}
			var reader io.ReadCloser
			descriptor, reader, err = repo.FetchReference(ctx, selector)
			if err != nil {
				return err
			}
			_, err = readArtifactMetadata(reader, descriptor)
			return err
		}, r.resolver.artifactRetryOptions())
		if err != nil {
			failures = append(failures, err)
			continue
		}
		r.mu.Lock()
		r.sources[descriptor.Digest] = orasReadSource{repository: repo, remaining: repositories[index+1:]}
		r.mu.Unlock()
		return descriptor, nil
	}
	return v1.Descriptor{}, errors.Join(failures...)
}

func (r *orasArtifactRepository) Fetch(ctx context.Context, descriptor v1.Descriptor) (io.ReadCloser, error) {
	if err := checkDescriptor(descriptor); err != nil {
		return nil, err
	}
	if isArtifactManifest(descriptor) && descriptor.Size > maxMetadataBytes {
		return nil, fmt.Errorf("metadata descriptor too large: %d bytes", descriptor.Size)
	}
	r.mu.Lock()
	selected := r.sources[descriptor.Digest]
	r.mu.Unlock()
	var repositories []orasEndpoint
	if selected.repository != nil {
		repositories = append([]orasEndpoint{func(context.Context) (*remote.Repository, error) { return selected.repository, nil }}, selected.remaining...)
	} else {
		selector := r.ref.Reference
		if selector == "" {
			selector = descriptor.Digest.String()
		}
		var err error
		repositories, err = r.readRepositories(selector)
		if err != nil {
			return nil, err
		}
	}
	var failures []error
	for index, open := range repositories {
		var repo *remote.Repository
		var reader io.ReadCloser
		var data []byte
		err := retry.IfNecessary(ctx, func() error {
			var err error
			repo, err = open(ctx)
			if err != nil {
				return err
			}
			reader, err = repo.Fetch(ctx, descriptor)
			if err != nil || !isArtifactManifest(descriptor) {
				return err
			}
			data, err = readArtifactMetadata(reader, descriptor)
			return err
		}, r.resolver.artifactRetryOptions())
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !isArtifactManifest(descriptor) {
			return reader, nil
		}
		var children []v1.Descriptor
		if descriptor.MediaType == v1.MediaTypeImageIndex || descriptor.MediaType == dockerIndexType {
			var index v1.Index
			if err := json.Unmarshal(data, &index); err != nil {
				return nil, err
			}
			children = index.Manifests
		} else {
			var manifest v1.Manifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				return nil, err
			}
			children = append(manifest.Layers, manifest.Config)
		}
		r.mu.Lock()
		for _, child := range children {
			r.sources[child.Digest] = orasReadSource{repository: repo, remaining: repositories[index+1:]}
		}
		r.mu.Unlock()
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	return nil, errors.Join(failures...)
}

// readArtifactMetadata bounds allocation before using the upstream size and
// digest verifier. ReadAll reads at most the validated size plus its EOF probe.
func readArtifactMetadata(reader io.ReadCloser, descriptor v1.Descriptor) ([]byte, error) {
	if err := checkDescriptor(descriptor); err != nil {
		return nil, errors.Join(err, reader.Close())
	}
	if descriptor.Size > maxMetadataBytes {
		return nil, errors.Join(fmt.Errorf("metadata descriptor too large: %d bytes", descriptor.Size), reader.Close())
	}
	data, err := content.ReadAll(reader, descriptor)
	return data, errors.Join(err, reader.Close())
}

func (r *orasArtifactRepository) Exists(ctx context.Context, descriptor v1.Descriptor) (bool, error) {
	if isArtifactManifest(descriptor) {
		// Visit the whole graph even when its root exists: a registry can retain
		// the manifest while a child blob needs to be republished.
		return false, nil
	}
	return (orasArtifactBlobs{resolver: r.resolver, ref: r.ref}).Exists(ctx, descriptor)
}

func (r *orasArtifactRepository) Push(ctx context.Context, descriptor v1.Descriptor, reader io.Reader) error {
	if !isArtifactManifest(descriptor) {
		return (orasArtifactBlobs{resolver: r.resolver, ref: r.ref}).Push(ctx, descriptor, reader)
	}
	file, err := spoolArtifact(reader, descriptor)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	defer func() { _ = file.Close() }()
	return retry.IfNecessary(ctx, func() error {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		// Manifest publication owns one retry budget; request retries stay disabled.
		repo, err := r.resolver.orasRepository(ctx, r.ref.Registry+"/"+r.ref.Repository, nil, r.resolver.SystemContext(), 0)
		if err != nil {
			return err
		}
		return repo.Push(ctx, descriptor, io.NopCloser(file))
	}, r.resolver.artifactRetryOptions())
}

func (r *Resolver) artifactRetryOptions() *retry.Options {
	options := *r.retryOptions
	options.IsErrorRetryable = orasArtifactRetryable
	return &options
}

func orasArtifactRetryable(err error) bool {
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		for _, cause := range joined.Unwrap() {
			if !orasArtifactRetryable(cause) {
				return false
			}
		}
		return true
	}
	var response *errcode.ErrorResponse
	if errors.As(err, &response) {
		// Native registries interpret error payloads only for client errors.
		if response.StatusCode < 400 || response.StatusCode >= 500 {
			return retry.IsErrorRetryable(docker.UnexpectedHTTPStatusError{StatusCode: response.StatusCode})
		}
		if len(response.Errors) != 0 {
			first := response.Errors[0]
			return retry.IsErrorRetryable(nativeerrcode.ParseErrorCode(first.Code).WithMessage(first.Message))
		}
		return retry.IsErrorRetryable(docker.UnexpectedHTTPStatusError{StatusCode: response.StatusCode})
	}
	// Native HTTP operations see a url.Error from http.Client.Do. Preserve that
	// classification for transport errors, including a bare connection EOF.
	return retry.IsErrorRetryable(&url.Error{Op: "registry request", Err: err})
}

func (r *orasArtifactRepository) Tag(ctx context.Context, descriptor v1.Descriptor, selector string) error {
	return retry.IfNecessary(ctx, func() error {
		repo, err := r.primaryRepository(ctx)
		if err != nil {
			return err
		}
		return repo.Tag(ctx, descriptor, selector)
	}, r.resolver.artifactRetryOptions())
}

func (r *Resolver) orasRepository(ctx context.Context, name string, endpointInsecure *bool, system *types.SystemContext, requestRetries int) (*remote.Repository, error) {
	repo, err := remote.NewRepository(name)
	if err != nil {
		return nil, err
	}
	client, credential, plainHTTP, err := r.orasConnection(ctx, name, endpointInsecure, system, requestRetries)
	if err != nil {
		return nil, err
	}
	authClient := &auth.Client{Client: client, Cache: auth.NewCache(), Credential: credential}
	authClient.SetUserAgent(system.DockerRegistryUserAgent)
	repo.Client = orasHeaderClient{client: authClient, headers: client.Transport.(*orasRegistryHeaders)}
	repo.PlainHTTP = plainHTTP
	repo.MaxMetadataBytes = maxMetadataBytes
	repo.SkipReferrersGC = true
	return repo, nil
}

func (r *Resolver) baleClient(ctx context.Context, reference string) (*bale.Client, error) {
	ref, err := registry.ParseReference(reference)
	if err != nil {
		return nil, err
	}
	client, credential, plainHTTP, err := r.orasConnection(ctx, ref.Registry+"/"+ref.Repository, nil, r.SystemContext(), r.retryOptions.MaxRetry)
	if err != nil {
		return nil, err
	}
	options := bale.ClientOptions{HTTPClient: client, Credentials: credential}
	if plainHTTP {
		options.PlainHTTPRegistries = []string{ref.Registry}
	}
	return bale.NewClient(options)
}

func (r *Resolver) orasConnection(ctx context.Context, name string, endpointInsecure *bool, system *types.SystemContext, requestRetries int) (*http.Client, auth.CredentialFunc, bool, error) {
	named, err := nativereference.ParseNormalizedNamed(name)
	if err != nil {
		return nil, nil, false, err
	}
	configuration, err := sysregistriesv2.FindRegistry(system, named.Name())
	if err != nil {
		return nil, nil, false, err
	}
	insecure := false
	if configuration != nil {
		if configuration.Blocked {
			return nil, nil, false, fmt.Errorf("registry %s is blocked", configuration.Prefix)
		}
		insecure = configuration.Insecure
	}
	if endpointInsecure != nil {
		insecure = *endpointInsecure
	}
	if system.DockerInsecureSkipTLSVerify != types.OptionalBoolUndefined {
		insecure = system.DockerInsecureSkipTLSVerify == types.OptionalBoolTrue
	}
	tlsConfiguration := tlsconfig.ClientDefault()
	if system.BaseTLSConfig != nil {
		tlsConfiguration = system.BaseTLSConfig.Clone()
	}
	certDirectory, err := orasCertificateDirectory(system, nativereference.Domain(named))
	if err != nil {
		return nil, nil, false, err
	}
	if certDirectory != "" {
		if err := tlsclientconfig.SetupCertificates(certDirectory, tlsConfiguration); err != nil {
			return nil, nil, false, err
		}
	}
	tlsConfiguration.InsecureSkipVerify = insecure
	transport := tlsclientconfig.NewTransport()
	transport.TLSClientConfig = tlsConfiguration
	if system.DockerProxyURL != nil {
		transport.Proxy = http.ProxyURL(system.DockerProxyURL)
	}
	if system.DockerProxy != nil {
		transport.Proxy = func(request *http.Request) (*url.URL, error) { return system.DockerProxy(request.URL) }
	}
	var base http.RoundTripper = transport
	if requestRetries > 0 {
		base = orasMetadataTransport{base: transport, repository: registry.Reference{Registry: nativereference.Domain(named), Repository: nativereference.Path(named)}}
	}
	base = orasRateLimitTransport{base: base, host: (registry.Reference{Registry: nativereference.Domain(named)}).Host()}
	client := &http.Client{Transport: &orasretry.Transport{Base: base, Policy: func() orasretry.Policy {
		backoff := orasretry.ExponentialBackoff(time.Second, 2, 0.1)
		if r.retryOptions.Delay != 0 {
			backoff = func(int, *http.Response) time.Duration { return r.retryOptions.Delay }
		}
		return &orasretry.GenericPolicy{MaxRetry: requestRetries, Retryable: orasHTTPRetryable, Backoff: backoff, MaxWait: time.Duration(1<<63 - 1)}
	}}}
	if requestRetries > 0 {
		client.Transport = orasReplayTransport{base: client.Transport}
	}
	headers := &orasRegistryHeaders{base: client.Transport, userAgent: system.DockerRegistryUserAgent}
	client.Transport = headers
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if headers.token == "" || len(via) == 0 || !strings.HasPrefix(via[0].URL.Path, headers.repositoryPath) {
			return nil
		}
		// Do not restore credentials once any hop has left the configured origin.
		// ORAS applies the same boundary before invoking this public callback.
		for _, hop := range append(via, request) {
			if hop.URL.Scheme+"://"+hop.URL.Host != headers.origin {
				return nil
			}
		}
		request.Header.Set("Authorization", "Bearer "+headers.token)
		return nil
	}
	credentials, err := nativecredentials.GetCredentialsForRef(system, named)
	if err != nil {
		return nil, nil, false, err
	}
	credential := auth.StaticCredential(nativereference.Domain(named), auth.Credential{
		Username: credentials.Username, Password: credentials.Password, RefreshToken: credentials.IdentityToken, AccessToken: system.DockerBearerRegistryToken,
	})
	probe, err := remote.NewRegistry(nativereference.Domain(named))
	if err != nil {
		return nil, nil, false, err
	}
	ping := &orasPingClient{client: client}
	probe.Client = ping
	err = probe.Ping(ctx)
	if orasPingSucceeded(err) {
		headers.origin = "https://" + probe.Reference.Host()
		headers.repositoryPath = "/v2/" + nativereference.Path(named) + "/"
		headers.pingChallenges = ping.challenges
		if ping.bearer {
			headers.token = system.DockerBearerRegistryToken
		}
		return client, credential, false, nil
	}
	if ctx.Err() != nil || !insecure {
		return nil, nil, false, err
	}
	probe.PlainHTTP = true
	if err := probe.Ping(ctx); !orasPingSucceeded(err) {
		return nil, nil, false, err
	}
	headers.origin = "http://" + probe.Reference.Host()
	headers.repositoryPath = "/v2/" + nativereference.Path(named) + "/"
	headers.pingChallenges = ping.challenges
	if ping.bearer {
		headers.token = system.DockerBearerRegistryToken
	}
	return client, credential, true, nil
}

func orasPingSucceeded(err error) bool {
	if err == nil {
		return true
	}
	var response *errcode.ErrorResponse
	return errors.As(err, &response) && response.StatusCode == http.StatusUnauthorized
}

func orasCertificateDirectory(system *types.SystemContext, host string) (string, error) {
	if system.DockerCertPath != "" {
		return system.DockerCertPath, nil
	}
	if system.DockerPerHostCertDirPath != "" {
		return filepath.Join(system.DockerPerHostCertDirPath, host), nil
	}
	paths, err := configfile.GetSearchPaths(&configfile.File{
		Name: "certs", Extension: "d", DoNotUseExtensionForConfigName: true,
		UserId: unshare.GetRootlessUID(), RootForImplicitAbsolutePaths: system.RootForImplicitAbsolutePaths,
	})
	if err != nil {
		return "", err
	}
	for _, directory := range append(paths.DropInDirectories, filepath.Join(system.RootForImplicitAbsolutePaths, "/etc/docker/certs.d")) {
		candidate := filepath.Join(directory, host)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrPermission) {
			return "", err
		}
	}
	return "", nil
}
