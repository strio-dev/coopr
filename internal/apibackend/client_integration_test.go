package apibackend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/coopr/api"
	"github.com/strio-dev/coopr/client"
	"github.com/strio-dev/coopr/internal/apiservice"
	"github.com/strio-dev/coopr/internal/buildah"
)

// This fixture exercises the public transport against the real backend. Service
// lifetime belongs to the test owner; the client neither starts nor stops it.
func publicNativeClient(t *testing.T) (*client.Client, func()) {
	t.Helper()
	directory, err := os.MkdirTemp("", "coopr-sdk-")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	socket := filepath.Join(directory, "api.sock")
	done := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = apiservice.Serve(ctx, socket, "native-client-qualification", Execute)
		close(done)
	}()
	c, err := client.New(socket, client.Options{})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		t.Helper()
		once.Do(func() {
			cancel()
			select {
			case <-done:
				if serveErr != nil {
					t.Errorf("stop public API: %v", serveErr)
				}
			case <-time.After(45 * time.Second):
				t.Error("public API did not drain")
			}
			c.CloseIdleConnections()
			if err := os.RemoveAll(directory); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(stop)
	ready, readyCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer readyCancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := c.Info(ready); err == nil {
			return c, stop
		}
		select {
		case <-done:
			t.Fatalf("API exited before readiness: %v", serveErr)
		case <-ready.Done():
			t.Fatalf("API readiness: %v", ready.Err())
		case <-ticker.C:
		}
	}
}

type publicNativeResponse struct {
	result api.Result
	err    error
	logs   string
}

func startPublicNative(ctx context.Context, c *client.Client, request api.Request) <-chan publicNativeResponse {
	done := make(chan publicNativeResponse, 1)
	go func() {
		var logs bytes.Buffer
		result, err := c.Execute(ctx, request, client.StreamOptions{Stdout: &logs, Stderr: &logs})
		done <- publicNativeResponse{result: result, err: err, logs: logs.String()}
	}()
	return done
}

func awaitPublicNative(t *testing.T, ctx context.Context, done <-chan publicNativeResponse) api.Result {
	t.Helper()
	select {
	case response := <-done:
		if response.err != nil {
			t.Fatalf("public client: %v\n%s", response.err, response.logs)
		}
		return response.result
	case <-ctx.Done():
		t.Fatalf("public client did not return: %v", ctx.Err())
		return api.Result{}
	}
}

func publicNativeWrite(t *testing.T, path, value string, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), mode); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1600000000, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

func publicNativeRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func clearPublicNativeFreshStore(t *testing.T, f *nativeAPIFixture) {
	t.Helper()
	store := buildah.StoreOptions{RunRoot: filepath.Join(f.root, "fresh", "run"), GraphRoot: filepath.Join(f.root, "fresh", "graph"), GraphDriverName: "vfs"}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := buildah.MaintainStoreSupervised(ctx, buildah.StoreMaintenanceRequest{Store: store, Mode: buildah.StoreMaintenancePrune, All: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.root, "fresh")); err != nil {
		t.Fatal(err)
	}
}

