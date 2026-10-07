package oci

import (
	"bytes"
	"context"
	"encoding/json"
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

	"coopr/internal/definition"
	"coopr/internal/planner"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestComponentLayoutIdenticalPackagesAndRepeatRemainValidated(t *testing.T) {
	data := packageTar(t, "same")
	def, err := definition.Parse(strings.NewReader("package as=\"one\"\npackage as=\"two\"\nextend\ncopy \"/src\" \"/one\" from=\"one\"\ncopy \"/src\" \"/two\" from=\"two\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Create(def, planner.Options{Mode: planner.Publish})
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	meta := ComponentMetadata{Version: ComponentVersion, Platform: platform, Component: *plan.Component}
	paths := map[string]string{}
	for _, name := range []string{"one", "two"} {
		path := filepath.Join(t.TempDir(), "package.tar")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		paths[name] = path
		meta.Packages = append(meta.Packages, Package{Stage: name, Descriptor: Descriptor(ComponentPackageType, data), Config: packageImageConfig(t, platform, data)})
	}
	layout := t.TempDir()
	first, err := WriteComponentLayout(context.Background(), layout, meta, paths)
	if err != nil {
		t.Fatal(err)
	}
	second, err := WriteComponentLayout(context.Background(), layout, meta, paths)
	if err != nil || first.Digest != second.Digest {
		t.Fatalf("repeat: %+v, %v", second, err)
	}
	if err := os.WriteFile(paths["two"], []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteComponentLayout(context.Background(), layout, meta, paths); err == nil {
		t.Fatal("existing blob bypassed corrupt source validation")
	}
}

func TestComponentPublicationRetryCoversTransientUploadAndTLSOptions(t *testing.T) {
	for _, tlsServer := range []bool{false, true} {
		for _, maxRetry := range []uint{0, 1} {
			backend := registryserver.New()
			var uploads atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs/uploads/") && uploads.Add(1) == 1 {
					http.Error(w, "transient", http.StatusServiceUnavailable)
					return
				}
				backend.ServeHTTP(w, r)
			})
			server := httptest.NewServer(handler)
			if tlsServer {
				server.Close()
				server = httptest.NewTLSServer(handler)
			}
			t.Cleanup(server.Close)
			host := strings.TrimPrefix(strings.TrimPrefix(server.URL, "http://"), "https://")
			resolver, err := NewResolver(Options{TLSVerify: boolOption(false), RetrySet: true, Retry: maxRetry, RetryDelay: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			meta, paths := publicationMetadata(t, nil, false)
			_, err = resolver.PublishComponent(context.Background(), host+"/test/component:stable", meta, paths)
			if maxRetry == 0 {
				if err == nil || uploads.Load() != 1 {
					t.Fatalf("TLS=%v zero retry publication: %d %v", tlsServer, uploads.Load(), err)
				}
			} else if err != nil || uploads.Load() < 2 {
				t.Fatalf("TLS=%v publication retry missing: %d %v", tlsServer, uploads.Load(), err)
			}

		}
	}
}

func TestRepeatedComponentLayoutVerifiesPersistentContent(t *testing.T) {
	for _, kind := range []string{"package", "config", "manifest"} {
		for _, corruption := range []string{"same size", "truncated"} {
			t.Run(kind+"/"+corruption, func(t *testing.T) {
				ctx := context.Background()
				meta, paths := publicationMetadata(t, packageTar(t, "persistent"), true)
				layout := t.TempDir()
				root, err := WriteComponentLayout(ctx, layout, meta, paths)
				if err != nil {
					t.Fatal(err)
				}
				rootPath := filepath.Join(layout, "blobs", root.Digest.Algorithm().String(), root.Digest.Encoded())
				rootData, err := os.ReadFile(rootPath)
				if err != nil {
					t.Fatal(err)
				}
				var manifest v1.Manifest
				if err := json.Unmarshal(rootData, &manifest); err != nil {
					t.Fatal(err)
				}
				desc := root
				switch kind {
				case "package":
					desc = manifest.Layers[0]
				case "config":
					desc = manifest.Config
				}
				path := filepath.Join(layout, "blobs", desc.Digest.Algorithm().String(), desc.Digest.Encoded())
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				publishAgain := func() (v1.Descriptor, error) { return WriteComponentLayout(ctx, layout, meta, paths) }
				if kind == "manifest" {
					// Reuse an open target to isolate existing-manifest acceptance from
					// OCI layout reopening, which may separately reject malformed JSON.
					target, err := orasoci.NewWithContext(ctx, layout)
					if err != nil {
						t.Fatal(err)
					}
					publishAgain = func() (v1.Descriptor, error) { return writeComponent(ctx, target, meta, paths) }
				}
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
				damaged := bytes.Repeat([]byte("x"), len(original))
				if corruption == "truncated" {
					damaged = damaged[:len(damaged)-1]
				}
				if err := os.WriteFile(path, damaged, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := publishAgain(); err == nil {
					t.Fatal("repeated publication accepted corrupt existing " + kind)
				}
				// Fail closed without implicitly replacing persistent content.
				retained, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(retained, damaged) {
					t.Fatalf("persistent bytes changed: error=%v", err)
				}
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
				repeated, err := publishAgain()
				if err != nil || repeated.Digest != root.Digest {
					t.Fatalf("valid repeated publication=%+v error=%v", repeated, err)
				}
			})
		}
	}
}

type failingExistingComponentTarget struct {
	oras.Target
	descriptor digest.Digest
	failure    error
	cancel     context.CancelFunc
}

func (target failingExistingComponentTarget) Fetch(ctx context.Context, desc v1.Descriptor) (io.ReadCloser, error) {
	if desc.Digest == target.descriptor {
		if target.failure != nil {
			return nil, target.failure
		}
		target.cancel()
	}
	return target.Target.Fetch(ctx, desc)
}
func TestRepeatedComponentContentVerificationPreservesErrors(t *testing.T) {
	for _, kind := range []string{"package", "config", "manifest"} {
		for _, cancelFetch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cancel=%v", kind, cancelFetch), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				meta, paths := publicationMetadata(t, packageTar(t, "persistent"), true)
				layout := t.TempDir()
				root, err := WriteComponentLayout(ctx, layout, meta, paths)
				if err != nil {
					t.Fatal(err)
				}
				target, err := orasoci.NewWithContext(ctx, layout)
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(filepath.Join(layout, "blobs", root.Digest.Algorithm().String(), root.Digest.Encoded()))
				if err != nil {
					t.Fatal(err)
				}
				var manifest v1.Manifest
				if err := json.Unmarshal(data, &manifest); err != nil {
					t.Fatal(err)
				}
				desc := root
				switch kind {
				case "package":
					desc = manifest.Layers[0]
				case "config":
					desc = manifest.Config
				}
				failure := errors.New("injected existing component fetch failure")
				fault := failingExistingComponentTarget{Target: target, descriptor: desc.Digest, failure: failure}
				if cancelFetch {
					failure = context.Canceled
					fault.failure = nil
					fault.cancel = cancel
				}
				_, err = writeComponent(ctx, fault, meta, paths)
				if !errors.Is(err, failure) {
					t.Fatalf("existing %s verification lost error: %v want %v", kind, err, failure)
				}
			})
		}
	}
}
