package oci

import (
	"context"
	"errors"
	"github.com/gofrs/flock"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"coopr/internal/componentstore"
	"github.com/google/go-containerregistry/pkg/registry"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestComponentNamesResolveLocallyBeforeRegistry(t *testing.T) {
	ctx := context.Background()
	var requests atomic.Int64
	registryHandler := registry.New()
	_, _, resolver, name := testRepository(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		registryHandler.ServeHTTP(w, r)
	}))
	resolver.componentStoreDir = filepath.Join(t.TempDir(), "components")
	meta, paths := publicationMetadata(t, packageTar(t, "local"), true)
	layout := t.TempDir()
	root, err := WriteComponentLayout(ctx, layout, meta, paths)
	if err != nil {
		t.Fatal(err)
	}
	source, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"settings", "settings:v1", "local:foo", name + ":latest"} {
		if err := componentstore.Put(ctx, resolver.componentStoreDir, source, root, tag); err != nil {
			t.Fatal(err)
		}
	}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	for _, ref := range []string{"settings", "settings:latest", "docker.io/library/settings", "settings:v1", "local:foo", name, "settings@" + root.Digest.String(), root.Digest.String()} {
		got, err := resolver.Resolve(ctx, ref, platform, Component)
		if err != nil || got.Root.Digest != root.Digest {
			t.Fatalf("resolve %q: %+v, %v", ref, got, err)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("local names contacted registry %d times", requests.Load())
	}
	// A present local name remains authoritative when its platform is incompatible.
	if _, err := resolver.Resolve(ctx, name, v1.Platform{OS: "linux", Architecture: "arm64"}, Component); err == nil {
		t.Fatal("incompatible local component platform accepted")
	}
	if requests.Load() != 0 {
		t.Fatal("local platform mismatch fell back to registry")
	}
	// local:foo is the literal image name local with tag foo, never an alias for foo.
	if _, _, err := resolver.lookupLocalComponent(ctx, "foo:latest"); err == nil {
		t.Fatal("local:foo was stored as foo")
	}
	// Missing blobs in a selected local graph must fail, not fetch registry content.
	if err := os.Remove(filepath.Join(resolver.componentStoreDir, "blobs", "sha256", root.Digest.Encoded())); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(ctx, name, platform, Component); err == nil {
		t.Fatal("corrupt local graph resolved")
	}
	if requests.Load() != 0 {
		t.Fatal("corrupt graph fell back to registry")
	}
}

