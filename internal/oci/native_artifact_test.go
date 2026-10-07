package oci

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	"go.podman.io/image/v5/types"
)

func boolOption(value bool) *bool { return &value }
func TestArtifactTLSVerifyPolicy(t *testing.T) {
	for _, tlsServer := range []bool{false, true} {
		for _, verify := range []bool{false, true} {
			for _, trusted := range []bool{false, true} {
				if !tlsServer && trusted {
					continue
				}
				t.Run(stringName(tlsServer, verify, trusted), func(t *testing.T) {
					handler := registryserver.New()
					var httpsRequests int
					wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.TLS != nil {
							httpsRequests++
						}
						handler.ServeHTTP(w, r)
					})
					server := httptest.NewServer(wrapped)
					if tlsServer {
						server.Close()
						server = httptest.NewTLSServer(wrapped)
					}
					defer server.Close()
					opts := Options{TLSVerify: boolOption(verify), RetrySet: true, Retry: 0}
					if trusted {
						opts.CertDir = t.TempDir()
						cert := server.Certificate()
						if _, err := x509.ParseCertificate(cert.Raw); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(opts.CertDir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0600); err != nil {
							t.Fatal(err)
						}
					}
					resolver, err := NewResolver(opts)
					if err != nil {
						t.Fatal(err)
					}
					meta, paths := publicationMetadata(t, nil, false)
					name := strings.TrimPrefix(strings.TrimPrefix(server.URL, "http://"), "https://") + "/test/component:stable"
					root, err := resolver.PublishComponent(context.Background(), name, meta, paths)
					expect := !verify || tlsServer && trusted
					if !expect {
						if err == nil {
							t.Fatal("strict policy accepted insecure registry")
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					resolved, err := resolver.Resolve(context.Background(), name, meta.Platform, Component)
					if err != nil || resolved.Root.Digest != root.Digest {
						t.Fatalf("round trip: %v", err)
					}
					if tlsServer && httpsRequests == 0 {
						t.Fatal("HTTPS registry did not use HTTPS")
					}
				})
			}
		}
	}
}
func stringName(tlsServer, verify, trusted bool) string {
	var parts []string
	if tlsServer {
		parts = append(parts, "https")
	} else {
		parts = append(parts, "http")
	}
	if verify {
		parts = append(parts, "strict")
	} else {
		parts = append(parts, "insecure")
	}
	if trusted {
		parts = append(parts, "trusted")
	}
	return strings.Join(parts, "/")
}
func TestTLSVerifyOptionalMapping(t *testing.T) {
	for _, item := range []struct {
		input *bool
		want  types.OptionalBool
	}{{nil, types.OptionalBoolUndefined}, {boolOption(true), types.OptionalBoolFalse}, {boolOption(false), types.OptionalBoolTrue}} {
		system := &types.SystemContext{}
		ApplyTLSVerify(system, item.input)
		if system.DockerInsecureSkipTLSVerify != item.want {
			t.Fatalf("mapping: %v", system.DockerInsecureSkipTLSVerify)
		}
		resolver, err := NewResolver(Options{TLSVerify: item.input})
		if err != nil {
			t.Fatal(err)
		}
		returned := resolver.RegistryOptions().TLSVerify
		if item.input == nil {
			if returned != nil {
				t.Fatal("omitted TLS policy became explicit")
			}
		} else if returned == nil || *returned != *item.input {
			t.Fatal("worker TLS policy changed")
		}
	}
}

