package buildah

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"coopr/internal/storeactivity"
)

func TestActivityRootsOmitsEmptyRoots(t *testing.T) {
	store := StoreOptions{GraphRoot: "/var/lib/containers/storage", RunRoot: "/run/containers/storage"}
	for _, tt := range []struct {
		name           string
		store          StoreOptions
		componentStore string
		want           []string
	}{
		{name: "native storage without image store", store: store, want: []string{store.GraphRoot, store.RunRoot}},
		{name: "native storage with component store", store: store, componentStore: "/components", want: []string{store.GraphRoot, store.RunRoot, "/components"}},
		{name: "all roots empty"},
		{name: "component store only", componentStore: "/components", want: []string{"/components"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ActivityRoots(tt.store, tt.componentStore); !slices.Equal(got, tt.want) {
				t.Fatalf("ActivityRoots() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestActivityRootsPreservesNativeStorageOrder(t *testing.T) {
	store := StoreOptions{GraphRoot: "/graph", RunRoot: "/run", ImageStore: "/images"}
	want := []string{store.GraphRoot, store.RunRoot, store.ImageStore, "/components"}
	if got := ActivityRoots(store, "/components"); !slices.Equal(got, want) {
		t.Fatalf("ActivityRoots() = %q, want %q", got, want)
	}
}

func TestBuildActivityLocksEachNativeStorageRoot(t *testing.T) {
	options := StoreOptions{GraphRoot: t.TempDir(), RunRoot: t.TempDir(), ImageStore: t.TempDir()}
	lease, err := acquirePlanActivity(context.Background(), PlanOptions{Store: options})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}()
	for _, root := range []string{options.GraphRoot, options.RunRoot, options.ImageStore} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		exclusive, err := storeactivity.AcquireExclusive(ctx, root)
		cancel()
		if exclusive != nil {
			_ = exclusive.Close()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("native root %q not protected by build lease: %v", root, err)
		}
	}
}
