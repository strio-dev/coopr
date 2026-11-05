package buildah

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	"go.podman.io/storage"
	"golang.org/x/sys/unix"
)

type nativeAliasTestLock struct {
	storage.Locker
	held    atomic.Bool
	failure error
}

func (l *nativeAliasTestLock) TryLock() error {
	if l.failure != nil {
		return l.failure
	}
	if !l.held.CompareAndSwap(false, true) {
		return unix.EAGAIN
	}
	return nil
}
func (l *nativeAliasTestLock) Unlock() { l.held.Store(false) }

type nativeAliasTestStore struct {
	storage.Store
	mu      sync.Mutex
	locks   map[digest.Digest]*nativeAliasTestLock
	reads   atomic.Int32
	failure error
}

//nolint:staticcheck // The native Store interface still returns the deprecated Locker type.
func (s *nativeAliasTestStore) GetDigestLock(key digest.Digest) (storage.Locker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks == nil {
		s.locks = make(map[digest.Digest]*nativeAliasTestLock)
	}
	if s.locks[key] == nil {
		s.locks[key] = &nativeAliasTestLock{failure: s.failure}
	}
	return s.locks[key], nil
}
func (s *nativeAliasTestStore) Image(string) (*storage.Image, error) {
	s.reads.Add(1)
	return nil, storage.ErrImageUnknown
}

func TestNativePullAliasLockCancellationAndIndependentNames(t *testing.T) {
	store := &nativeAliasTestStore{}
	holder, err := lockNativePullAlias(t.Context(), store, "alpine:3.23")
	if err != nil {
		t.Fatal(err)
	}
	defer holder()
	ctx, cancel := context.WithCancel(t.Context())
	observed := &artifactLockObservedContext{Context: ctx, waiting: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		unlock, err := lockNativePullAlias(observed, store, "docker.io/library/alpine:3.23")
		if unlock != nil {
			unlock()
		}
		done <- err
	}()
	select {
	case <-observed.waiting:
	case <-time.After(time.Second):
		t.Fatal("same normalized alias did not wait")
	}
	other, err := lockNativePullAlias(t.Context(), store, "docker.io/library/other:3.23")
	if err != nil {
		t.Fatal(err)
	}
	other()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter leaked")
	}
	key := digest.FromString(nativePullAliasLockPrefix + "docker.io/library/alpine:3.23")
	if !store.locks[key].held.Load() {
		t.Fatal("canceled waiter unlocked another owner")
	}
}

func TestCachedNativeAliasReadWaitsForOriginTransaction(t *testing.T) {
	store := &nativeAliasTestStore{}
	holder, err := lockNativePullAlias(t.Context(), store, "registry.example/base:latest")
	if err != nil {
		t.Fatal(err)
	}
	defer holder()
	resolver, err := oci.NewResolver(oci.Options{PullPolicy: string(oci.PullMissing)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := &artifactLockObservedContext{Context: ctx, waiting: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := selectImageSource(observed, resolver, "registry.example/base:latest", v1.Platform{OS: "linux", Architecture: "amd64"}, store)
		done <- err
	}()
	select {
	case <-observed.waiting:
	case <-time.After(time.Second):
		t.Fatal("cached reader entered partial origin transaction")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cached reader cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cached reader failed to cancel")
	}
	if store.reads.Load() != 0 {
		t.Fatal("cached reader observed alias before origin transaction completed")
	}
}

func TestNativePullAliasLockPropagatesNonContentionFailure(t *testing.T) {
	failure := errors.New("lock permission denied")
	_, err := lockNativePullAlias(t.Context(), &nativeAliasTestStore{failure: failure}, "alpine:3.23")
	if !errors.Is(err, failure) {
		t.Fatalf("lock error hidden: %v", err)
	}
}
