package oci

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
)

func TestArtifactRegistryPublicationDoesNotListTags(t *testing.T) {
	for _, mode := range []string{"direct", "layout"} {
		for _, nativeReferrers := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/native-referrers=%t", mode, nativeReferrers), func(t *testing.T) {
				backend := registryserver.New()
				if nativeReferrers {
					backend = artifactRegistryNativeReferrersFixture(t)
				}
				var tagLists, referrerReads, inventoryWrites atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					if strings.HasSuffix(request.URL.Path, "/tags/list") {
						tagLists.Add(1)
						http.Error(w, "tag listing denied", http.StatusForbidden)
						return
					}
					if strings.Contains(request.URL.Path, "/referrers/") {
						referrerReads.Add(1)
					}
					if request.Method == http.MethodPut && strings.Contains(request.URL.Path, "/manifests/sha256-") {
						inventoryWrites.Add(1)
					}
					backend.ServeHTTP(w, request)
				}))
				t.Cleanup(server.Close)
				host := strings.TrimPrefix(server.URL, "http://")
				resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true})
				if err != nil {
					t.Fatal(err)
				}
				metadata, paths := baleComponentMetadata(t, "amd64", "no-tag-list", baleRegistryDependency)
				repository := host + "/coopr/no-tag-list"
				root, err := publishBaleRegistryComponent(t, mode, resolver, repository+":stable", metadata, paths)
				if err != nil {
					t.Fatalf("publish with tag listing denied: %v", err)
				}
				assertBaleRegistryDeclaration(t, baleHTTPClient(t, host), repository, root)
				if got := tagLists.Load(); got != 0 {
					t.Fatalf("publication/inspection listed repository tags %d times", got)
				}
				if referrerReads.Load() == 0 {
					t.Fatal("publication/inspection never checked referrer capability")
				}
				if nativeReferrers && inventoryWrites.Load() != 0 {
					t.Fatal("native-referrers registry received a fallback inventory write")
				}
				if !nativeReferrers && inventoryWrites.Load() == 0 {
					t.Fatal("registry without referrers never received a fallback inventory")
				}
			})
		}
	}
}

func artifactRegistryNativeReferrersFixture(t *testing.T) http.Handler {
	t.Helper()
	backend := registryserver.New(registryserver.WithReferrersSupport(true))
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		referrers := strings.Contains(request.URL.Path, "/referrers/")
		manifestPush := request.Method == http.MethodPut && strings.Contains(request.URL.Path, "/manifests/")
		if !referrers && !manifestPush {
			backend.ServeHTTP(w, request)
			return
		}
		response := httptest.NewRecorder()
		backend.ServeHTTP(response, request)
		data := response.Body.Bytes()
		if manifestPush && response.Code == http.StatusCreated {
			// The fixture also omits OCI-Subject when it indexes a subject-bearing
			// manifest. Advertise that actual processing so ORAS can select the
			// native API instead of correctly falling back to a tag inventory.
			manifestResponse := httptest.NewRecorder()
			manifestRequest := httptest.NewRequest(http.MethodGet, request.URL.Path, nil).WithContext(request.Context())
			backend.ServeHTTP(manifestResponse, manifestRequest)
			var envelope v1.Manifest
			if err := json.Unmarshal(manifestResponse.Body.Bytes(), &envelope); manifestResponse.Code != http.StatusOK || err != nil {
				t.Errorf("read pushed fixture envelope: status=%d, error=%v", manifestResponse.Code, err)
				http.Error(w, "fixture pushed envelope unavailable", http.StatusInternalServerError)
				return
			}
			if envelope.Subject != nil {
				response.Header().Set("OCI-Subject", envelope.Subject.Digest.String())
			}
		}
		if referrers && response.Code == http.StatusOK {
			// go-containerregistry v0.22.1's test registry incorrectly uses
			// config.mediaType for every referrer artifactType. Correct only
			// that response hint from the actual stored envelope, preserving
			// Bale's strict conflict validation and the real registry graph.
			var index v1.Index
			if err := json.Unmarshal(data, &index); err != nil {
				t.Errorf("decode fixture referrers: %v", err)
				http.Error(w, "fixture referrer decode failed", http.StatusInternalServerError)
				return
			}
			repositoryPath, _, _ := strings.Cut(request.URL.Path, "/referrers/")
			for position, descriptor := range index.Manifests {
				manifestResponse := httptest.NewRecorder()
				manifestRequest := httptest.NewRequest(http.MethodGet, repositoryPath+"/manifests/"+descriptor.Digest.String(), nil).WithContext(request.Context())
				backend.ServeHTTP(manifestResponse, manifestRequest)
				var envelope v1.Manifest
				if err := json.Unmarshal(manifestResponse.Body.Bytes(), &envelope); manifestResponse.Code != http.StatusOK || err != nil {
					t.Errorf("read fixture referrer envelope: status=%d, error=%v", manifestResponse.Code, err)
					http.Error(w, "fixture envelope unavailable", http.StatusInternalServerError)
					return
				}
				index.Manifests[position].ArtifactType = envelope.ArtifactType
				if envelope.ArtifactType == "" {
					index.Manifests[position].ArtifactType = envelope.Config.MediaType
				}
			}
			var err error
			data, err = json.Marshal(index)
			if err != nil {
				t.Errorf("encode fixture referrers: %v", err)
				http.Error(w, "fixture referrer encode failed", http.StatusInternalServerError)
				return
			}
		}
		for name, values := range response.Header() {
			w.Header()[name] = values
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		if referrers && response.Code == http.StatusOK {
			w.Header().Set("Docker-Content-Digest", Descriptor(v1.MediaTypeImageIndex, data).Digest.String())
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(data)
	})
}

