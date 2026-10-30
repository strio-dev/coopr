package oci

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	gcrlayout "github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	gcrtypes "github.com/google/go-containerregistry/pkg/v1/types"
	imagecopy "go.podman.io/image/v5/copy"
	nativedocker "go.podman.io/image/v5/docker"
	nativeoci "go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/pkg/blobinfocache"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/types"
	"oras.land/oras-go/v2/content"
)

func TestArtifactRegistryUploadSessionRetryBudget(t *testing.T) {
	for _, test := range []struct {
		retries    uint
		persistent bool
	}{
		{retries: 0},
		{retries: 1},
		{retries: 1, persistent: true},
		{retries: 2, persistent: true},
	} {
		t.Run(fmt.Sprintf("retries=%d/persistent=%t", test.retries, test.persistent), func(t *testing.T) {
			backend := registryserver.New()
			var sessions atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/blobs/uploads/") {
					attempt := sessions.Add(1)
					if test.persistent || attempt == 1 {
						http.Error(w, "upload session temporarily unavailable", http.StatusServiceUnavailable)
						return
					}
				}
				backend.ServeHTTP(w, request)
			}))
			t.Cleanup(server.Close)
			resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true, Retry: test.retries, RetryDelay: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			repository := strings.TrimPrefix(server.URL, "http://") + "/coopr/session-budget"
			target, err := resolver.CacheRepository(repository)
			if err != nil {
				t.Fatal(err)
			}
			data := []byte("file-backed upload session retry content")
			descriptor := Descriptor("application/vnd.coopr.cache.layer.v1+tar", data)
			file, err := os.CreateTemp(t.TempDir(), "layer-*")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = file.Close() })
			if _, err := file.Write(data); err != nil {
				t.Fatal(err)
			}
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			err = target.Push(t.Context(), descriptor, file)
			wantSessions := int64(test.retries + 1)
			if !test.persistent && wantSessions > 2 {
				wantSessions = 2
			}
			if got := sessions.Load(); got != wantSessions {
				t.Fatalf("upload sessions = %d, want %d for one retry budget", got, wantSessions)
			}
			if test.persistent || test.retries == 0 {
				if err == nil {
					t.Fatal("failed upload session unexpectedly succeeded")
				}
				return
			}
			if err != nil {
				t.Fatalf("configured upload session retry did not recover: %v", err)
			}
			response, err := server.Client().Get(server.URL + "/v2/coopr/session-budget/blobs/" + descriptor.Digest.String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			got, err := content.ReadAll(response.Body, descriptor)
			if response.StatusCode != http.StatusOK || err != nil || !bytes.Equal(got, data) {
				t.Fatalf("retried upload content = %q, status=%d, error=%v", got, response.StatusCode, err)
			}
		})
	}
}

func TestArtifactRegistryCorruptUploadRejectedBeforeWrites(t *testing.T) {
	for _, readerKind := range []string{"memory", "file"} {
		for _, corruption := range []string{"same-size", "truncated"} {
			t.Run(readerKind+"/"+corruption, func(t *testing.T) {
				backend := registryserver.New()
				var writes atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					if request.Method != http.MethodGet && request.Method != http.MethodHead {
						writes.Add(1)
					}
					backend.ServeHTTP(w, request)
				}))
				t.Cleanup(server.Close)
				resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true, Retry: 1, RetryDelay: time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				target, err := resolver.CacheRepository(strings.TrimPrefix(server.URL, "http://") + "/coopr/verified-upload")
				if err != nil {
					t.Fatal(err)
				}
				data := []byte("verified upload payload")
				descriptor := Descriptor("application/vnd.coopr.cache.layer.v1+tar", data)
				bad := bytes.Clone(data)
				if corruption == "same-size" {
					bad[0] ^= 1
				} else {
					bad = bad[:len(bad)-1]
				}
				var reader io.Reader = bytes.NewReader(bad)
				if readerKind == "file" {
					file, err := os.CreateTemp(t.TempDir(), "corrupt-layer-*")
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = file.Close() })
					if _, err := file.Write(bad); err != nil {
						t.Fatal(err)
					}
					if _, err := file.Seek(0, io.SeekStart); err != nil {
						t.Fatal(err)
					}
					reader = file
				}
				if err := target.Push(t.Context(), descriptor, reader); err == nil {
					t.Fatal("corrupt upload succeeded")
				}
				if got := writes.Load(); got != 0 {
					t.Fatalf("corrupt upload made %d registry writes before validation", got)
				}
			})
		}
	}
}

