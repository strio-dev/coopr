package buildah

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"coopr/internal/oci"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestArtifactInputLockCancellationDoesNotEnterTransaction(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	root := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	holder := artifactLockTestStart(t, func() error {
		return withArtifactInputLock(ctx, root, func() error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	})
	artifactLockTestWait(t, ctx, entered)
	waitCtx, cancelWait := context.WithCancel(ctx)
	defer cancelWait()
	observed := &artifactLockObservedContext{Context: waitCtx, waiting: make(chan struct{})}
	called := make(chan struct{})
	waiter := artifactLockTestStart(t, func() error {
		return withArtifactInputLock(observed, root, func() error {
			close(called)
			return nil
		})
	})
	select {
	case <-observed.waiting:
	case err := <-waiter:
		t.Fatalf("contending transaction did not wait: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancelWait()
	if err := <-waiter; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lock waiter returned %v", err)
	}
	select {
	case <-called:
		t.Fatal("canceled waiter entered the transaction")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-holder; err != nil {
		t.Fatal(err)
	}
	if err := withArtifactInputLock(ctx, root, func() error { return nil }); err != nil {
		t.Fatalf("released lock was not reusable: %v", err)
	}
}

func TestArtifactIndexRefreshCoordinatesCachedAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	paused := make(chan struct{})
	release := make(chan struct{})
	var pauseOnce, releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	backend := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/blobs/"+digest.FromString("refresh-amd64").String()) {
			pauseOnce.Do(func() { close(paused) })
			select {
			case <-release:
			case <-request.Context().Done():
				return
			case <-ctx.Done():
				return
			}
		}
		backend.ServeHTTP(w, request)
	}))
	t.Cleanup(server.Close)
	repository := artifactIndexTestRepository(t, server.URL)
	reference := repository.Reference.String() + ":latest"
	store, system := artifactSourceTestStore(t)
	artifactSourceTestAdd(t, ctx, store, reference, map[string]string{"payload": "old flat"})
	children := artifactIndexTestPublish(t, repository, "refresh")
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false), PullPolicy: "always", SignaturePolicyPath: system.SignaturePolicyPath, RetrySet: true, Retry: 0})
	if err != nil {
		t.Fatal(err)
	}
	var refreshed, admitted ResolvedImageSource
	refresh := artifactLockTestStart(t, func() error {
		var err error
		refreshed, err = resolveFileSource(ctx, resolver, reference, platform, store, system)
		return err
	})
	artifactLockTestWait(t, ctx, paused)
	observed := &artifactLockObservedContext{Context: ctx, waiting: make(chan struct{})}
	admission := artifactLockTestStart(t, func() error {
		var err error
		admitted, err = resolveStoredArtifactFileSource(observed, reference, platform, store, system)
		return err
	})
	select {
	case <-observed.waiting:
	case err := <-admission:
		t.Fatalf("cached admission completed during the paused refresh: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-refresh; err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if err := <-admission; err != nil {
		t.Fatalf("cached admission after refresh: %v", err)
	}
	for _, selected := range []ResolvedImageSource{refreshed, admitted} {
		if selected.Selected.Digest != children["amd64"].Digest {
			t.Fatalf("transaction selected stale content: %+v", selected)
		}
		artifactIndexTestPayload(t, store, selected, "refresh-amd64")
	}
	server.Close()
	offline, err := artifactIndexTestResolve(t, reference, "amd64", "never", store, system)
	if err != nil || offline.Selected.Digest != children["amd64"].Digest {
		t.Fatalf("final offline selection = %+v, %v", offline, err)
	}
}

func TestNativeImageShadowsCachedArtifactIndexWithoutEvictingIt(t *testing.T) {
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	repository := artifactIndexTestRepository(t, server.URL)
	reference := repository.Reference.String() + ":latest"
	store, system := artifactSourceTestStore(t)
	children := artifactIndexTestPublish(t, repository, "indexed")
	if _, err := artifactIndexTestResolve(t, reference, "amd64", "missing", store, system); err != nil {
		t.Fatal(err)
	}
	sourcePath := t.TempDir()
	source, err := orasoci.NewWithContext(t.Context(), sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	image, _ := sourceTestImage(t, t.Context(), source, v1.Platform{OS: "linux", Architecture: "amd64"}, "native image")
	imageID, err := ImportSelectedImage(t.Context(), store, system, sourcePath, image)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddNames(imageID, []string{reference}); err != nil {
		t.Fatal(err)
	}
	server.Close()
	selected, err := artifactIndexTestResolve(t, reference, "amd64", "never", store, system)
	if err != nil || selected.Artifact || selected.ImageID != imageID {
		t.Fatalf("native image did not shadow the artifact: %+v, %v", selected, err)
	}
	if err := store.RemoveNames(imageID, []string{reference}); err != nil {
		t.Fatal(err)
	}
	selected, err = artifactIndexTestResolve(t, reference, "amd64", "never", store, system)
	if err != nil || !selected.Artifact || selected.Selected.Digest != children["amd64"].Digest {
		t.Fatalf("removing image name did not reveal retained artifact: %+v, %v", selected, err)
	}
	artifactIndexTestPayload(t, store, selected, "indexed-amd64")
}

// flock checks Done only after an unsuccessful TryLock. Observing this call
// establishes contention without a sleep or an elapsed-time assertion.
type artifactLockObservedContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *artifactLockObservedContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

func artifactLockTestStart(t *testing.T, operation func() error) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- operation()
		close(done)
	}()
	t.Cleanup(func() { <-done })
	return done
}

func artifactLockTestWait(t *testing.T, ctx context.Context, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