func TestArtifactRegistryDefaultCertificateDiscovery(t *testing.T) {
	for _, mode := range []string{"direct", "layout"} {
		for _, directory := range []string{"containers", "docker"} {
			t.Run(mode+"/"+directory, func(t *testing.T) {
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				server := httptest.NewTLSServer(registryserver.New())
				t.Cleanup(server.Close)
				host := strings.TrimPrefix(server.URL, "https://")
				implicitRoot := t.TempDir()
				newResolver := func() *Resolver {
					resolver, err := testResolver(t, Options{TLSVerify: boolOption(true), RetrySet: true})
					if err != nil {
						t.Fatal(err)
					}
					resolver.system.RootForImplicitAbsolutePaths = implicitRoot
					return resolver
				}
				metadata, paths := baleComponentMetadata(t, "amd64", "default-ca", baleRegistryDependency)
				repository := host + "/coopr/default-ca"
				if _, err := publishBaleRegistryComponent(t, mode, newResolver(), repository+":stable", metadata, paths); err == nil {
					t.Fatal("private-CA registry accepted before its default trust directory existed")
				}
				certDir := filepath.Join(implicitRoot, "etc", directory, "certs.d", host)
				if err := os.MkdirAll(certDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(certDir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
					t.Fatal(err)
				}
				resolver := newResolver()
				root, err := publishBaleRegistryComponent(t, mode, resolver, repository+":stable", metadata, paths)
				if err != nil {
					t.Fatalf("publish using default %s certificate discovery: %v", directory, err)
				}
				resolved, err := resolver.Resolve(t.Context(), repository+":stable", metadata.Platform, Component)
				if err != nil || !content.Equal(resolved.Root, root) {
					t.Fatalf("resolve using default certificate discovery: %+v, %v", resolved, err)
				}
				client, err := bale.NewClient(bale.ClientOptions{HTTPClient: server.Client()})
				if err != nil {
					t.Fatal(err)
				}
				assertBaleRegistryDeclaration(t, client, repository, root)
			})
		}
	}
}

func TestArtifactRegistryClientCertificate(t *testing.T) {
	for _, mode := range []string{"direct", "layout"} {
		t.Run(mode, func(t *testing.T) {
			clientCertificate, certificatePEM, keyPEM, clientRoots := artifactRegistryClientCertificate(t)
			var authenticatedWrites atomic.Int64
			backend := registryserver.New()
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.TLS == nil || len(request.TLS.VerifiedChains) == 0 {
					t.Error("registry received request without verified client certificate")
				}
				if request.Method == http.MethodPut || request.Method == http.MethodPost {
					authenticatedWrites.Add(1)
				}
				backend.ServeHTTP(w, request)
			}))
			server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientRoots}
			server.StartTLS()
			t.Cleanup(server.Close)
			host := strings.TrimPrefix(server.URL, "https://")
			certDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(certDir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
				t.Fatal(err)
			}
			newResolver := func() *Resolver {
				resolver, err := testResolver(t, Options{CertDir: certDir, TLSVerify: boolOption(true), RetrySet: true})
				if err != nil {
					t.Fatal(err)
				}
				return resolver
			}
			metadata, paths := baleComponentMetadata(t, "amd64", "client-ca", baleRegistryDependency)
			repository := host + "/coopr/client-ca"
			if _, err := publishBaleRegistryComponent(t, mode, newResolver(), repository+":stable", metadata, paths); err == nil {
				t.Fatal("mTLS registry accepted publication without a client certificate")
			}
			if authenticatedWrites.Load() != 0 {
				t.Fatal("unauthenticated publication wrote registry content")
			}
			if err := os.WriteFile(filepath.Join(certDir, "client.cert"), certificatePEM, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(certDir, "client.key"), keyPEM, 0o600); err != nil {
				t.Fatal(err)
			}
			resolver := newResolver()
			root, err := publishBaleRegistryComponent(t, mode, resolver, repository+":stable", metadata, paths)
			if err != nil {
				t.Fatalf("publish with client certificate: %v", err)
			}
			if authenticatedWrites.Load() == 0 {
				t.Fatal("successful publication did not exercise mTLS writes")
			}
			resolved, err := resolver.Resolve(t.Context(), repository+":stable", metadata.Platform, Component)
			if err != nil || !content.Equal(resolved.Root, root) {
				t.Fatalf("resolve with client certificate: %+v, %v", resolved, err)
			}
			transport := server.Client().Transport.(*http.Transport).Clone()
			transport.TLSClientConfig = transport.TLSClientConfig.Clone()
			transport.TLSClientConfig.Certificates = []tls.Certificate{clientCertificate}
			client, err := bale.NewClient(bale.ClientOptions{HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			assertBaleRegistryDeclaration(t, client, repository, root)
		})
	}
}