func TestPublicClientNativeConcurrentCacheInvalidationAndIntegrity(t *testing.T) {
	f := newNativeAPIFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	publicNativeWrite(t, filepath.Join(f.context, "input"), "first\n", 0644)
	request := func(label, lane, store string) api.Request {
		r := f.request(label, lane, store)
		operations := r.Program.States[0].Operations
		run := operations[len(operations)-1]
		r.Program.States[0].Operations = append(operations[:len(operations)-1], api.Operation{Kind: "copy", Sources: []string{"input"}, Destination: "/input"}, run)
		return r
	}
	c, stop := publicNativeClient(t)
	first := startPublicNative(ctx, c, request("client-cold-a", "a", "shared"))
	second := startPublicNative(ctx, c, request("client-cold-b", "b", "shared"))
	awaitNativeAPIArrivals(t, ctx, f, "a", "b")
	if workers := directNativeAPIWorkers(t); len(workers) != 2 {
		t.Fatalf("public requests did not overlap in distinct workers: %v", workers)
	}
	f.once.Do(func() { close(f.release) })
	proofs := map[string]string{}
	for i, done := range []<-chan publicNativeResponse{first, second} {
		result := awaitPublicNative(t, ctx, done)
		if result.Cache == nil || result.Cache.Errors != 0 {
			t.Fatalf("cold cache: %+v", result.Cache)
		}
		lane := []string{"a", "b"}[i]
		proofs[lane] = publicNativeRead(t, filepath.Join(result.Filesystem, "proof"))
	}
	stop()
	c, _ = publicNativeClient(t)
	for _, lane := range []string{"a", "b"} {
		result := awaitPublicNative(t, ctx, startPublicNative(ctx, c, request("client-warm-"+lane, lane, "fresh")))
		if result.Cache == nil || result.Cache.Hits != 3 || result.Cache.Misses != 0 || result.Cache.Errors != 0 {
			t.Fatalf("fresh-store warm cache: %+v", result.Cache)
		}
		if publicNativeRead(t, filepath.Join(result.Filesystem, "proof")) != proofs[lane] {
			t.Fatal("warm public invocation reran nondeterministic RUN")
		}
		clearPublicNativeFreshStore(t, f)
	}
	select {
	case lane := <-f.arrivals:
		t.Fatalf("warm invocation executed lane %s", lane)
	default:
	}
	// Equal-size edits retain mtime, so correctness depends on content identity.
	publicNativeWrite(t, filepath.Join(f.context, "input"), "other\n", 0644)
	changed := awaitPublicNative(t, ctx, startPublicNative(ctx, c, request("client-changed", "a", "fresh")))
	awaitNativeAPIArrivals(t, ctx, f, "a")
	if changed.Cache == nil || changed.Cache.Misses < 2 || publicNativeRead(t, filepath.Join(changed.Filesystem, "input")) != "other\n" {
		t.Fatalf("changed content did not invalidate COPY/RUN: %+v", changed.Cache)
	}
	changedProof := publicNativeRead(t, filepath.Join(changed.Filesystem, "proof"))
	if changedProof == proofs["a"] {
		t.Fatal("changed content reused the earlier RUN")
	}
	clearPublicNativeFreshStore(t, f)
	// Corrupt a shared filesystem layer while preserving its declared size.
	var largest string
	var size int64
	if err := filepath.WalkDir(f.cache, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() && strings.Contains(filepath.ToSlash(path), "/blobs/sha256/") {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Size() > size {
				largest, size = path, info.Size()
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if largest == "" || size == 0 {
		t.Fatal("native cache did not retain a payload blob")
	}
	info, err := os.Stat(largest)
	if err != nil {
		t.Fatal(err)
	}
	// CAS payloads are intentionally read-only. This test owns the private
	// cache and restores its original mode after deliberate corruption.
	if err := os.Chmod(largest, 0600); err != nil {
		t.Fatal(err)
	}
	blob, err := os.OpenFile(largest, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	one := []byte{0}
	_, readErr := blob.ReadAt(one, size-1)
	one[0] ^= 0xff
	_, writeErr := blob.WriteAt(one, size-1)
	if err := errors.Join(readErr, writeErr, blob.Close(), os.Chmod(largest, info.Mode().Perm())); err != nil {
		t.Fatal(err)
	}
	rebuilt := awaitPublicNative(t, ctx, startPublicNative(ctx, c, request("client-corrupt", "a", "fresh")))
	awaitNativeAPIArrivals(t, ctx, f, "a")
	if rebuilt.Cache == nil || rebuilt.Cache.Errors == 0 || rebuilt.Cache.Misses == 0 {
		t.Fatalf("corrupt payload did not cause a reported rebuild: %+v", rebuilt.Cache)
	}
	if publicNativeRead(t, filepath.Join(rebuilt.Filesystem, "input")) != "other\n" || publicNativeRead(t, filepath.Join(rebuilt.Filesystem, "proof")) == changedProof {
		t.Fatal("corrupt cache content escaped verified restoration")
	}
	if workers := directNativeAPIWorkers(t); len(workers) != 0 {
		t.Fatalf("completed public requests retained workers: %v", workers)
	}
}

func TestPublicClientNativeCancellationWaitsForWorkerDrain(t *testing.T) {
	f := newNativeAPIFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	c, _ := publicNativeClient(t)
	run, cancelRun := context.WithCancel(ctx)
	done := startPublicNative(run, c, f.request("client-cancel", "canceled", "shared"))
	awaitNativeAPIArrivals(t, ctx, f, "canceled")
	if workers := directNativeAPIWorkers(t); len(workers) != 1 {
		t.Fatalf("missing worker before cancellation: %v", workers)
	}
	cancelRun()
	select {
	case response := <-done:
		if !errors.Is(response.err, context.Canceled) || errors.Is(response.err, client.ErrCleanupFailed) {
			t.Fatalf("cancellation lost acknowledgement: %v", response.err)
		}
		var execution *client.ExecutionError
		if !errors.As(response.err, &execution) || execution.ID == "" {
			t.Fatalf("cancellation lost execution identity: %v", response.err)
		}
		status, err := c.Get(ctx, execution.ID)
		if err != nil || status.State != "canceled" {
			t.Fatalf("cancellation status: %+v, %v", status, err)
		}
	case <-ctx.Done():
		t.Fatal("canceled public invocation did not return")
	}
	if workers := directNativeAPIWorkers(t); len(workers) != 0 {
		t.Fatalf("client returned before worker drain: %v", workers)
	}
	if _, err := os.Lstat(filepath.Join(f.root, "client-cancel.oci")); !os.IsNotExist(err) {
		t.Fatalf("canceled client invocation published output: %v", err)
	}
	f.once.Do(func() { close(f.release) })
	retry := awaitPublicNative(t, ctx, startPublicNative(ctx, c, f.request("client-cancel-retry", "canceled", "shared")))
	awaitNativeAPIArrivals(t, ctx, f, "canceled")
	if retry.Cache == nil || retry.Cache.Misses == 0 {
		t.Fatalf("canceled RUN populated reusable cache: %+v", retry.Cache)
	}
}

func publicNativePolicy(t *testing.T, f *nativeAPIFixture) {
	t.Helper()
	publicNativeWrite(t, f.policy, `{"default":[{"type":"reject"}],"transports":{"containers-storage":{"":[{"type":"insecureAcceptAnything"}]},"oci":{"":[{"type":"insecureAcceptAnything"}]},"oci-archive":{"":[{"type":"insecureAcceptAnything"}]},"docker":{"":[{"type":"insecureAcceptAnything"}]}}}`, 0600)
}

func publicNativeDefinition(f *nativeAPIFixture, file, kind, label, componentStore string) api.Request {
	r := f.request(label, "unused", "shared")
	r.Kind, r.Program = kind, nil
	r.Build = &api.Build{Files: []string{file}, ComponentStoreDir: componentStore}
	if kind == "component" {
		r.Options.Output = api.Output{OCI: filepath.Join(f.root, label+".oci.tar")}
	}
	r.Options.Registry.TLSVerify = new(false)
	return r
}

func publicNativePublication(r api.Request, artifact, source, componentStore string, destinations ...string) api.Request {
	return api.Request{Kind: "publish", Publish: &api.Publish{Artifact: artifact, Source: source, ComponentStoreDir: componentStore, Destinations: destinations}, Options: api.Options{Store: r.Options.Store, Registry: r.Options.Registry}}
}

func TestPublicClientNativeBuildAndConcurrentPublication(t *testing.T) {
	f := newNativeAPIFixture(t)
	publicNativePolicy(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	c, _ := publicNativeClient(t)
	assets := filepath.Join(f.root, "assets")
	if err := os.Mkdir(assets, 0700); err != nil {
		t.Fatal(err)
	}
	publicNativeWrite(t, filepath.Join(assets, "asset"), "named context\n", 0751)
	imageFile := filepath.Join(f.root, "image.coopr")
	publicNativeWrite(t, imageFile, "from \"scratch\" {\n copy \"asset\" \"/asset\" from=\"assets\"\n}\n", 0600)
	imageRequest := publicNativeDefinition(f, imageFile, "build", "client-image", "")
	imageRequest.Options.Contexts = map[string]string{"assets": assets}
	image := awaitPublicNative(t, ctx, startPublicNative(ctx, c, imageRequest))
	if image.Cache != nil || publicNativeRead(t, filepath.Join(image.Filesystem, "asset")) != "named context\n" {
		t.Fatal("native image semantics or unavailable cache statistics changed")
	}
	componentFile := filepath.Join(f.root, "component.coopr")
	publicNativeWrite(t, componentFile, "package as=\"payload\" { copy \"busybox\" \"/busybox\" }\nextend { copy \"/busybox\" \"/busybox\" from=\"payload\" }\n", 0600)
	componentStore := filepath.Join(f.root, "publication-components")
	componentRequest := publicNativeDefinition(f, componentFile, "component", "client-component", componentStore)
	component := awaitPublicNative(t, ctx, startPublicNative(ctx, c, componentRequest))
	if component.Cache != nil || component.Reference == "" {
		t.Fatal("native component identity/statistics changed")
	}
	arrivals := make(chan string, 2)
	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	seen := map[string]bool{}
	storage := registry.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/") {
			repository := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/"), "/manifests/")[0]
			mu.Lock()
			first := !seen[repository]
			seen[repository] = true
			mu.Unlock()
			if first {
				arrivals <- repository
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
		}
		storage.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { once.Do(func() { close(release) }); server.Close() })
	host := strings.TrimPrefix(server.URL, "http://")
	publications := []api.Request{
		publicNativePublication(imageRequest, "image", image.ImageID, "", host+"/images/result:ready"),
		publicNativePublication(componentRequest, "component", component.Reference, componentStore, host+"/components/result:ready"),
	}
	first := startPublicNative(ctx, c, publications[0])
	second := startPublicNative(ctx, c, publications[1])
	var reached []string
	for len(reached) != 2 {
		select {
		case repository := <-arrivals:
			reached = append(reached, repository)
		case <-ctx.Done():
			t.Fatalf("publication requests did not overlap: %v", reached)
		}
	}
	slices.Sort(reached)
	if !slices.Equal(reached, []string{"components/result", "images/result"}) {
		t.Fatalf("publication rendezvous: %v", reached)
	}
	once.Do(func() { close(release) })
	for _, done := range []<-chan publicNativeResponse{first, second} {
		result := awaitPublicNative(t, ctx, done)
		if len(result.Destinations) != 1 || result.Destinations[0].Status != "passed" || !strings.Contains(result.Destinations[0].Reference, "@sha256:") {
			t.Fatalf("publication lost destination identity: %+v", result)
		}
	}
	// A later transport failure retains an earlier successful delivery.
	reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "publication deliberately rejected", http.StatusInternalServerError)
	}))
	t.Cleanup(reject.Close)
	good, existing := filepath.Join(f.root, "published-image"), filepath.Join(f.root, "existing-image")
	partial := <-startPublicNative(ctx, c, publicNativePublication(imageRequest, "image", image.ImageID, "", "oci:"+good, strings.TrimPrefix(reject.URL, "http://")+"/reject:result"))
	var remote *api.Error
	if !errors.As(partial.err, &remote) || len(partial.result.Destinations) != 2 || partial.result.Destinations[0].Status != "passed" || partial.result.Destinations[1].Status != "failed" {
		t.Fatalf("partial publication changed: %+v, %v\n%s", partial.result, partial.err, partial.logs)
	}
	if _, err := os.Stat(filepath.Join(good, "index.json")); err != nil {
		t.Fatal("earlier successful publication was lost")
	}
	if err := os.Mkdir(existing, 0700); err != nil {
		t.Fatal(err)
	}
	publicNativeWrite(t, filepath.Join(existing, "sentinel"), "preserved\n", 0600)
	refused := <-startPublicNative(ctx, c, publicNativePublication(imageRequest, "image", image.ImageID, "", "oci:"+existing))
	if !errors.As(refused.err, &remote) || publicNativeRead(t, filepath.Join(existing, "sentinel")) != "preserved\n" {
		t.Fatalf("existing destination was not preserved: %v", refused.err)
	}
	// Two real copies race for one destination; exactly one may promote it.
	race := publicNativePublication(imageRequest, "image", image.ImageID, "", "oci:"+filepath.Join(f.root, "racing-image"))
	racing := []<-chan publicNativeResponse{startPublicNative(ctx, c, race), startPublicNative(ctx, c, race)}
	successes := 0
	for _, done := range racing {
		response := <-done
		if response.err == nil {
			successes++
		} else if !errors.As(response.err, &remote) {
			t.Fatalf("racing publication lost structured error: %v", response.err)
		}
	}
	if successes != 1 {
		t.Fatalf("racing publications reported %d successes", successes)
	}
	if _, err := os.Stat(filepath.Join(f.root, "racing-image", "index.json")); err != nil {
		t.Fatal("winning publication is missing")
	}
	staging, err := filepath.Glob(filepath.Join(f.root, ".coopr-publish-*"))
	if err != nil || len(staging) != 0 {
		t.Fatalf("publication staging survived completion: %v, %v", staging, err)
	}
}

