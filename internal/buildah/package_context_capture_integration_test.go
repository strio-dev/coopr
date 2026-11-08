package buildah

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

// A package lookup must retain the input view it measured. The remote ADD
// deliberately changes the caller's local files after aggregate key creation
// but before COPY, without relying on scheduler timing or a workload RUN.
func TestPublishPlanRetainsKeyedContextThroughExecution(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		t.Run(fmt.Sprintf("prepared=%t", prepared), func(t *testing.T) {
			testPublishPlanRetainsKeyedContextThroughExecution(t, prepared)
		})
	}
}

func testPublishPlanRetainsKeyedContextThroughExecution(t *testing.T, prepared bool) {
	if testing.Short() {
		t.Skip("requires native rootless storage for actual package publication")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o750); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(contextDir, "payload")
	stamp := time.Unix(123456789, 0)
	writePayload := func(value string) error {
		if err := os.WriteFile(payload, []byte(value), 0o640); err != nil {
			return err
		}
		return os.Chtimes(payload, stamp, stamp)
	}
	if err := writePayload("before"); err != nil {
		t.Fatal(err)
	}
	const trigger = "captured input trigger\n"
	var once sync.Once
	var requests atomic.Int32
	mutated := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(trigger)))
		if r.Method == http.MethodHead {
			return
		}
		requests.Add(1)
		once.Do(func() { mutated <- writePayload("after!") })
		_, _ = fmt.Fprint(w, trigger)
	}))
	defer server.Close()
	plan := testPublicationPlan(t, fmt.Sprintf(`
package as="payload" {
    add %q "/trigger" checksum=%q
    copy "payload" "/payload"
}
extend { copy "/payload" "/payload" from="payload" }
`, server.URL+"/trigger", digest.FromString(trigger)))
	build := func(name, want string) StoreOptions {
		t.Helper()
		store := StoreOptions{RunRoot: filepath.Join(root, name, "run"), GraphRoot: filepath.Join(root, name, "graph"), GraphDriverName: "vfs"}
		output := filepath.Join(root, name, "payload.tar")
		_, err := PublishPlan(ctx, plan, PublicationOptions{
			PlanOptions:  PlanOptions{Store: store, ContextDir: contextDir, ContextPrepared: prepared, Isolation: "rootless", CacheLocalDir: filepath.Join(root, "cache")},
			PackagePaths: map[string]string{"payload": output},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertPackageTarFile(t, output, "payload", want)
		return store
	}
	build("cold", "before")
	select {
	case err := <-mutated:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("remote ADD did not exercise the caller mutation")
	}
	if data, err := os.ReadFile(payload); err != nil || string(data) != "after!" {
		t.Fatalf("caller mutation was not retained: %q, %v", data, err)
	}
	coldRequests := requests.Load()
	if err := writePayload("before"); err != nil {
		t.Fatal(err)
	}
	warmStore := build("warm", "before")
	lease, err := acquireStore(warmStore)
	if err != nil {
		t.Fatal(err)
	}
	images, imageErr := lease.store.Images()
	closeErr := lease.Close()
	if err := errors.Join(imageErr, closeErr); err != nil {
		t.Fatal(err)
	}
	if len(images) != 0 || requests.Load() != coldRequests {
		t.Fatalf("warm package did not restore directly: images=%d requests=%d (cold=%d)", len(images), requests.Load(), coldRequests)
	}
	if err := writePayload("newest"); err != nil {
		t.Fatal(err)
	}
	build("changed", "newest")
}