func TestComponentMissingNameFallsBackWithoutCreatingStore(t *testing.T) {
	ctx := context.Background()
	_, _, resolver, name := testRepository(t, registry.New())
	resolver.componentStoreDir = filepath.Join(t.TempDir(), "absent")
	meta, paths := publicationMetadata(t, packageTar(t, "remote"), true)
	root, err := resolver.PublishComponent(ctx, name+":v1", meta, paths)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolver.Resolve(ctx, name+":v1", meta.Platform, Component)
	if err != nil || got.Root.Digest != root.Digest {
		t.Fatalf("remote fallback: %+v, %v", got, err)
	}
	if _, err := os.Stat(resolver.componentStoreDir); !os.IsNotExist(err) {
		t.Fatalf("lookup initialized absent store: %v", err)
	}
	// A first-use directory may exist with only coordination locks. Lookup must
	// still fall back without inventing OCI metadata there.
	if err := os.MkdirAll(resolver.componentStoreDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"empty", "locks-only"} {
		if state == "locks-only" {
			for _, lock := range []string{".coopr-store.lock", ".coopr-activity.lock"} {
				if err := os.WriteFile(filepath.Join(resolver.componentStoreDir, lock), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		got, err := resolver.Resolve(ctx, name+":v1", meta.Platform, Component)
		if err != nil || got.Root.Digest != root.Digest {
			t.Fatalf("%s first-use fallback: %+v, %v", state, got, err)
		}
		for _, metadata := range []string{v1.ImageIndexFile, v1.ImageLayoutFile} {
			if _, err := os.Stat(filepath.Join(resolver.componentStoreDir, metadata)); !os.IsNotExist(err) {
				t.Fatalf("%s lookup initialized %s: %v", state, metadata, err)
			}
		}
	}
	// A named immutable selection must not be replaced by a mutable local tag.
	layout := t.TempDir()
	localMeta, localPaths := publicationMetadata(t, packageTar(t, "local"), true)
	localRoot, err := WriteComponentLayout(ctx, layout, localMeta, localPaths)
	if err != nil {
		t.Fatal(err)
	}
	source, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := componentstore.Put(ctx, resolver.componentStoreDir, source, localRoot, name+":v1"); err != nil {
		t.Fatal(err)
	}
	got, err = resolver.Resolve(ctx, name+"@"+root.Digest.String(), meta.Platform, Component)
	if err != nil || got.Root.Digest != root.Digest {
		t.Fatalf("immutable registry selection: %+v, %v", got, err)
	}
}

func TestComponentCorruptStoreAndBareDigestsDoNotReachRegistry(t *testing.T) {
	ctx := context.Background()
	var requests atomic.Int64
	_, _, resolver, name := testRepository(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected registry request", http.StatusInternalServerError)
	}))
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	for _, state := range []string{"malformed-index", "invalid-index", "missing-index", "missing-all-metadata", "unrelated-content", "non-directory"} {
		t.Run(state, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "components")
			if state == "non-directory" {
				if err := os.WriteFile(dir, []byte("bad"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				switch state {
				case "missing-all-metadata":
					if err := os.Mkdir(filepath.Join(dir, "blobs"), 0700); err != nil {
						t.Fatal(err)
					}
				case "unrelated-content":
					if err := os.WriteFile(filepath.Join(dir, "payload"), []byte("resident"), 0600); err != nil {
						t.Fatal(err)
					}
				case "malformed-index", "invalid-index":
					index := []byte("{")
					if state == "invalid-index" {
						index = []byte(`{}`)
					}
					if err := os.WriteFile(filepath.Join(dir, v1.ImageIndexFile), index, 0600); err != nil {
						t.Fatal(err)
					}
				default:
					if err := os.WriteFile(filepath.Join(dir, v1.ImageLayoutFile), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			resolver.componentStoreDir = dir
			if _, err := resolver.Resolve(ctx, name+":latest", platform, Component); err == nil {
				t.Fatal("invalid store accepted")
			}
		})
	}
	resolver.componentStoreDir = filepath.Join(t.TempDir(), "absent")
	for _, ref := range []string{digest.FromString("absent").String(), "sha256:not-a-digest", "settings@sha256:bad"} {
		if _, err := resolver.Resolve(ctx, ref, platform, Component); err == nil {
			t.Fatalf("invalid/missing digest %q accepted", ref)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("store/digest failures contacted registry %d times", requests.Load())
	}
}

func TestValidateComponentReferenceConventionalNames(t *testing.T) {
	for _, ref := range []string{"settings", "team/settings:v1", "local:foo", "registry.example/team/settings@" + digest.FromString("component").String()} {
		if err := ValidateComponentReference(ref); err != nil {
			t.Fatalf("%q: %v", ref, err)
		}
	}
	for _, ref := range []string{"./component.coopr", "../component.coopr", "/tmp/component.coopr", "", strings.Repeat("a", 256)} {
		if err := ValidateComponentReference(ref); err == nil {
			t.Fatalf("invalid OCI name %q accepted", ref)
		}
	}
}

func TestComponentLookupWaitsForLocalStoreWriter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	dir := t.TempDir()
	lock := flock.New(filepath.Join(dir, ".coopr-store.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Unlock() }()
	resolver, err := NewResolver(Options{ComponentStoreDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolver.Resolve(ctx, "settings", v1.Platform{OS: "linux", Architecture: "amd64"}, Component)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("did not wait for local writer: %v", err)
	}
}