func artifactRegistryClientCertificate(t *testing.T) (tls.Certificate, []byte, []byte, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Coopr test client CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	client := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Coopr test client"}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	clientDER, err := x509.CreateCertificate(rand.Reader, client, ca, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	return pair, certificatePEM, keyPEM, roots
}

func TestArtifactRegistryCredentialHelperPrecedence(t *testing.T) {
	for _, mode := range []string{"direct", "layout"} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/explicit=%t", mode, explicit), func(t *testing.T) {
				backend := registryserver.New()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					username, password, ok := request.BasicAuth()
					if !ok || username != "tester" || password != "secret" {
						w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
					backend.ServeHTTP(w, request)
				}))
				t.Cleanup(server.Close)
				host := strings.TrimPrefix(server.URL, "http://")
				helperDir := t.TempDir()
				secret := "secret"
				if explicit {
					secret = "wrong"
				}
				shell, err := exec.LookPath("sh")
				if err != nil {
					t.Fatal(err)
				}
				script := fmt.Sprintf("#!%s\ncase \"$1\" in\nget) cat >/dev/null; printf '%%s' '{\"Username\":\"tester\",\"Secret\":%q}';;\n*) exit 1;;\nesac\n", shell, secret)
				if err := os.WriteFile(filepath.Join(helperDir, "docker-credential-coopr-artifact-test"), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
				authFile := filepath.Join(t.TempDir(), "auth.json")
				data := fmt.Sprintf(`{"auths":{%q:{"auth":%q}},"credHelpers":{%q:"coopr-artifact-test"}}`, host, base64.StdEncoding.EncodeToString([]byte("unused:incorrect")), host)
				if err := os.WriteFile(authFile, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
				opts := Options{AuthFile: authFile, TLSVerify: boolOption(false), RetrySet: true}
				if explicit {
					opts.Credentials = "tester:secret"
				}
				resolver, err := testResolver(t, opts)
				if err != nil {
					t.Fatal(err)
				}
				metadata, paths := baleComponentMetadata(t, "amd64", "helper-auth", baleRegistryDependency)
				root, err := publishBaleRegistryComponent(t, mode, resolver, host+"/coopr/helper:stable", metadata, paths)
				if err != nil {
					t.Fatalf("publish with credential helper: %v", err)
				}
				resolved, err := resolver.Resolve(t.Context(), host+"/coopr/helper:stable", metadata.Platform, Component)
				if err != nil || !content.Equal(resolved.Root, root) {
					t.Fatalf("resolve with credential helper: %+v, %v", resolved, err)
				}
			})
		}
	}
}