func TestArtifactRegistryNamespaceProxyScopesQuery(t *testing.T) {
	metadata, paths := baleComponentMetadata(t, "amd64", "namespace-proxy")
	packageData, err := os.ReadFile(paths[metadata.Packages[0].Stage])
	if err != nil {
		t.Fatal(err)
	}
	packageDigest := metadata.Packages[0].Descriptor.Digest.String()
	var tokenRequests, redirectedReads, namespaceRequests atomic.Int64
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		tokenRequests.Add(1)
		if request.URL.Query().Get("existing") != "keep" || request.URL.Query().Has("ns") {
			t.Errorf("token realm query was changed: %s", request.URL.RawQuery)
		}
		username, password, ok := request.BasicAuth()
		if !ok || username != "tester" || password != "secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="token service"`)
			http.Error(w, "token credentials required", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "namespace-token", "expires_in": 3600})
	}))
	t.Cleanup(tokens.Close)
	downloads := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		redirectedReads.Add(1)
		if request.URL.Query().Get("existing") != "keep" || request.URL.Query().Has("ns") {
			t.Errorf("redirect target query was changed: %s", request.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", packageDigest)
		_, _ = w.Write(packageData)
	}))
	t.Cleanup(downloads.Close)
	backend := registryserver.New()
	var armed atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if armed.Load() {
			if request.URL.Path != "/v2/" {
				namespaceRequests.Add(1)
				if values := request.URL.Query()["ns"]; len(values) != 1 || values[0] != "source.example" {
					t.Errorf("proxy request lost namespace selection: %s", request.URL)
				}
			}
			if request.Header.Get("Authorization") != "Bearer namespace-token" {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="namespace-proxy",scope="repository:coopr/proxied:pull"`, tokens.URL+"/token?existing=keep"))
				http.Error(w, "bearer token required", http.StatusUnauthorized)
				return
			}
			if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/blobs/"+packageDigest) {
				http.Redirect(w, request, downloads.URL+"/package?existing=keep", http.StatusTemporaryRedirect)
				return
			}
		}
		backend.ServeHTTP(w, request)
	}))
	t.Cleanup(proxy.Close)
	proxyHost := strings.TrimPrefix(proxy.URL, "http://")
	seed, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	root, err := seed.PublishComponent(t.Context(), proxyHost+"/coopr/proxied:stable", metadata, paths)
	if err != nil {
		t.Fatal(err)
	}
	authFile := filepath.Join(t.TempDir(), "auth.json")
	credentials := fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, proxyHost, base64.StdEncoding.EncodeToString([]byte("tester:secret")))
	if err := os.WriteFile(authFile, []byte(credentials), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "registries.conf")
	configuration := fmt.Sprintf("[[registry]]\nlocation = \"source.example\"\n[[registry.mirror]]\nlocation = %q\ninsecure = true\nnamespace-proxy = \"source.example\"\n", proxyHost)
	if err := os.WriteFile(configPath, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := testResolver(t, Options{AuthFile: authFile, RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	resolver.system.SystemRegistriesConfPath = configPath
	resolver.system.SystemRegistriesConfDirPath = t.TempDir()
	armed.Store(true)
	resolved, err := resolver.Resolve(t.Context(), "source.example/coopr/proxied:stable", metadata.Platform, Component)
	if err != nil || !content.Equal(resolved.Root, root) {
		t.Fatalf("resolve through namespace proxy: %+v, %v", resolved, err)
	}
	output := filepath.Join(t.TempDir(), "package.tar")
	if err := resolver.Download(t.Context(), resolved, metadata.Packages[0].Descriptor, output); err != nil {
		t.Fatalf("download through namespace proxy: %v", err)
	}
	got, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(got, packageData) {
		t.Fatalf("namespace-proxied package changed: %v", err)
	}
	if namespaceRequests.Load() == 0 || tokenRequests.Load() == 0 || redirectedReads.Load() == 0 {
		t.Fatal("namespace routing, token realm and redirect were not all exercised")
	}
}

func TestArtifactRegistryCrossRepositoryBlobMount(t *testing.T) {
	type scenario struct {
		authentication string
		decline        bool
		failure        string
		retries        uint
	}
	var cases []scenario
	for _, authentication := range []string{"anonymous", "basic", "bearer-ping"} {
		for _, decline := range []bool{false, true} {
			cases = append(cases, scenario{authentication: authentication, decline: decline})
		}
	}
	for _, failure := range []string{"transient", "persistent"} {
		for _, retries := range []uint{0, 1} {
			cases = append(cases, scenario{authentication: "anonymous", decline: true, failure: failure, retries: retries})
		}
	}
	for _, test := range cases {
		authentication, decline := test.authentication, test.decline
		t.Run(fmt.Sprintf("auth=%s/declined=%t/failure=%s/retries=%d", authentication, decline, test.failure, test.retries), func(t *testing.T) {
			data := packageTar(t, "mounted-layer")
			layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(data)), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			image, err := mutate.AppendLayers(empty.Image, layer)
			if err != nil {
				t.Fatal(err)
			}
			image = mutate.MediaType(image, gcrtypes.OCIManifestSchema1)
			compressed, err := layer.Compressed()
			if err != nil {
				t.Fatal(err)
			}
			compressedData, err := io.ReadAll(compressed)
			_ = compressed.Close()
			if err != nil {
				t.Fatal(err)
			}
			pkg := Descriptor(string(gcrtypes.OCILayer), compressedData)
			layoutPath := t.TempDir()
			layout, err := gcrlayout.Write(layoutPath, empty.Index)
			if err != nil {
				t.Fatal(err)
			}
			if err := layout.AppendImage(image, gcrlayout.WithAnnotations(map[string]string{"org.opencontainers.image.ref.name": "seed"})); err != nil {
				t.Fatal(err)
			}
			backend := registryserver.New()
			var mu sync.Mutex
			linked := map[string]bool{}
			var mounts, secondUploads, consumerUploads, declinedMounts atomic.Int64
			var consumerOrigin, realm string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if authentication != "anonymous" {
					if request.URL.Path == "/token" {
						user, password, ok := request.BasicAuth()
						if !ok || user != "tester" || password != "secret" {
							http.Error(w, "invalid token credentials", http.StatusUnauthorized)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"token":"mount-token","expires_in":3600}`)
						return
					}
					valid := request.Header.Get("Authorization") == "Bearer mount-token"
					if authentication == "basic" {
						user, password, ok := request.BasicAuth()
						valid = ok && user == "tester" && password == "secret"
					}
					if !valid {
						if request.URL.Path == "/v2/" || authentication == "basic" {
							challenge := `Basic realm="mount-registry"`
							if authentication == "bearer-ping" {
								challenge = fmt.Sprintf(`Bearer realm=%q,service="mount-registry"`, realm)
							}
							w.Header().Set("WWW-Authenticate", challenge)
						}
						http.Error(w, "authentication required", http.StatusUnauthorized)
						return
					}
				}
				mu.Lock()
				defer mu.Unlock()
				for _, repository := range []string{"coopr/mount-source", "coopr/mount-destination", "coopr/mount-native-consumer"} {
					prefix := "/v2/" + repository + "/blobs/"
					if request.URL.Path == prefix+pkg.Digest.String() && (request.Method == http.MethodGet || request.Method == http.MethodHead) && !linked[repository] {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusNotFound)
						_, _ = io.WriteString(w, `{"errors":[{"code":"BLOB_UNKNOWN","message":"blob not linked to repository"}]}`)
						return
					}
					if request.Method == http.MethodPost && request.URL.Path == prefix+"uploads/" && request.URL.Query().Get("mount") == pkg.Digest.String() {
						if source := request.URL.Query().Get("from"); linked[source] {
							if decline && repository == "coopr/mount-destination" {
								declinedMounts.Add(1)
								response := httptest.NewRecorder()
								backend.ServeHTTP(response, request)
								if response.Code != http.StatusAccepted {
									t.Errorf("declined mount status=%d, want202", response.Code)
								}
								for name, values := range response.Header() {
									w.Header()[name] = values
								}
								w.WriteHeader(response.Code)
								_, _ = io.Copy(w, response.Body)
								return
							}
							linked[repository] = true
							mounts.Add(1)
							if repository == "coopr/mount-native-consumer" {
								consumerOrigin = source
							}
							w.Header().Set("Location", prefix+pkg.Digest.String())
							w.Header().Set("Docker-Content-Digest", pkg.Digest.String())
							w.WriteHeader(http.StatusCreated)
							return
						}
					}
					if request.Method == http.MethodPut && strings.HasPrefix(request.URL.Path, prefix+"uploads/") && request.URL.Query().Get("digest") == pkg.Digest.String() {
						if repository == "coopr/mount-destination" {
							attempt := secondUploads.Add(1)
							if test.failure == "persistent" || test.failure == "transient" && attempt == 1 {
								http.Error(w, "temporary declined-mount upload failure", http.StatusServiceUnavailable)
								return
							}
						}
						linked[repository] = true
						if repository == "coopr/mount-native-consumer" {
							consumerUploads.Add(1)
						}
					}
				}
				backend.ServeHTTP(w, request)
			}))
			t.Cleanup(server.Close)
			realm = server.URL + "/token"
			host := strings.TrimPrefix(server.URL, "http://")
			authFile := filepath.Join(t.TempDir(), "auth.json")
			auths := map[string]map[string]string{
				host:                              {"auth": base64.StdEncoding.EncodeToString([]byte("tester:secret"))},
				host + "/coopr/mount-destination": {"auth": base64.StdEncoding.EncodeToString([]byte("tester:secret"))},
			}
			writeAuth := func() {
				t.Helper()
				data, err := json.Marshal(map[string]any{"auths": auths})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(authFile, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			writeAuth()
			resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), AuthFile: authFile, RetrySet: true, Retry: test.retries, RetryDelay: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			resolver.system.BlobInfoCacheDir = t.TempDir()
			source, err := nativeoci.ParseReference(layoutPath + ":seed")
			if err != nil {
				t.Fatal(err)
			}
			destination, err := nativedocker.ParseReference("//" + host + "/coopr/mount-source:seed")
			if err != nil {
				t.Fatal(err)
			}
			policy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = policy.Destroy() }()
			if _, err := imagecopy.Image(t.Context(), policy, destination, source, &imagecopy.Options{SourceCtx: resolver.SystemContext(), DestinationCtx: resolver.SystemContext()}); err != nil {
				t.Fatalf("seed image and native blob cache: %v", err)
			}
			// Native reuse authenticates cached source probes as the destination.
			// A conflicting source credential must not change that principal.
			auths[host+"/coopr/mount-source"] = map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte("wrong:credential"))}
			writeAuth()
			target, err := resolver.CacheRepository(host + "/coopr/mount-destination")
			if err != nil {
				t.Fatal(err)
			}
			if exists, err := target.Exists(t.Context(), pkg); err != nil || exists {
				t.Fatalf("before mount: exists=%t error=%v", exists, err)
			}
			pushErr := target.Push(t.Context(), pkg, bytes.NewReader(compressedData))
			if test.failure != "" {
				if secondUploads.Load() != int64(test.retries+1) {
					t.Fatalf("declined mount PUT attempts=%d want%d", secondUploads.Load(), test.retries+1)
				}
				if test.failure == "persistent" || test.retries == 0 {
					if pushErr == nil {
						t.Fatal("failed declined mount upload escaped retry budget")
					}
					return
				}
			}
			if pushErr != nil {
				t.Fatalf("ORAS push with native cache: %v", pushErr)
			}
			wantMounts, wantUploads := int64(1), int64(0)
			if decline {
				wantMounts, wantUploads = 0, 1
			}
			if test.failure != "" {
				wantUploads = int64(test.retries + 1)
			}
			if mounts.Load() != wantMounts || secondUploads.Load() != wantUploads || (decline && test.failure == "" && declinedMounts.Load() != 1) {
				t.Fatalf("shared package transfer: mounts=%d destination payload uploads=%d; unexpected mount or upload transfer outcome", mounts.Load(), secondUploads.Load())
			}

			cache := blobinfocache.DefaultCache(resolver.SystemContext())
			scope := types.BICTransportScope{Opaque: host}
			found := false
			for _, candidate := range cache.CandidateLocations(nativedocker.Transport, scope, pkg.Digest, false) {
				if candidate.Digest == pkg.Digest && candidate.Location.Opaque == host+"/coopr/mount-destination" {
					found = true
				}
			}
			if !found {
				t.Fatal("ORAS mount did not record a native-readable destination location")
			}
			// Make the original source unavailable so a later native copy must use the
			// location recorded by the ORAS adapter to avoid uploading the payload.
			mu.Lock()
			linked["coopr/mount-source"] = false
			mu.Unlock()
			consumer, err := nativedocker.ParseReference("//" + host + "/coopr/mount-native-consumer:seed")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := imagecopy.Image(t.Context(), policy, consumer, source, &imagecopy.Options{SourceCtx: resolver.SystemContext(), DestinationCtx: resolver.SystemContext()}); err != nil {
				t.Fatalf("native consumes ORAS location: %v", err)
			}
			mu.Lock()
			origin := consumerOrigin
			mu.Unlock()
			if origin != "coopr/mount-destination" || mounts.Load() != wantMounts+1 || consumerUploads.Load() != 0 {
				t.Fatalf("native copy did not reuse ORAS-recorded location: origin=%q mounts=%d payloadUploads=%d", origin, mounts.Load(), consumerUploads.Load())
			}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/v2/coopr/mount-destination/blobs/"+pkg.Digest.String(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if authentication == "basic" {
				request.SetBasicAuth("tester", "secret")
			}
			if authentication == "bearer-ping" {
				request.Header.Set("Authorization", "Bearer mount-token")
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := content.ReadAll(response.Body, pkg); err != nil {
				t.Fatalf("mounted package integrity: %v", err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
