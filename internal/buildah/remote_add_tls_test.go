package buildah

import (
	"context"
	"coopr/internal/buildcontext"
	"coopr/internal/definition"
	"coopr/internal/planner"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/types"
)

func remoteAddTLSCertificate(t *testing.T, server *httptest.Server) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRemoteAddTLSPolicyMatchesNative(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("payload")) }))
	defer server.Close()
	certDir := remoteAddTLSCertificate(t, server)
	root := t.TempDir()
	lease, err := acquireStore(cacheTestStore(root))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}()
	network, err := newNetworkInterface(lease.store)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		options upstream.AddAndCopyOptions
		allowed bool
	}{
		{"default strict", upstream.AddAndCopyOptions{}, false},
		{"explicit strict", upstream.AddAndCopyOptions{InsecureSkipTLSVerify: types.OptionalBoolFalse}, false},
		{"insecure", upstream.AddAndCopyOptions{InsecureSkipTLSVerify: types.OptionalBoolTrue}, true},
		{"custom CA", upstream.AddAndCopyOptions{CertPath: certDir}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, native := range []bool{true, false} {
				b, err := upstream.NewBuilder(ctx, lease.store, upstream.BuilderOptions{FromImage: "scratch", PullPolicy: define.PullNever, Isolation: define.IsolationChroot, NetworkInterface: network, SystemContext: &types.SystemContext{BigFilesTemporaryDir: root}})
				if err != nil {
					t.Fatal(err)
				}
				if native {
					err = b.Add("/payload", false, test.options, server.URL+"/source")
				} else {
					err = applyRemoteAddSource(ctx, nativeBuilder{Builder: b}, Add{Destination: "/payload"}, test.options, server.URL+"/source")
				}
				if (err == nil) != test.allowed {
					t.Errorf("native=%t allowed=%t: %v", native, test.allowed, err)
				}
				if err := b.Delete(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	t.Run("HTTP contexts remain strict like native", func(t *testing.T) {
		nativeDirectory, _, nativeErr := define.TempDirForURL(t.TempDir(), "native", server.URL+"/context.tar")
		if nativeDirectory != "" {
			defer func() {
				if err := os.RemoveAll(nativeDirectory); err != nil {
					t.Error(err)
				}
			}()
		}
		_, _, cleanup, cooprErr := downloadHTTPContext(ctx, buildcontext.Spec{Name: "context", Kind: buildcontext.HTTPArchive, Reference: server.URL + "/context.tar"})
		if cleanup != nil {
			defer func() {
				if err := cleanup(); err != nil {
					t.Error(err)
				}
			}()
		}
		if nativeErr == nil || cooprErr == nil || !strings.Contains(nativeErr.Error(), "x509") || !strings.Contains(cooprErr.Error(), "x509") {
			t.Fatalf("context TLS mismatch: native=%v Coopr=%v", nativeErr, cooprErr)
		}
	})

}

func TestRemoteAddTLSBuildCacheAndEpoch(t *testing.T) {
	requireLiveInstructionCache(t)
	for _, mode := range []string{"insecure", "custom CA"} {
		t.Run(mode, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			var body atomic.Value
			body.Store("first")
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Last-Modified", time.Unix(123, 0).UTC().Format(http.TimeFormat))
				_, _ = io.WriteString(w, body.Load().(string))
			}))
			defer server.Close()
			options := SupervisedPlanOptions{ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun", CacheLocalDir: f.options.CacheLocalDir, Stdout: io.Discard, Stderr: io.Discard, SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath}
			if mode == "insecure" {
				options.TLSVerify = new(false)
			} else {
				options.CertDir = remoteAddTLSCertificate(t, server)
			}
			def, err := definition.Parse(strings.NewReader(fmt.Sprintf("arg \"SOURCE_DATE_EPOCH\" \"metadata\"\nfrom \"scratch\" as=\"metadata\" {add %q \"/source\";}\n", server.URL+"/source")))
			if err != nil {
				t.Fatal(err)
			}
			for _, attempt := range []string{"cold", "warm", "changed", "warm changed", "fresh store"} {
				if attempt == "changed" {
					body.Store("second")
				}
				root := filepath.Join(f.root, attempt)
				options.Store = cacheTestStore(filepath.Join(root, "store"))
				options.Output = Output{Path: filepath.Join(root, "layout")}
				result, err := BuildDefinitionSupervised(f.ctx, def, planner.Options{Mode: planner.Build}, options)
				if err != nil {
					t.Fatalf("%s: %v", attempt, err)
				}
				if got := localComponentLastFile(t, result.Layout, "source"); got != body.Load().(string) {
					t.Fatalf("%s consumed %q, want current body", attempt, got)
				}
				_, config := readPlanImage(t, result.Layout)
				if config.Created == nil || config.Created.Unix() != 123 {
					t.Fatalf("%s source epoch=%v", attempt, config.Created)
				}
				if attempt != "cold" && attempt != "changed" && result.CacheStats.Hits == 0 {
					t.Fatalf("%s did not restore portable input cache: %+v", attempt, result.CacheStats)
				}
			}
		})
	}
}

func TestRemoteAddTLSCredentialRedirect(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Referer") != "" {
			leaked.Store(true)
		}
		_, _ = io.WriteString(w, "redirected payload")
	}))
	defer target.Close()
	targetURL := strings.Replace(target.URL, "127.0.0.1", "example.com", 1)
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Basic source" {
			t.Error("source host secret missing")
		}
		http.Redirect(w, r, targetURL+"/target", http.StatusFound)
	}))
	defer source.Close()
	builder := &credentialAddBuilder{secrets: map[string][]byte{"HTTP_AUTH_HEADER_127.0.0.1": []byte("Basic source")}}
	client, err := configuredRemoteAddHTTPClient(builder, upstream.AddAndCopyOptions{CertPath: remoteAddTLSCertificate(t, source)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	transport := client.Transport.(*http.Transport)
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if strings.HasPrefix(address, "example.com:") {
			address = strings.TrimPrefix(target.URL, "https://")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, source.URL+"/source?secret=query", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := setRemoteAddAuthorization(request, builder); err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || string(data) != "redirected payload" || leaked.Load() {
		t.Fatalf("HTTPS redirect lost body or leaked source credentials: read=%v close=%v leaked=%t", readErr, closeErr, leaked.Load())
	}
}