func TestArtifactRegistryRepositoryScopedCredentials(t *testing.T) {
	for _, mode := range []string{"direct", "layout"} {
		t.Run(mode, func(t *testing.T) {
			backend := registryserver.New()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				password := ""
				switch {
				case strings.HasPrefix(request.URL.Path, "/v2/coopr/first/"):
					password = "first-secret"
				case strings.HasPrefix(request.URL.Path, "/v2/coopr/second/"):
					password = "second-secret"
				}
				username, supplied, ok := request.BasicAuth()
				ping := request.URL.Path == "/v2/" && (supplied == "first-secret" || supplied == "second-secret")
				if !ok || username != "tester" || (!ping && (password == "" || supplied != password)) {
					w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
					http.Error(w, "repository credentials required", http.StatusUnauthorized)
					return
				}
				backend.ServeHTTP(w, request)
			}))
			t.Cleanup(server.Close)
			host := strings.TrimPrefix(server.URL, "http://")
			authFile := filepath.Join(t.TempDir(), "auth.json")
			data := fmt.Sprintf(`{"auths":{%q:{"auth":%q},%q:{"auth":%q},%q:{"auth":%q}}}`, host,
				base64.StdEncoding.EncodeToString([]byte("unused:incorrect")), host+"/coopr/first",
				base64.StdEncoding.EncodeToString([]byte("tester:first-secret")), host+"/coopr/second",
				base64.StdEncoding.EncodeToString([]byte("tester:second-secret")))
			if err := os.WriteFile(authFile, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			resolver, err := testResolver(t, Options{AuthFile: authFile, TLSVerify: boolOption(false), RetrySet: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"first", "second", "first"} {
				metadata, paths := baleComponentMetadata(t, "amd64", name, baleRegistryDependency)
				reference := host + "/coopr/" + name + ":stable"
				root, err := publishBaleRegistryComponent(t, mode, resolver, reference, metadata, paths)
				if err != nil {
					t.Fatalf("publish %s with repository-scoped credentials: %v", name, err)
				}
				resolved, err := resolver.Resolve(t.Context(), reference, metadata.Platform, Component)
				if err != nil || !content.Equal(resolved.Root, root) {
					t.Fatalf("resolve %s with repository-scoped credentials: %+v, %v", name, resolved, err)
				}
			}
		})
	}
}

func TestArtifactRegistryBearerRealmUsesRegistryTrust(t *testing.T) {
	for _, mode := range []string{"direct", "layout"} {
		t.Run(mode, func(t *testing.T) {
			artifactRegistryBearerRealm(t, mode, true)
		})
	}
}

func TestArtifactRegistryRejectsInsecureTokenRealm(t *testing.T) {
	artifactRegistryBearerRealm(t, "direct", false)
}

