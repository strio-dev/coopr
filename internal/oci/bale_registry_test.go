package oci

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
)

const baleRegistryDependency = "registry.example/components/child:stable"

func publishBaleRegistryComponent(t *testing.T, mode string, resolver *Resolver, target string, metadata ComponentMetadata, paths map[string]string) (v1.Descriptor, error) {
	t.Helper()
	if mode == "direct" {
		return resolver.PublishComponent(t.Context(), target, metadata, paths)
	}
	layout := t.TempDir()
	root, err := WriteComponentLayout(t.Context(), layout, metadata, paths)
	if err != nil {
		return root, err
	}
	_, err = resolver.PublishLayout(t.Context(), target, layout, root)
	return root, err
}

func baleRegistryTestClient(t *testing.T, resolver *Resolver, repository string) *bale.Client {
	t.Helper()
	client, err := resolver.baleClient(t.Context(), repository)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func assertBaleRegistryDeclaration(t *testing.T, client *bale.Client, repository string, subject v1.Descriptor) v1.Descriptor {
	t.Helper()
	declarations, err := client.InspectDependencies(t.Context(), repository+"@"+subject.Digest.String())
	if err != nil || len(declarations) != 1 {
		t.Fatalf("dependency declarations = %+v, %v; want one", declarations, err)
	}
	declaration := declarations[0]
	if !content.Equal(declaration.Subject, subject) || len(declaration.Declaration.Dependencies) != 1 || declaration.Declaration.Dependencies[0].Ref != baleRegistryDependency {
		t.Fatalf("declaration subject or tagged dependency changed: %+v", declaration)
	}
	return declaration.Descriptor
}

func TestBaleRegistryComponentPublicationAuthentication(t *testing.T) {
	for _, mode := range []string{"direct", "layout"} {
		for _, test := range []struct {
			name        string
			tls         bool
			credentials bool
			customCA    bool
			perHostCA   bool
		}{
			{name: "authfile-custom-ca", tls: true, customCA: true},
			{name: "authfile-discovered-ca", tls: true, perHostCA: true},
			{name: "credentials-override-authfile", tls: true, credentials: true, customCA: true},
			{name: "insecure-https-first", tls: true, credentials: true},
			{name: "insecure-http-fallback"},
		} {
			t.Run(mode+"/"+test.name, func(t *testing.T) {
				registry := registryserver.New()
				var httpsRequests, referrerAPIRequests atomic.Int64
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					if request.TLS != nil {
						httpsRequests.Add(1)
					}
					if strings.Contains(request.URL.Path, "/referrers/") {
						referrerAPIRequests.Add(1)
					}
					user, password, ok := request.BasicAuth()
					if !ok || user != "tester" || password != "secret" {
						w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
					registry.ServeHTTP(w, request)
				}))
				if test.tls {
					server.StartTLS()
				} else {
					server.Start()
				}
				t.Cleanup(server.Close)
				host := strings.TrimPrefix(strings.TrimPrefix(server.URL, "https://"), "http://")
				authFile := filepath.Join(t.TempDir(), "auth.json")
				authValue := "tester:secret"
				if test.credentials {
					authValue = "unused:incorrect"
				}
				if err := os.WriteFile(authFile, []byte(fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, base64.StdEncoding.EncodeToString([]byte(authValue)))), 0o600); err != nil {
					t.Fatal(err)
				}
				opts := Options{AuthFile: authFile, RetrySet: true, Retry: 0, TLSVerify: boolOption(test.customCA || test.perHostCA)}
				if test.credentials {
					opts.Credentials = "tester:secret"
				}
				certRoot := t.TempDir()
				if test.customCA || test.perHostCA {
					hostCertDir := filepath.Join(certRoot, host)
					if err := os.MkdirAll(hostCertDir, 0o700); err != nil {
						t.Fatal(err)
					}
					certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
					if err := os.WriteFile(filepath.Join(hostCertDir, "ca.crt"), certificate, 0o600); err != nil {
						t.Fatal(err)
					}
					if test.customCA {
						opts.CertDir = hostCertDir
					}
				}
				resolver, err := testResolver(t, opts)
				if err != nil {
					t.Fatal(err)
				}
				if test.perHostCA {
					resolver.system.DockerPerHostCertDirPath = certRoot
				}
				metadata, paths := baleComponentMetadata(t, "amd64", "authenticated", baleRegistryDependency)
				repository := host + "/coopr/authenticated"
				root, err := publishBaleRegistryComponent(t, mode, resolver, repository+":stable", metadata, paths)
				if err != nil {
					t.Fatal(err)
				}
				assertBaleRegistryDeclaration(t, baleRegistryTestClient(t, resolver, repository), repository, root)
				if test.tls && httpsRequests.Load() == 0 {
					t.Fatal("HTTPS publication did not use TLS")
				}
				if referrerAPIRequests.Load() == 0 {
					t.Fatal("artifact client did not probe the native referrers endpoint")
				}
			})
		}
	}
}

