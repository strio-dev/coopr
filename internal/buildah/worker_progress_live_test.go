package buildah

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/strio-dev/coopr/internal/planner"
	"go.podman.io/storage"
)

// Both independent stages must enter a blocked ADD. Jobs=2 forces isolated
// stage workers, so observing their prepare starts at the caller while the HTTP
// handlers remain blocked proves both nested stage and enclosing plan streams.
func TestWorkerProgressLiveAcrossPlanAndIsolatedStageWorkers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping actual rootless plan/stage worker progress in short mode")
	}
	for _, cancelBuild := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelBuild), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			requested := make(chan string, 2)
			var requestsMu sync.Mutex
			seenRequests := make(map[string]bool)
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				requestsMu.Lock()
				if !seenRequests[request.URL.Path] {
					seenRequests[request.URL.Path] = true
					requested <- request.URL.Path
				}
				requestsMu.Unlock()
				select {
				case <-release:
					_, _ = io.WriteString(response, "live worker progress proof\n")
				case <-request.Context().Done():
				}
			}))
			defer server.Close()
			root, err := os.MkdirTemp("", "coopr-live-worker-")
			if err != nil {
				t.Fatal(err)
			}
			joined := false
			t.Cleanup(func() {
				if joined {
					if err := os.RemoveAll(root); err != nil {
						t.Error(err)
					}
				} else {
					t.Log("Unconfirmed worker exit; retained private evidence:", root)
				}
			})
			layout := filepath.Join(root, "layout")
			definition := parseWorkerDefinition(t, fmt.Sprintf(`
from "scratch" as="left" {
 add %q "/left"
}
from "scratch" as="right" {
 add %q "/right"
}
from "scratch" {
 copy "/left" "/left" from="left"
 copy "/right" "/right" from="right"
}
`, server.URL+"/left", server.URL+"/right"))
			store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
			var eventsMu sync.Mutex
			var events []OperationProgress
			notices := make(chan OperationProgress, 2*operationProgressPhaseLimit)
			result := make(chan error, 1)
			go func() {
				_, err := BuildDefinitionSupervised(ctx, definition, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH}, SupervisedPlanOptions{
					Store: store, ContextDir: root, Isolation: "rootless", Jobs: 2, Output: Output{Path: layout}, Stdout: io.Discard, Stderr: io.Discard,
					Progress: func(event OperationProgress) {
						eventsMu.Lock()
						events = append(events, cloneOperationProgress(event))
						eventsMu.Unlock()
						notices <- event
					},
				})
				result <- err
			}()
			defer func() {
				cancel()
				unblock()
				server.CloseClientConnections()
				if !joined {
					select {
					case <-result: //nolint:errcheck // Failure cleanup cancels and joins the worker.
						joined = true
					case <-time.After(20 * time.Second):
						t.Error("worker exit remains unconfirmed after cleanup deadline")
					}
				}
			}()
			for range 2 {
				select {
				case <-requested:
				case err := <-result:
					joined = true
					t.Fatalf("build exited before both isolated ADDs blocked: %v", err)
				case <-ctx.Done():
					t.Fatal("both isolated ADDs did not reach fixture")
				}
			}
			pending := make(map[string]bool)
			for len(pending) < 2 {
				select {
				case event := <-notices:
					if event.Instruction == "add" && event.State == "started" && event.Phase == "prepare" {
						pending[event.ID] = true
					}
				case err := <-result:
					joined = true
					t.Fatalf("build exited while ADD fixture remained blocked: %v", err)
				case <-ctx.Done():
					t.Fatal("actual stage starts were not visible before worker completion")
				}
			}
			if cancelBuild {
				cancel()
			} else {
				unblock()
			}
			var buildErr error
			select {
			case buildErr = <-result:
				joined = true
			case <-time.After(20 * time.Second):
				t.Fatal("worker did not finish after release/cancellation")
			}
			if cancelBuild {
				if !errors.Is(buildErr, context.Canceled) {
					t.Fatalf("cancellation result: %v", buildErr)
				}
				if _, err := os.Lstat(layout); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("canceled worker published output: %v", err)
				}
			} else if buildErr != nil {
				t.Fatal(buildErr)
			}
			eventsMu.Lock()
			captured := append([]OperationProgress(nil), events...)
			eventsMu.Unlock()
			if err := validateOperationProgressBatch(captured); err != nil {
				t.Fatalf("invalid/duplicate nested live transitions: %v", err)
			}
			completions := make(map[string]int)
			for _, event := range captured {
				if pending[event.ID] && event.State != "started" {
					completions[event.ID]++
					if cancelBuild && event.State == "completed" {
						t.Fatalf("cancellation fabricated successful blocked ADD completion: %#v", event)
					}
				}
			}
			if !cancelBuild {
				for id := range pending {
					if completions[id] != 1 {
						t.Fatalf("start %s has %d completions", id, completions[id])
					}
				}
			}
			if err := WithStore(store, func(store storage.Store) error {
				containers, err := store.Containers()
				if err != nil {
					return err
				}
				if len(containers) != 0 {
					return fmt.Errorf("worker left %d containers", len(containers))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