func artifactRegistryBearerRealm(t *testing.T, mode string, realmTLS bool) {
	t.Helper()
	var tokenRequests, authenticatedWrites, registryChallenges atomic.Int64
	tokenHandler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		tokenRequests.Add(1)
		username, password, ok := request.BasicAuth()
		if !ok || username != "tester" || password != "secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="token service"`)
			http.Error(w, "token credentials required", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "test-token", "expires_in": 3600})
	})
	tokens := httptest.NewServer(tokenHandler)
	if realmTLS {
		tokens.Close()
		tokens = httptest.NewTLSServer(tokenHandler)
	}
	t.Cleanup(tokens.Close)
	backend := registryserver.New()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-token" {
			registryChallenges.Add(1)
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="test-registry",scope="repository:coopr/bearer:pull,push"`, tokens.URL+"/token"))
			http.Error(w, "bearer token required", http.StatusUnauthorized)
			return
		}
		if request.Method == http.MethodPut || request.Method == http.MethodPost {
			authenticatedWrites.Add(1)
		}
		backend.ServeHTTP(w, request)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "https://")
	certDir := t.TempDir()
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if realmTLS {
		certificate = append(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tokens.Certificate().Raw})...)
	}
	if err := os.WriteFile(filepath.Join(certDir, "ca.crt"), certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := testResolver(t, Options{CertDir: certDir, TLSVerify: boolOption(true), Credentials: "tester:secret", RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	metadata, paths := baleComponentMetadata(t, "amd64", "bearer", baleRegistryDependency)
	reference := host + "/coopr/bearer:stable"
	root, err := publishBaleRegistryComponent(t, mode, resolver, reference, metadata, paths)
	if !realmTLS {
		if err == nil {
			t.Fatal("publication accepted insecure token realm")
		}
		if registryChallenges.Load() == 0 {
			t.Fatal("insecure token realm challenge was not exercised")
		}
		if tokenRequests.Load() != 0 || authenticatedWrites.Load() != 0 {
			t.Fatalf("insecure token realm was contacted: token requests=%d authenticated writes=%d", tokenRequests.Load(), authenticatedWrites.Load())
		}
		return
	}
	if err != nil {
		t.Fatalf("publish with separate private-CA token realm: %v", err)
	}
	resolved, err := resolver.Resolve(t.Context(), reference, metadata.Platform, Component)
	if err != nil || !content.Equal(resolved.Root, root) {
		t.Fatalf("resolve with separate private-CA token realm: %+v, %v", resolved, err)
	}
	if tokenRequests.Load() == 0 || authenticatedWrites.Load() == 0 {
		t.Fatal("publication did not exercise bearer exchange and authenticated writes")
	}
}

type artifactRegistryProtocolListener struct {
	net.Listener
	plaintext *atomic.Int64
}

func (listener artifactRegistryProtocolListener) Accept() (net.Conn, error) {
	connection, err := listener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &artifactRegistryProtocolConn{Conn: connection, plaintext: listener.plaintext}, nil
}

type artifactRegistryProtocolConn struct {
	net.Conn
	plaintext *atomic.Int64
	read      bool
}

func (connection *artifactRegistryProtocolConn) Read(data []byte) (int, error) {
	n, err := connection.Conn.Read(data)
	if n > 0 && !connection.read {
		connection.read = true
		if data[0] >= 'A' && data[0] <= 'Z' {
			connection.plaintext.Add(1)
		}
	}
	return n, err
}

func TestArtifactRegistryHTTPSAuthenticationFailureDoesNotDowngrade(t *testing.T) {
	var plaintextConnections, httpsRequests atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		httpsRequests.Add(1)
		w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		http.Error(w, "credentials denied", http.StatusUnauthorized)
	}))
	server.Listener = artifactRegistryProtocolListener{Listener: server.Listener, plaintext: &plaintextConnections}
	server.StartTLS()
	t.Cleanup(server.Close)
	resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), Credentials: "wrong:credentials", RetrySet: true})
	if err != nil {
		t.Fatal(err)
	}
	metadata, paths := baleComponentMetadata(t, "amd64", "denied", baleRegistryDependency)
	reference := strings.TrimPrefix(server.URL, "https://") + "/coopr/denied:stable"
	if _, err := resolver.PublishComponent(t.Context(), reference, metadata, paths); err == nil {
		t.Fatal("publication succeeded after authentication failure")
	}
	if httpsRequests.Load() == 0 {
		t.Fatal("authentication failure was not exercised over HTTPS")
	}
	if plaintextConnections.Load() != 0 {
		t.Fatal("HTTPS authentication failure triggered a plaintext registry connection")
	}
}

func TestArtifactRegistryManifestReadRetryBudget(t *testing.T) {
	for _, retries := range []uint{0, 1} {
		t.Run(fmt.Sprintf("retries=%d", retries), func(t *testing.T) {
			backend := registryserver.New()
			var failNext atomic.Bool
			var failures atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/manifests/") && failNext.CompareAndSwap(true, false) {
					failures.Add(1)
					http.Error(w, "manifest temporarily unavailable", http.StatusServiceUnavailable)
					return
				}
				backend.ServeHTTP(w, request)
			}))
			t.Cleanup(server.Close)
			resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true, Retry: retries, RetryDelay: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			metadata, paths := baleComponentMetadata(t, "amd64", "retry")
			reference := strings.TrimPrefix(server.URL, "http://") + "/coopr/retry:stable"
			root, err := resolver.PublishComponent(t.Context(), reference, metadata, paths)
			if err != nil {
				t.Fatal(err)
			}
			failNext.Store(true)
			resolved, err := resolver.Resolve(t.Context(), reference, metadata.Platform, Component)
			if failures.Load() != 1 {
				t.Fatal("transient manifest failure was not exercised exactly once")
			}
			if retries == 0 {
				if err == nil {
					t.Fatal("explicit zero retry budget retried a failed manifest read")
				}
				return
			}
			if err != nil || !content.Equal(resolved.Root, root) {
				t.Fatalf("configured retry did not recover the manifest: %+v, %v", resolved, err)
			}
		})
	}
}

func TestArtifactRegistryMirrorAvailability(t *testing.T) {
	for _, missingBlob := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing-mirror-blob=%t", missingBlob), func(t *testing.T) {
			primaryBackend, mirrorBackend := registryserver.New(), registryserver.New()
			var primaryUnavailable, armed atomic.Bool
			var primaryRequests, primaryBlobReads, rejectedMirrorBlobReads atomic.Int64
			metadata, paths := baleComponentMetadata(t, "amd64", "mirror-availability")
			packageDigest := metadata.Packages[0].Descriptor.Digest.String()
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if armed.Load() {
					primaryRequests.Add(1)
					if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/blobs/"+packageDigest) {
						primaryBlobReads.Add(1)
					}
				}
				if primaryUnavailable.Load() {
					http.Error(w, "primary unavailable", http.StatusServiceUnavailable)
					return
				}
				primaryBackend.ServeHTTP(w, request)
			}))
			t.Cleanup(primary.Close)
			mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if armed.Load() && missingBlob && request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/blobs/"+packageDigest) {
					rejectedMirrorBlobReads.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"errors":[{"code":"BLOB_UNKNOWN","message":"mirror blob missing"}]}`))
					return
				}
				mirrorBackend.ServeHTTP(w, request)
			}))
			t.Cleanup(mirror.Close)
			primaryHost := strings.TrimPrefix(primary.URL, "http://")
			mirrorHost := strings.TrimPrefix(mirror.URL, "http://")
			seed, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, host := range []string{primaryHost, mirrorHost} {
				if _, err := seed.PublishComponent(t.Context(), host+"/coopr/mirror:stable", metadata, paths); err != nil {
					t.Fatal(err)
				}
			}
			configPath := filepath.Join(t.TempDir(), "registries.conf")
			config := fmt.Sprintf("[[registry]]\nlocation = %q\ninsecure = true\n[[registry.mirror]]\nlocation = %q\ninsecure = true\n", primaryHost, mirrorHost)
			if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			resolver, err := testResolver(t, Options{RetrySet: true})
			if err != nil {
				t.Fatal(err)
			}
			resolver.system.SystemRegistriesConfPath = configPath
			resolver.system.SystemRegistriesConfDirPath = t.TempDir()
			primaryUnavailable.Store(!missingBlob)
			armed.Store(true)
			resolved, err := resolver.Resolve(t.Context(), primaryHost+"/coopr/mirror:stable", metadata.Platform, Component)
			if err != nil {
				t.Fatalf("resolve with available mirror: %v", err)
			}
			output := filepath.Join(t.TempDir(), "package.tar")
			if err := resolver.Download(t.Context(), resolved, metadata.Packages[0].Descriptor, output); err != nil {
				t.Fatalf("download package through mirror policy: %v", err)
			}
			got, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(paths[metadata.Packages[0].Stage])
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatal("mirror selection changed package content")
			}
			if missingBlob {
				if rejectedMirrorBlobReads.Load() == 0 || primaryBlobReads.Load() == 0 {
					t.Fatal("missing mirror blob did not fall back to the primary")
				}
			} else if primaryRequests.Load() != 0 {
				t.Fatal("complete mirror read depended on the unavailable primary")
			}
		})
	}
}