func TestBaleRegistryComponentPublicationNativePolicy(t *testing.T) {
	for _, mode := range []string{"direct", "layout"} {
		for _, policy := range []string{"insecure", "blocked", "strict-override"} {
			t.Run(mode+"/"+policy, func(t *testing.T) {
				registry := registryserver.New()
				var writes atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					if request.Method != http.MethodGet && request.Method != http.MethodHead {
						writes.Add(1)
					}
					registry.ServeHTTP(w, request)
				}))
				t.Cleanup(server.Close)
				host := strings.TrimPrefix(server.URL, "http://")
				config := fmt.Sprintf("[[registry]]\nlocation = %q\ninsecure = true\n", host)
				if policy == "blocked" {
					config += "blocked = true\n"
				}
				configPath := filepath.Join(t.TempDir(), "registries.conf")
				if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
					t.Fatal(err)
				}
				opts := Options{RetrySet: true, Retry: 0}
				if policy == "strict-override" {
					opts.TLSVerify = boolOption(true)
				}
				resolver, err := testResolver(t, opts)
				if err != nil {
					t.Fatal(err)
				}
				resolver.system.SystemRegistriesConfPath = configPath
				resolver.system.SystemRegistriesConfDirPath = t.TempDir()
				metadata, paths := baleComponentMetadata(t, "amd64", "configured", baleRegistryDependency)
				repository := host + "/coopr/configured"
				root, err := publishBaleRegistryComponent(t, mode, resolver, repository+":stable", metadata, paths)
				if policy != "insecure" {
					if err == nil {
						t.Fatal("native registry restriction was ignored")
					}
					if policy == "blocked" && !strings.Contains(err.Error(), "blocked") {
						t.Fatalf("wrong blocked-registry error: %v", err)
					}
					if writes.Load() != 0 {
						t.Fatal("rejected publication wrote registry content")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				assertBaleRegistryDeclaration(t, baleRegistryTestClient(t, resolver, repository), repository, root)
			})
		}
	}
}

func TestBaleRegistryDependencyInventoryFailureRepairsOnRetry(t *testing.T) {
	for _, mode := range []string{"direct", "layout"} {
		t.Run(mode, func(t *testing.T) {
			registry := registryserver.New()
			var fail atomic.Bool
			var failedPuts atomic.Int64
			_, _, _, name := testRepository(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if fail.Load() && request.Method == http.MethodPut && strings.Contains(request.URL.Path, "/manifests/sha256-") {
					failedPuts.Add(1)
					http.Error(w, "inventory temporarily unavailable", http.StatusServiceUnavailable)
					return
				}
				registry.ServeHTTP(w, request)
			}))
			resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true, Retry: 0})
			if err != nil {
				t.Fatal(err)
			}
			metadata, paths := baleComponentMetadata(t, "amd64", "original", baleRegistryDependency)
			target := name + ":stable"
			original, err := publishBaleRegistryComponent(t, mode, resolver, target, metadata, paths)
			if err != nil {
				t.Fatal(err)
			}
			client := baleRegistryTestClient(t, resolver, name)
			assertBaleRegistryDeclaration(t, client, name, original)
			metadata, paths = baleComponentMetadata(t, "amd64", "replacement", baleRegistryDependency)
			layout := t.TempDir()
			expected, err := WriteComponentLayout(t.Context(), layout, metadata, paths)
			if err != nil {
				t.Fatal(err)
			}
			request, err := bale.PrepareDependencyDeclaration(t.Context(), bale.Root{Repository: name, Descriptor: expected}, bale.DependencyDeclaration{Dependencies: []bale.DependencyReference{{Ref: baleRegistryDependency}}})
			if err != nil {
				t.Fatal(err)
			}
			fail.Store(true)
			published, err := publishBaleRegistryComponent(t, mode, resolver, target, metadata, paths)
			if err == nil || !strings.Contains(err.Error(), "published=true, discoverable=false") {
				t.Fatalf("wrong partial-publication result: %+v, %v", published, err)
			}
			if mode == "direct" && !content.Equal(published, expected) {
				t.Fatalf("partial publication lost its component descriptor: %+v, want %+v", published, expected)
			}
			if failedPuts.Load() == 0 {
				t.Fatal("inventory failure was not exercised")
			}
			for _, descriptor := range []v1.Descriptor{expected, request.Descriptor} {
				found, err := client.Resolve(t.Context(), name+"@"+descriptor.Digest.String())
				if err != nil || !content.Equal(found.Descriptor, descriptor) {
					t.Fatalf("reported published content missing: %+v, %v", found.Descriptor, err)
				}
			}
			current, err := client.Resolve(t.Context(), target)
			if err != nil || !content.Equal(current.Descriptor, original) {
				t.Fatalf("failed inventory publication moved the target tag: %+v, %v", current.Descriptor, err)
			}
			declarations, err := client.InspectDependencies(t.Context(), name+"@"+expected.Digest.String())
			if err != nil || len(declarations) != 0 {
				t.Fatalf("failed inventory was reported discoverable: %+v, %v", declarations, err)
			}
			fail.Store(false)
			retried, err := publishBaleRegistryComponent(t, mode, resolver, target, metadata, paths)
			if err != nil || !content.Equal(retried, expected) {
				t.Fatalf("retry changed component identity or failed: %+v, %v", retried, err)
			}
			declaration := assertBaleRegistryDeclaration(t, client, name, expected)
			if !content.Equal(declaration, request.Descriptor) {
				t.Fatalf("repair changed attachment identity: %+v, want %+v", declaration, request.Descriptor)
			}
			current, err = client.Resolve(t.Context(), target)
			if err != nil || !content.Equal(current.Descriptor, expected) {
				t.Fatalf("confirmed retry did not move target tag: %+v, %v", current.Descriptor, err)
			}
		})
	}
}