func TestPublicClientBaleNestedPackageRelocationWithoutSourceRegistry(t *testing.T) {
	f := newNativeAPIFixture(t)
	publicNativePolicy(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	t.Setenv("XDG_DATA_HOME", filepath.Join(f.root, "publisher-data"))
	c, stop := publicNativeClient(t)
	source := httptest.NewServer(registry.New())
	t.Cleanup(source.Close)
	sourceHost := strings.TrimPrefix(source.URL, "http://")
	publisher := filepath.Join(f.root, "publisher")
	if err := os.Mkdir(publisher, 0700); err != nil {
		t.Fatal(err)
	}
	publicNativeWrite(t, filepath.Join(publisher, "asset"), "packaged nested asset\n", 0751)
	childFile := filepath.Join(publisher, "child.coopr")
	publicNativeWrite(t, childFile, "package as=\"payload\" { copy \"asset\" \"/asset\" }\nextend {\n copy \"/asset\" \"/nested-asset\" from=\"payload\"\n env CHILD=\"delivered\"\n}\n", 0600)
	componentStore := filepath.Join(f.root, "publisher-components")
	childRequest := publicNativeDefinition(f, childFile, "component", "child", componentStore)
	childRequest.Options.ContextDir = publisher
	child := awaitPublicNative(t, ctx, startPublicNative(ctx, c, childRequest))
	childPublication := awaitPublicNative(t, ctx, startPublicNative(ctx, c, publicNativePublication(childRequest, "component", child.Reference, componentStore, sourceHost+"/child:stable")))
	childRef := childPublication.Destinations[0].Reference
	parentFile := filepath.Join(publisher, "parent.coopr")
	publicNativeWrite(t, parentFile, fmt.Sprintf("extend {\n component %q\n env PARENT=\"delivered\"\n}\n", childRef), 0600)
	parentRequest := publicNativeDefinition(f, parentFile, "component", "parent", componentStore)
	parentRequest.Options.ContextDir = publisher
	parent := awaitPublicNative(t, ctx, startPublicNative(ctx, c, parentRequest))
	parentPublication := awaitPublicNative(t, ctx, startPublicNative(ctx, c, publicNativePublication(parentRequest, "component", parent.Reference, componentStore, sourceHost+"/parent:stable")))
	parentRef := parentPublication.Destinations[0].Reference
	bundleClient, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{sourceHost}})
	if err != nil {
		t.Fatal(err)
	}
	created, err := bundleClient.Create(ctx, bale.Recipe{Include: []bale.Selection{{Ref: parentRef}}}, sourceHost+"/bundle:captured", bale.CreateOptions{})
	if err != nil || !created.Published {
		t.Fatalf("capture nested package graph: %+v, %v", created, err)
	}
	layout := filepath.Join(f.root, "delivery")
	if _, err := bundleClient.Pull(ctx, sourceHost+"/bundle:captured", layout, bale.PullLocked); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	reads := map[string]int{}
	storage := registry.New()
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			mu.Lock()
			path := strings.TrimPrefix(r.URL.Path, "/v2/")
			for _, marker := range []string{"/manifests/", "/blobs/", "/referrers/"} {
				if repository, _, ok := strings.Cut(path, marker); ok {
					reads[repository]++
					break
				}
			}
			mu.Unlock()
		}
		storage.ServeHTTP(w, r)
	}))
	t.Cleanup(destination.Close)
	destinationHost := strings.TrimPrefix(destination.URL, "http://")
	bundleClient, err = bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{destinationHost}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := bundleClient.PlanPush(ctx, layout, destinationHost+"/bundle:delivered", bale.PushOptions{RepositoryMap: map[string]string{sourceHost + "/parent": "components/parent", sourceHost + "/child": "components/child"}})
	if err != nil {
		t.Fatal(err)
	}
	// Capture ends before delivery: neither registry nor publisher context can
	// rescue a missing relocation/dependency during the consumer invocation.
	source.Close()
	probe := &http.Client{Timeout: time.Second}
	if response, err := probe.Get(source.URL + "/v2/"); err == nil {
		_ = response.Body.Close()
		t.Fatal("source registry remained reachable")
	}
	if err := os.RemoveAll(publisher); err != nil {
		t.Fatal(err)
	}
	stop()
	delivered, err := bundleClient.Push(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	_, parentDigest, ok := strings.Cut(parentRef, "@")
	if !ok {
		t.Fatalf("parent publication was not immutable: %q", parentRef)
	}
	relocation, ok := delivered.Relocations.Lookup(bale.ArtifactID{Repository: sourceHost + "/parent", Digest: digest.Digest(parentDigest)})
	if !ok {
		t.Fatal("delivery omitted parent relocation")
	}
	_, childDigest, ok := strings.Cut(childRef, "@")
	if !ok {
		t.Fatalf("child publication was not immutable: %q", childRef)
	}
	childRelocation, ok := delivered.Relocations.Lookup(bale.ArtifactID{Repository: sourceHost + "/child", Digest: digest.Digest(childDigest)})
	if !ok {
		t.Fatal("delivery omitted child relocation")
	}
	consumer := filepath.Join(f.root, "consumer")
	if err := os.Mkdir(consumer, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(f.root, "consumer-data"))
	c, _ = publicNativeClient(t)
	consumerFile := filepath.Join(f.root, "consumer.coopr")
	publicNativeWrite(t, consumerFile, fmt.Sprintf("from \"scratch\" {\n component %q\n}\n", relocation.Destination.Repository+"@"+relocation.Destination.Digest.String()), 0600)
	r := publicNativeDefinition(f, consumerFile, "build", "relocated", filepath.Join(f.root, "consumer-components"))
	r.Options.ContextDir, r.Options.Cache = consumer, api.Cache{NoCache: true, LocalDir: filepath.Join(f.root, "consumer-cache")}
	r.Options.Store = api.Store{RunRoot: filepath.Join(f.root, "fresh", "run"), GraphRoot: filepath.Join(f.root, "fresh", "graph"), Driver: "vfs"}
	mu.Lock()
	clear(reads)
	mu.Unlock()
	result := awaitPublicNative(t, ctx, startPublicNative(ctx, c, r))
	if publicNativeRead(t, filepath.Join(result.Filesystem, "nested-asset")) != "packaged nested asset\n" {
		t.Fatal("relocated PACKAGE asset bytes changed")
	}
	info, err := os.Stat(filepath.Join(result.Filesystem, "nested-asset"))
	if err != nil || info.Mode().Perm() != 0751 {
		t.Fatalf("relocated PACKAGE mode: %v, %v", info, err)
	}
	mu.Lock()
	parentReads := reads[strings.TrimPrefix(relocation.Destination.Repository, destinationHost+"/")]
	childReads := reads[strings.TrimPrefix(childRelocation.Destination.Repository, destinationHost+"/")]
	mu.Unlock()
	if parentReads == 0 || childReads == 0 {
		t.Fatalf("consumer did not fetch both relocated components: %v", reads)
	}
	index := v1.Index{}
	if err := json.Unmarshal([]byte(publicNativeRead(t, filepath.Join(result.OCI, "index.json"))), &index); err != nil || len(index.Manifests) != 1 {
		t.Fatalf("consumer OCI index: %+v, %v", index, err)
	}
	manifest := v1.Manifest{}
	if err := json.Unmarshal([]byte(publicNativeRead(t, filepath.Join(result.OCI, "blobs", "sha256", index.Manifests[0].Digest.Encoded()))), &manifest); err != nil {
		t.Fatal(err)
	}
	image := v1.Image{}
	if err := json.Unmarshal([]byte(publicNativeRead(t, filepath.Join(result.OCI, "blobs", "sha256", manifest.Config.Digest.Encoded()))), &image); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"CHILD=delivered", "PARENT=delivered"} {
		if !slices.Contains(image.Config.Env, value) {
			t.Fatalf("relocated component configuration lost %s: %v", value, image.Config.Env)
		}
	}
}
