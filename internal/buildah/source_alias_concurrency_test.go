package buildah

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
)

type pausedPullAliasStore struct {
	storage.Store
	alias                                        string
	firstAdded, secondAdded, secondLock, release chan struct{}
	aliases                                      atomic.Int32
	waitOnce                                     sync.Once
}

func (s *pausedPullAliasStore) AddNames(id string, names []string) error {
	if err := s.Store.AddNames(id, names); err != nil {
		return err
	}
	if slices.Contains(names, s.alias) {
		switch s.aliases.Add(1) {
		case 1:
			close(s.firstAdded)
			<-s.release
		case 2:
			close(s.secondAdded)
		}
	}
	return nil
}

type observedPullAliasLock struct {
	storage.Locker
	waiting chan struct{}
	once    *sync.Once
}

func (l observedPullAliasLock) TryLock() error {
	err := l.Locker.(interface{ TryLock() error }).TryLock()
	if err != nil {
		l.once.Do(func() { close(l.waiting) })
	}
	return err
}

//nolint:staticcheck // The native Store interface still returns the deprecated Locker type.
func (s *pausedPullAliasStore) GetDigestLock(key digest.Digest) (storage.Locker, error) {
	lock, err := s.Store.GetDigestLock(key)
	if err != nil {
		return nil, err
	}
	if key == digest.FromString(nativePullAliasLockPrefix+s.alias) {
		return observedPullAliasLock{Locker: lock, waiting: s.secondLock, once: &s.waitOnce}, nil
	}
	return lock, nil
}

func TestConcurrentCrossPlatformPullCapturesOriginBeforeAliasMoves(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	server := httptest.NewServer(registry.New())
	defer server.Close()
	address, _ := url.Parse(server.URL)
	reference := address.Host + "/multi:latest"
	source, err := orasoci.NewWithContext(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	platforms := []v1.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}}
	manifests := make([]v1.Descriptor, 2)
	configs := make([]v1.Descriptor, 2)
	children := make([]v1.Descriptor, 2)
	for i, platform := range platforms {
		manifests[i], configs[i] = sourceTestImage(t, ctx, source, platform, platform.Architecture)
		children[i] = manifests[i]
		children[i].Platform = &platform
	}
	rootIndex := sourceTestIndex(t, ctx, source, children...)
	if err := source.Tag(ctx, rootIndex, "latest"); err != nil {
		t.Fatal(err)
	}
	repo, err := remote.NewRepository(reference)
	if err != nil {
		t.Fatal(err)
	}
	repo.PlainHTTP = true
	if _, err := oras.Copy(ctx, source, "latest", repo, "latest", oras.CopyOptions{}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	backend, err := storage.GetStore(storage.StoreOptions{GraphDriverName: "vfs", GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = backend.Shutdown(true) }()
	store := &pausedPullAliasStore{Store: backend, alias: reference, firstAdded: make(chan struct{}), secondAdded: make(chan struct{}), secondLock: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(store.release) })
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root}
	type response struct {
		selection ResolvedImageSource
		err       error
	}
	results := []chan response{make(chan response, 1), make(chan response, 1)}
	var jobs sync.WaitGroup
	defer func() {
		cancel()
		release.Do(func() { close(store.release) })
		jobs.Wait()
	}()
	start := func(i int) {
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			resolver, err := oci.NewResolver(oci.Options{PullPolicy: string(oci.PullAlways), TLSVerify: new(false)})
			if err != nil {
				results[i] <- response{err: err}
				return
			}
			selected, err := ResolveImageSource(ctx, resolver, reference, platforms[i], store, system)
			results[i] <- response{selected, err}
		}()
	}
	start(0)
	select {
	case <-store.firstAdded:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	start(1)
	// The old implementation publishes the second alias here; the fixed one
	// actually contends on the same-alias lock and cannot publish until origin capture ends.
	select {
	case <-store.secondLock:
	case <-store.secondAdded:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	release.Do(func() { close(store.release) })
	for i, ch := range results {
		var got response
		select {
		case got = <-ch:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if got.err != nil {
			t.Fatalf("concurrent %s pull: %v", platforms[i].Architecture, got.err)
		}
		if got.selection.ImageID != configs[i].Digest.Encoded() || got.selection.Selected.Digest != manifests[i].Digest || got.selection.Root.Digest != rootIndex.Digest {
			t.Fatalf("wrong immutable selection: %+v", got.selection)
		}
		data, err := backend.ImageBigData(got.selection.ImageID, "coopr.image-origins.v1")
		if err != nil {
			t.Fatal(err)
		}
		var origins map[string]struct{ Root, Selected digest.Digest }
		if err := json.Unmarshal(data, &origins); err != nil {
			t.Fatal(err)
		}
		if origins[reference].Root != rootIndex.Digest || origins[reference].Selected != manifests[i].Digest {
			t.Fatalf("wrong per-ID origin association: %s", data)
		}
	}
	// A caller that does not participate in the alias transaction must still be
	// rejected if it tries to associate the current name with the old image.
	_, err = oci.ResolveStoredImage(ctx, backend, reference, platforms[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := oci.RecordStoredOrigin(ctx, backend, reference, oci.StoredSelection{ImageID: configs[0].Digest.Encoded(), Root: rootIndex, Manifest: manifests[0]}); err == nil {
		t.Fatal("name-shadow origin guard was weakened")
	}
	if err := ctx.Err(); err != nil {
		t.Fatal(err)
	}
}