func TestBaleRegistryPublicationPreservesPrimaryInventoryWithStaleMirror(t *testing.T) {
	for _, mode := range []string{"direct", "layout"} {
		for _, failPrimaryHead := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/head-failure=%t", mode, failPrimaryHead), func(t *testing.T) {
				primaryBackend := registryserver.New()
				var failHead atomic.Bool
				var inventoryWrites atomic.Int64
				primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					if failHead.Load() && request.Method == http.MethodHead && strings.Contains(request.URL.Path, "/manifests/") {
						http.Error(w, "primary unavailable", http.StatusServiceUnavailable)
						return
					}
					if request.Method == http.MethodPut && strings.Contains(request.URL.Path, "/manifests/sha256-") {
						inventoryWrites.Add(1)
					}
					primaryBackend.ServeHTTP(w, request)
				}))
				t.Cleanup(primary.Close)
				var mirrorReads atomic.Int64
				mirrorBackend := registryserver.New()
				mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					if (request.Method == http.MethodGet || request.Method == http.MethodHead) && strings.Contains(request.URL.Path, "/manifests/sha256-") {
						mirrorReads.Add(1)
					}
					mirrorBackend.ServeHTTP(w, request)
				}))
				t.Cleanup(mirror.Close)
				primaryHost := strings.TrimPrefix(primary.URL, "http://")
				mirrorHost := strings.TrimPrefix(mirror.URL, "http://")
				repository := primaryHost + "/coopr/mirrored"
				mirrorRepository := mirrorHost + "/coopr/mirrored"
				resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true, Retry: 0})
				if err != nil {
					t.Fatal(err)
				}
				metadata, paths := baleComponentMetadata(t, "amd64", "mirrored", baleRegistryDependency)
				root, err := publishBaleRegistryComponent(t, mode, resolver, repository+":stable", metadata, paths)
				if err != nil {
					t.Fatal(err)
				}
				client := baleHTTPClient(t, primaryHost, mirrorHost)
				automatic := assertBaleRegistryDeclaration(t, client, repository, root)
				snapshot, err := client.Resolve(t.Context(), repository+"@"+root.Digest.String())
				if err != nil {
					t.Fatal(err)
				}
				plan, err := client.Plan(t.Context(), bale.TransferRequest{Roots: []bale.Root{snapshot}, Destination: mirrorHost})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := client.Execute(t.Context(), plan); err != nil {
					t.Fatal(err)
				}
				assertBaleRegistryDeclaration(t, client, mirrorRepository, root)
				additional, err := bale.PrepareDependencyDeclaration(t.Context(), snapshot, bale.DependencyDeclaration{Context: "additional", Dependencies: []bale.DependencyReference{{Ref: baleRegistryDependency}}})
				if err != nil {
					t.Fatal(err)
				}
				attached, err := client.Attach(t.Context(), additional)
				if err != nil || !attached.Published || !attached.Discoverable {
					t.Fatalf("attach primary-only declaration: %+v, %v", attached, err)
				}
				before, err := client.InspectDependencies(t.Context(), repository+"@"+root.Digest.String())
				if err != nil || len(before) != 2 {
					t.Fatalf("primary inventory before publication = %+v, %v; want two declarations", before, err)
				}
				resolver.system.SystemRegistriesConfPath = writeRegistryConfig(t, fmt.Sprintf("[[registry]]\nlocation = %q\ninsecure = true\n[[registry.mirror]]\nlocation = %q\ninsecure = true\n", primaryHost, mirrorHost))
				resolver.system.SystemRegistriesConfDirPath = t.TempDir()
				mirrorReads.Store(0)
				writesBefore := inventoryWrites.Load()
				failHead.Store(failPrimaryHead)
				_, err = publishBaleRegistryComponent(t, mode, resolver, repository+":stable", metadata, paths)
				failHead.Store(false)
				if failPrimaryHead {
					if err == nil {
						t.Error("publication succeeded despite primary HEAD failure")
					}
					if inventoryWrites.Load() != writesBefore {
						t.Error("publication mutated primary inventory after primary HEAD failure")
					}
				} else if err != nil {
					t.Fatal(err)
				}
				declarations, err := client.InspectDependencies(t.Context(), repository+"@"+root.Digest.String())
				if err != nil {
					t.Fatal(err)
				}
				found := map[string]bool{}
				for _, declaration := range declarations {
					found[declaration.Descriptor.Digest.String()] = true
				}
				if len(declarations) != 2 || !found[automatic.Digest.String()] || !found[additional.Descriptor.Digest.String()] {
					t.Errorf("primary inventory lost a declaration: got %+v; want %s and %s", declarations, automatic.Digest, additional.Descriptor.Digest)
				}
				if mirrorReads.Load() != 0 {
					t.Errorf("publication read mutable referrer inventory from stale pull mirror: %d reads", mirrorReads.Load())
				}
			})
		}
	}
}

