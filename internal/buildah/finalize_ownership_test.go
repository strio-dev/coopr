package buildah

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	upstream "go.podman.io/buildah"
	nettypes "go.podman.io/common/libnetwork/types"
	imagetypes "go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

type finalizationNameStore struct {
	storage.Store
	names   []string
	stopped error
}

func (store *finalizationNameStore) Containers() ([]storage.Container, error) { return nil, nil }
func (store *finalizationNameStore) CreateContainer(_ string, names []string, _, _, _ string, _ *storage.ContainerOptions) (*storage.Container, error) {
	store.names = append(store.names, names...)
	return nil, store.stopped
}

type finalizationNameNetwork struct{ nettypes.ContainerNetwork }

func TestFinalizationBuilderHasJobOwnershipBeforeCreation(t *testing.T) {
	for _, jobID := range []string{strings.Repeat("a", 32), ""} {
		label := "supervised"
		if jobID == "" {
			label = "unsupervised"
		}
		t.Run(label, func(t *testing.T) {
			sentinel := errors.New("stop before creating native storage")
			store := &finalizationNameStore{stopped: sentinel}
			counter := uint64(7)
			executor := &graphExecutor{store: store, sharedBuilderCounter: &counter,
				options: PlanOptions{JobID: jobID, Output: Output{Filesystem: FilesystemOutput{Type: "local", Path: "/unused-finalization-output"}}}}
			options := upstream.BuilderOptions{Container: "prior-stage-builder", NetworkInterface: finalizationNameNetwork{},
				SystemContext: &imagetypes.SystemContext{SystemRegistriesConfPath: "/unused-registries.conf"}}
			for attempt := 0; attempt < 2; attempt++ {
				_, err := executor.finalizeOutput(context.Background(), Result{}, "", options, nil)
				if !errors.Is(err, sentinel) {
					t.Fatalf("finalization did not reach intercepted CreateContainer: %v", err)
				}
				if len(store.names) != attempt+1 {
					t.Fatalf("CreateContainer names: %q", store.names)
				}
				want := "working-container"
				if jobID != "" {
					want = fmt.Sprintf("coopr-%s-%d-%d-final", jobID, os.Getpid(), 8+attempt)
				}
				if got := store.names[attempt]; got != want {
					t.Fatalf("finalization builder name = %q, want %q", got, want)
				}
				if options.Container != "prior-stage-builder" {
					t.Fatal("finalization mutated caller builder options")
				}
			}
			wantCounter := uint64(7)
			if jobID != "" {
				wantCounter = 9
			}
			if counter != wantCounter {
				t.Fatalf("shared builder counter = %d, want %d", counter, wantCounter)
			}
		})
	}
}