func TestArtifactRegistryFileBackedUploadRetryBudget(t *testing.T) {
	for _, mode := range []string{"direct", "layout"} {
		for _, retries := range []uint{0, 1} {
			t.Run(fmt.Sprintf("%s/retries=%d", mode, retries), func(t *testing.T) {
				metadata, paths := baleComponentMetadata(t, "amd64", "upload-retry")
				packageDigest := metadata.Packages[0].Descriptor.Digest.String()
				backend := registryserver.New()
				var failNext atomic.Bool
				var failedUploads atomic.Int64
				failNext.Store(true)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					if request.Method == http.MethodPut && strings.Contains(request.URL.Path, "/blobs/uploads/") && request.URL.Query().Get("digest") == packageDigest && failNext.CompareAndSwap(true, false) {
						failedUploads.Add(1)
						http.Error(w, "upload temporarily unavailable", http.StatusServiceUnavailable)
						return
					}
					backend.ServeHTTP(w, request)
				}))
				t.Cleanup(server.Close)
				resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true, Retry: retries, RetryDelay: time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				reference := strings.TrimPrefix(server.URL, "http://") + "/coopr/upload-retry:stable"
				root, err := publishBaleRegistryComponent(t, mode, resolver, reference, metadata, paths)
				if failedUploads.Load() != 1 {
					t.Fatal("file-backed package upload failure was not exercised exactly once")
				}
				if retries == 0 {
					if err == nil {
						t.Fatal("explicit zero retry budget retried a failed package upload")
					}
					return
				}
				if err != nil {
					t.Fatalf("configured retry did not recover file-backed package upload: %v", err)
				}
				resolved, err := resolver.Resolve(t.Context(), reference, metadata.Platform, Component)
				if err != nil || !content.Equal(resolved.Root, root) {
					t.Fatalf("resolve retried package: %+v, %v", resolved, err)
				}
				output := filepath.Join(t.TempDir(), "package.tar")
				if err := resolver.Download(t.Context(), resolved, metadata.Packages[0].Descriptor, output); err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(output)
				if err != nil {
					t.Fatal(err)
				}
				want, err := os.ReadFile(paths[metadata.Packages[0].Stage])
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(want) {
					t.Fatal("retry changed package content")
				}
			})
		}
	}
}