func TestBaleRegistryResolveRejectsDigestSelectorHeadMismatch(t *testing.T) {
	backend := registryserver.New()
	var mismatch atomic.Bool
	var root v1.Descriptor
	var mismatchedHeads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if mismatch.Load() && request.Method == http.MethodHead && strings.HasSuffix(request.URL.Path, "/manifests/"+root.Digest.String()) {
			mismatchedHeads.Add(1)
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("0", 64))
			w.Header().Set("Content-Type", root.MediaType)
			w.Header().Set("Content-Length", fmt.Sprint(root.Size))
			w.WriteHeader(http.StatusOK)
			return
		}
		backend.ServeHTTP(w, request)
	}))
	t.Cleanup(server.Close)
	repositoryName := strings.TrimPrefix(server.URL, "http://") + "/coopr/mismatch"
	resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true, Retry: 0})
	if err != nil {
		t.Fatal(err)
	}
	metadata, paths := baleComponentMetadata(t, "amd64", "digest-mismatch", baleRegistryDependency)
	root, err = resolver.PublishComponent(t.Context(), repositoryName+":stable", metadata, paths)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := resolver.orasRepository(t.Context(), repositoryName, nil, resolver.SystemContext(), 0)
	if err != nil {
		t.Fatal(err)
	}
	mismatch.Store(true)
	if descriptor, err := repository.Resolve(t.Context(), root.Digest.String()); err == nil {
		t.Fatalf("accepted contradictory digest-selector HEAD: %+v", descriptor)
	}
	if mismatchedHeads.Load() == 0 {
		t.Fatal("digest-selector resolution did not verify primary HEAD")
	}
}

func TestBaleRegistryCanceledDependencyDiscovery(t *testing.T) {
	backend := registryserver.New()
	var armed atomic.Bool
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if armed.Load() {
			requests.Add(1)
		}
		backend.ServeHTTP(w, request)
	}))
	t.Cleanup(server.Close)
	resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	repository := strings.TrimPrefix(server.URL, "http://") + "/coopr/canceled-discovery"
	metadata, paths := baleComponentMetadata(t, "amd64", "canceled", baleRegistryDependency)
	root, err := resolver.PublishComponent(t.Context(), repository+":stable", metadata, paths)
	if err != nil {
		t.Fatal(err)
	}
	client := baleRegistryTestClient(t, resolver, repository)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	armed.Store(true)
	if _, err := client.InspectDependencies(ctx, repository+"@"+root.Digest.String()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled dependency discovery: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("canceled discovery contacted registry")
	}
}