func TestArtifactNativeRegistryConfiguration(t *testing.T) {
	for _, mode := range []string{"insecure", "blocked", "remap", "mirror"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(registryserver.New())
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "http://")
			seed, err := NewResolver(Options{TLSVerify: boolOption(false), RetrySet: true})
			if err != nil {
				t.Fatal(err)
			}
			meta, paths := publicationMetadata(t, packageTar(t, "configured"), true)
			root, err := seed.PublishComponent(context.Background(), host+"/test/component:stable", meta, paths)
			if err != nil {
				t.Fatal(err)
			}
			logical := host + "/test/component"
			conf := fmt.Sprintf("[[registry]]\nlocation = %q\ninsecure = true\n", host)
			switch mode {
			case "blocked":
				conf += "blocked = true\n"
			case "remap":
				logical = "configured.example/test/component"
				conf = fmt.Sprintf("[[registry]]\nprefix = %q\nlocation = %q\ninsecure = true\n", "configured.example", host)
			case "mirror":
				logical = "configured.example/test/component"
				conf = fmt.Sprintf("[[registry]]\nlocation = %q\n[[registry.mirror]]\nlocation = %q\ninsecure = true\n", "configured.example", host)
			}
			configPath := filepath.Join(t.TempDir(), "registries.conf")
			if err := os.WriteFile(configPath, []byte(conf), 0600); err != nil {
				t.Fatal(err)
			}
			resolver, err := NewResolver(Options{RetrySet: true})
			if err != nil {
				t.Fatal(err)
			}
			resolver.system.SystemRegistriesConfPath = configPath
			resolver.system.SystemRegistriesConfDirPath = t.TempDir()
			resolved, err := resolver.Resolve(context.Background(), logical+":stable", meta.Platform, Component)
			if mode == "blocked" {
				if err == nil || !strings.Contains(err.Error(), "blocked") {
					t.Fatalf("blocked resolve: %v", err)
				}
				_, err = resolver.PublishComponent(context.Background(), logical+":other", meta, paths)
				if err == nil || !strings.Contains(err.Error(), "blocked") {
					t.Fatalf("blocked publish: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Root.Digest != root.Digest {
				t.Fatal("configuration changed artifact")
			}
			file := filepath.Join(t.TempDir(), "package.tar")
			if err = resolver.Download(context.Background(), resolved, meta.Packages[0].Descriptor, file); err != nil {
				t.Fatal(err)
			}
			if mode == "insecure" {
				_, err = resolver.PublishComponent(context.Background(), logical+":other", meta, paths)
				if err != nil {
					t.Fatal(err)
				}
				strict, err := NewResolver(Options{TLSVerify: boolOption(true), RetrySet: true})
				if err != nil {
					t.Fatal(err)
				}
				strict.system.SystemRegistriesConfPath = configPath
				strict.system.SystemRegistriesConfDirPath = t.TempDir()
				if _, err = strict.Resolve(context.Background(), logical+":stable", meta.Platform, Component); err == nil {
					t.Fatal("explicit strict did not override insecure config")
				}
			}
		})
	}
}

