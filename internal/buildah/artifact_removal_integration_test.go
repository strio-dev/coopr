package buildah

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/common/pkg/libartifact"
	artifacttypes "go.podman.io/common/pkg/libartifact/types"
)

func TestArtifactCopyRejectsRemovalBetweenProbeAndApply(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx := t.Context()
	store, system := artifactSourceTestStore(t)
	const reference = "registry.example/team/files:latest"
	artifactSourceTestAdd(t, ctx, store, reference, map[string]string{"payload": "original"})
	resolver, err := oci.NewResolver(oci.Options{PullPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := resolveFileSource(ctx, resolver, reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, store, system)
	if err != nil {
		t.Fatal(err)
	}
	builder, err := upstream.NewBuilder(ctx, store, upstream.BuilderOptions{
		FromImage: "scratch", PullPolicy: define.PullNever,
		Isolation: define.IsolationChroot, SystemContext: system,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := builder.Delete(); err != nil {
			t.Error(err)
		}
	})
	adapter := nativeBuilder{Builder: builder, isolation: define.IsolationChroot}
	operation := copyFromImageOperation{
		store: store, artifact: &selected,
		sources: []string{"payload"}, destination: "/copied",
	}
	if _, err := probeCopyAddDigestContext(ctx, adapter, "", nil, operation); err != nil {
		t.Fatalf("probe artifact input: %v", err)
	}
	artifacts, err := libartifact.NewArtifactStore(filepath.Join(store.GraphRoot(), "artifacts"), system)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := libartifact.NewArtifactStorageReference(reference)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifacts.Remove(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if applied, err := applyCopyAddWithDigestContext(ctx, adapter, "", nil, operation); !errors.Is(err, artifacttypes.ErrArtifactNotExist) || applied != "" {
		t.Fatalf("removed artifact apply returned digest %q, error %v", applied, err)
	}
	mount, err := builder.Mount(builder.MountLabel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(mount, "copied")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed artifact copy left a destination payload: %v", err)
	}
}
