package oci

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestNativeImageRetryCoversMetadataResolution(t *testing.T) {
	backend := registryserver.New()
	var armed atomic.Bool
	var manifestRequests atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if armed.Load() && strings.Contains(r.URL.Path, "/manifests/") && manifestRequests.Add(1) == 1 {
			http.Error(w, "transient", http.StatusServiceUnavailable)
			return
		}
		backend.ServeHTTP(w, r)
	})
	server, repo, _, _ := testRepository(t, handler)
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	manifest, _ := imageManifest(t, repo, platform, "retry")
	stream, err := repo.Fetch(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.PushReference(context.Background(), manifest, stream, "retry"); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	armed.Store(true)
	host := strings.TrimPrefix(server.URL, "http://")
	resolver, err := NewResolver(Options{PlainHTTPRegistries: []string{host}, Retry: 3, RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolveRemoteImage(context.Background(), host+"/coopr/test:retry", platform); err != nil {
		t.Fatalf("retry-enabled resolution failed after %d manifest requests: %v", manifestRequests.Load(), err)
	}
}

func TestNativeRegistryRetryOptionsPreserveExplicitZero(t *testing.T) {
	options, err := RegistryRetryOptions(Options{RetrySet: true, Retry: 0, RetryDelay: 25 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if options.MaxRetry != 0 || options.Delay != 25*time.Millisecond {
		t.Fatalf("explicit retry options = %#v", options)
	}
}

func TestNativeImageRetryCoversConfigBlob(t *testing.T) {
	backend := registryserver.New()
	var armed atomic.Bool
	var configRequests atomic.Int32
	var configPath string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if armed.Load() && r.URL.Path == configPath && configRequests.Add(1) == 1 {
			http.Error(w, "transient", http.StatusServiceUnavailable)
			return
		}
		backend.ServeHTTP(w, r)
	})
	server, repo, _, _ := testRepository(t, handler)
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	manifest, _ := imageManifest(t, repo, platform, "config retry")
	configPath = "/v2/coopr/test/blobs/" + manifestConfigDigest(t, repo, manifest)
	if err := repo.Tag(context.Background(), manifest, "retry-config"); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	host := strings.TrimPrefix(server.URL, "http://")
	resolver, err := NewResolver(Options{PlainHTTPRegistries: []string{host}, Retry: 3, RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolveRemoteImage(context.Background(), host+"/coopr/test:retry-config", platform); err != nil {
		t.Fatalf("retry-enabled config resolution failed after %d requests: %v", configRequests.Load(), err)
	}
	if got := configRequests.Load(); got != 2 {
		t.Fatalf("config requests = %d, want 2", got)
	}
}

func TestNativeImageRetryCoversPolicyManifest(t *testing.T) {
	backend := registryserver.New()
	var armed atomic.Bool
	var configSeen atomic.Bool
	var policyRequests atomic.Int32
	var configPath string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if armed.Load() && r.URL.Path == configPath {
			backend.ServeHTTP(w, r)
			configSeen.Store(true)
			return
		}
		if armed.Load() && configSeen.Load() && strings.Contains(r.URL.Path, "/manifests/") {
			if policyRequests.Add(1) == 1 {
				http.Error(w, "transient", http.StatusServiceUnavailable)
				return
			}
		}
		backend.ServeHTTP(w, r)
	})
	server, repo, _, _ := testRepository(t, handler)
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	manifest, _ := imageManifest(t, repo, platform, "policy retry")
	configPath = "/v2/coopr/test/blobs/" + manifestConfigDigest(t, repo, manifest)
	if err := repo.Tag(context.Background(), manifest, "retry-policy"); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	host := strings.TrimPrefix(server.URL, "http://")
	resolver, err := NewResolver(Options{PlainHTTPRegistries: []string{host}, Retry: 3, RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolveRemoteImage(context.Background(), host+"/coopr/test:retry-policy", platform); err != nil {
		t.Fatalf("retry-enabled policy resolution failed after %d policy requests: %v", policyRequests.Load(), err)
	}
	if got := policyRequests.Load(); got != 2 {
		t.Fatalf("policy manifest requests = %d, want 2", got)
	}
	if !configSeen.Load() {
		t.Fatal("policy request was not observed after config resolution")
	}
}

func TestNativeImageExplicitZeroDisablesRetry(t *testing.T) {
	backend := registryserver.New()
	var armed atomic.Bool
	var manifestRequests atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if armed.Load() && strings.Contains(r.URL.Path, "/manifests/") {
			manifestRequests.Add(1)
			http.Error(w, "transient", http.StatusServiceUnavailable)
			return
		}
		backend.ServeHTTP(w, r)
	})
	server, repo, _, _ := testRepository(t, handler)
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	manifest, _ := imageManifest(t, repo, platform, "no retry")
	if err := repo.Tag(context.Background(), manifest, "no-retry"); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	host := strings.TrimPrefix(server.URL, "http://")
	resolver, err := NewResolver(Options{PlainHTTPRegistries: []string{host}, Retry: 0, RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolveRemoteImage(context.Background(), host+"/coopr/test:no-retry", platform); err == nil {
		t.Fatal("resolution unexpectedly succeeded with retries disabled")
	}
	if got := manifestRequests.Load(); got != 1 {
		t.Fatalf("manifest requests = %d, want 1", got)
	}
}

func manifestConfigDigest(t *testing.T, repo interface {
	Fetch(context.Context, v1.Descriptor) (io.ReadCloser, error)
}, descriptor v1.Descriptor) string {
	t.Helper()
	stream, err := repo.Fetch(context.Background(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(stream)
	if err := stream.Close(); readErr == nil {
		readErr = err
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest.Config.Digest.String()
}