func TestNativeCacheTargetPreservesRawEnvelope(t *testing.T) {
	server := httptest.NewServer(registryserver.New())
	defer server.Close()
	resolver, err := NewResolver(Options{TLSVerify: boolOption(false), RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	target, err := resolver.CacheRepository(strings.TrimPrefix(server.URL, "http://") + "/cache/test")
	if err != nil {
		t.Fatal(err)
	}
	configData := []byte(`{"custom":"cache"}`)
	blobData := []byte("cache payload")
	config := Descriptor("application/vnd.coopr.cache.config.v1+json", configData)
	blob := Descriptor("application/vnd.coopr.cache.layer.v1+tar", blobData)
	for _, item := range []struct {
		desc v1.Descriptor
		data []byte
	}{{config, configData}, {blob, blobData}} {
		if err := target.Push(context.Background(), item.desc, bytes.NewReader(item.data)); err != nil {
			t.Fatal(err)
		}
	}
	manifestData, err := json.Marshal(VersionedManifest(config, []v1.Descriptor{blob}, "application/vnd.coopr.cache.v1+json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := Descriptor(v1.MediaTypeImageManifest, manifestData)
	if err = target.Push(context.Background(), manifest, bytes.NewReader(manifestData)); err != nil {
		t.Fatal(err)
	}
	indexData, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, ArtifactType: "application/vnd.coopr.cache.index.v1+json", Manifests: []v1.Descriptor{manifest}, Annotations: map[string]string{"custom": "retained"}})
	if err != nil {
		t.Fatal(err)
	}
	index := Descriptor(v1.MediaTypeImageIndex, indexData)
	if err = target.Push(context.Background(), index, bytes.NewReader(indexData)); err != nil {
		t.Fatal(err)
	}
	if err = target.Tag(context.Background(), index, "stable"); err != nil {
		t.Fatal(err)
	}
	desc, err := target.Resolve(context.Background(), "stable")
	if err != nil || !sameDescriptor(desc, index) {
		t.Fatalf("index resolve: %+v %v", desc, err)
	}
	for _, item := range []struct {
		desc v1.Descriptor
		data []byte
	}{{index, indexData}, {manifest, manifestData}, {config, configData}, {blob, blobData}} {
		exists, err := target.Exists(context.Background(), item.desc)
		if err != nil || exists != !isArtifactManifest(item.desc) {
			t.Fatalf("exists %s: %v %v", item.desc.MediaType, exists, err)
		}
		stream, err := target.Fetch(context.Background(), item.desc)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(stream)
		if closeErr := stream.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil || !bytes.Equal(data, item.data) {
			t.Fatalf("raw content changed: %s %v", item.desc.MediaType, err)
		}
	}
	missing := Descriptor(v1.MediaTypeImageManifest, []byte("missing"))
	if exists, err := target.Exists(context.Background(), missing); err != nil || exists {
		t.Fatalf("missing manifest: %v %v", exists, err)
	}
	missingBlob := Descriptor("application/vnd.coopr.cache.layer.v1+tar", []byte("missing"))
	if exists, err := target.Exists(context.Background(), missingBlob); err != nil || exists {
		t.Fatalf("missing blob: %v %v", exists, err)
	}
	if err := target.Push(context.Background(), blob, bytes.NewReader([]byte("corrupt"))); err == nil {
		t.Fatal("corrupt blob upload accepted")
	}

}

func TestArtifactReadRetriesUseNativeControls(t *testing.T) {
	for _, maxRetry := range []uint{0, 1} {
		for _, fault := range []string{"manifest", "blob"} {
			t.Run(fmt.Sprintf("%s/%d", fault, maxRetry), func(t *testing.T) {
				backend := registryserver.New()
				var armed atomic.Bool
				var failures atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					match := r.Method == http.MethodGet && (fault == "manifest" && strings.HasSuffix(r.URL.Path, "/manifests/stable") || fault == "blob" && strings.Contains(r.URL.Path, "/blobs/"))
					if armed.Load() && match && failures.Add(1) == 1 {
						http.Error(w, "transient", http.StatusServiceUnavailable)
						return
					}
					backend.ServeHTTP(w, r)
				}))
				defer server.Close()
				resolver, err := NewResolver(Options{TLSVerify: boolOption(false), RetrySet: true, Retry: maxRetry, RetryDelay: time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				meta, paths := publicationMetadata(t, nil, false)
				name := strings.TrimPrefix(server.URL, "http://") + "/test/component:stable"
				if _, err = resolver.PublishComponent(context.Background(), name, meta, paths); err != nil {
					t.Fatal(err)
				}
				armed.Store(true)
				_, err = resolver.Resolve(context.Background(), name, meta.Platform, Component)
				if maxRetry == 0 {
					if err == nil || failures.Load() != 1 {
						t.Fatalf("zero retry: %d %v", failures.Load(), err)
					}
				} else if err != nil || failures.Load() < 2 {
					t.Fatalf("retry: %d %v", failures.Load(), err)
				}
			})
		}
	}
}

func TestPublicationDoesNotUseMirrorPresenceAsDestinationPresence(t *testing.T) {
	ctx := context.Background()
	primary := httptest.NewServer(registryserver.New())
	defer primary.Close()
	var mirrorReads atomic.Int32
	mirrorBackend := registryserver.New()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/manifests/") {
			mirrorReads.Add(1)
		}
		mirrorBackend.ServeHTTP(w, r)
	}))
	defer mirror.Close()
	primaryHost := strings.TrimPrefix(primary.URL, "http://")
	mirrorHost := strings.TrimPrefix(mirror.URL, "http://")
	resolver, err := NewResolver(Options{TLSVerify: boolOption(false), RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	meta, paths := publicationMetadata(t, packageTar(t, "mirror-only"), true)
	layout := t.TempDir()
	root, err := WriteComponentLayout(ctx, layout, meta, paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = resolver.PublishLayout(ctx, mirrorHost+"/test/component:stable", layout, root); err != nil {
		t.Fatal(err)
	}
	conf := fmt.Sprintf("[[registry]]\nlocation = %q\ninsecure = true\n[[registry.mirror]]\nlocation = %q\ninsecure = true\n", primaryHost, mirrorHost)
	resolver.system.SystemRegistriesConfPath = writeRegistryConfig(t, conf)
	resolver.system.SystemRegistriesConfDirPath = t.TempDir()
	if _, err = resolver.PublishLayout(ctx, primaryHost+"/test/component:stable", layout, root); err != nil {
		t.Fatal(err)
	}
	if mirrorReads.Load() != 0 {
		t.Fatalf("publication consulted pull mirrors: %d reads", mirrorReads.Load())
	}
	// Read the original endpoint without mirror configuration to prove the complete graph arrived.
	direct, err := NewResolver(Options{TLSVerify: boolOption(false), RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := direct.Resolve(ctx, primaryHost+"/test/component:stable", meta.Platform, Component)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Root.Digest != root.Digest {
		t.Fatal("published root changed")
	}
	if err = direct.Download(ctx, resolved, meta.Packages[0].Descriptor, filepath.Join(t.TempDir(), "package.tar")); err != nil {
		t.Fatal(err)
	}
}

func writeRegistryConfig(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registries.conf")
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConcurrentCacheGraphsKeepOwnMirrorBlobAnchors(t *testing.T) {
	ctx := context.Background()
	var mirrors []string
	var roots []v1.Descriptor
	var configs []v1.Descriptor
	for i := 0; i < 2; i++ {
		server := httptest.NewServer(registryserver.New())
		t.Cleanup(server.Close)
		host := strings.TrimPrefix(server.URL, "http://")
		mirrors = append(mirrors, host)
		seed, err := NewResolver(Options{TLSVerify: boolOption(false), RetrySet: true})
		if err != nil {
			t.Fatal(err)
		}
		target, err := seed.CacheRepository(host + "/cache/test")
		if err != nil {
			t.Fatal(err)
		}
		data := []byte(fmt.Sprintf(`{"graph":%d}`, i))
		config := Descriptor("application/vnd.coopr.cache.config.v1+json", data)
		if err = target.Push(ctx, config, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		manifestData, err := json.Marshal(VersionedManifest(config, nil, "application/vnd.coopr.cache.v1+json"))
		if err != nil {
			t.Fatal(err)
		}
		root := Descriptor(v1.MediaTypeImageManifest, manifestData)
		if err = target.Push(ctx, root, bytes.NewReader(manifestData)); err != nil {
			t.Fatal(err)
		}
		if err = target.Tag(ctx, root, fmt.Sprintf("graph-%d", i)); err != nil {
			t.Fatal(err)
		}
		roots = append(roots, root)
		configs = append(configs, config)
	}
	primary := httptest.NewServer(registryserver.New())
	defer primary.Close()
	host := strings.TrimPrefix(primary.URL, "http://")
	resolver, err := NewResolver(Options{RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	resolver.system.SystemRegistriesConfPath = writeRegistryConfig(t, fmt.Sprintf("[[registry]]\nlocation = %q\ninsecure = true\n[[registry.mirror]]\nlocation = %q\ninsecure = true\n[[registry.mirror]]\nlocation = %q\ninsecure = true\n", host, mirrors[0], mirrors[1]))
	resolver.system.SystemRegistriesConfDirPath = t.TempDir()
	target, err := resolver.CacheRepository(host + "/cache/test")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		root, err := target.Resolve(ctx, fmt.Sprintf("graph-%d", i))
		if err != nil || !sameDescriptor(root, roots[i]) {
			t.Fatalf("resolve graph %d: %v", i, err)
		}
	}
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			stream, err := target.Fetch(ctx, configs[i])
			if err == nil {
				data, readErr := io.ReadAll(stream)
				closeErr := stream.Close()
				err = errors.Join(readErr, closeErr)
				if !bytes.Equal(data, []byte(fmt.Sprintf(`{"graph":%d}`, i))) {
					err = fmt.Errorf("graph %d content changed", i)
				}
			}
			done <- err
		}(i)
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