func TestArtifactRegistryDownloadRejectsCorruptAndTrailingBlobs(t *testing.T) {
	for _, corruption := range []string{"same-size", "trailing"} {
		t.Run(corruption, func(t *testing.T) {
			metadata, paths := baleComponentMetadata(t, "amd64", "verified-download")
			packageData, err := os.ReadFile(paths[metadata.Packages[0].Stage])
			if err != nil {
				t.Fatal(err)
			}
			packageDigest := metadata.Packages[0].Descriptor.Digest.String()
			backend := registryserver.New()
			var armed atomic.Bool
			var corruptReads atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if armed.Load() && request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/blobs/"+packageDigest) {
					corruptReads.Add(1)
					data := append([]byte(nil), packageData...)
					if corruption == "same-size" {
						data[0] ^= 1
					} else {
						data = append(data, 'x')
					}
					w.Header().Set("Content-Type", "application/octet-stream")
					w.Header().Set("Docker-Content-Digest", packageDigest)
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					_, _ = w.Write(data)
					return
				}
				backend.ServeHTTP(w, request)
			}))
			t.Cleanup(server.Close)
			resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true})
			if err != nil {
				t.Fatal(err)
			}
			reference := strings.TrimPrefix(server.URL, "http://") + "/coopr/verified-download:stable"
			if _, err := resolver.PublishComponent(t.Context(), reference, metadata, paths); err != nil {
				t.Fatal(err)
			}
			resolved, err := resolver.Resolve(t.Context(), reference, metadata.Platform, Component)
			if err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(t.TempDir(), "package.tar")
			if err := os.WriteFile(output, []byte("keep original output"), 0o600); err != nil {
				t.Fatal(err)
			}
			armed.Store(true)
			if err := resolver.Download(t.Context(), resolved, metadata.Packages[0].Descriptor, output); err == nil {
				t.Fatal("corrupted or trailing bytes were accepted")
			}
			if corruptReads.Load() == 0 {
				t.Fatal("corrupt package response was not exercised")
			}
			got, err := os.ReadFile(output)
			if err != nil || string(got) != "keep original output" {
				t.Fatalf("failed verification replaced the output: %q, %v", got, err)
			}
		})
	}
}
