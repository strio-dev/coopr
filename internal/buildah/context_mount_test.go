package buildah

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
)

func TestRootFSIdentity(t *testing.T) {
	first := digest.FromString("first")
	second := digest.FromString("second")
	tests := []struct {
		name    string
		rootFS  v1.RootFS
		want    string
		wantErr string
	}{
		{name: "empty", rootFS: v1.RootFS{Type: "layers"}, want: "empty"},
		{name: "one layer", rootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{first}}, want: first.String()},
		{
			name:   "layer chain",
			rootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{first, second}},
			want:   digest.FromString(first.String() + " " + second.String()).String(),
		},
		{name: "unsupported type", rootFS: v1.RootFS{Type: "unknown"}, wantErr: "unsupported rootfs type"},
		{name: "invalid diff ID", rootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{"sha256:nope"}}, wantErr: "invalid diff ID 0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := rootFSIdentity(test.rootFS)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("identity = %q, want %q", got, test.want)
			}
		})
	}
}

func TestContextBindRunUsesFilteredEphemeralSnapshot(t *testing.T) {
	contextDir := filepath.Join(t.TempDir(), "context")
	if err := os.Mkdir(contextDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"included":      "include\n",
		"ignored":       "ignore\n",
		"discard.log":   "discard\n",
		"keep.log":      "keep\n",
		".dockerignore": "ignored\n*.log\n!keep.log\n!artifact-alias\n",
	} {
		if err := os.WriteFile(filepath.Join(contextDir, name), []byte(contents), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("included", filepath.Join(contextDir, "included-link")); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(contextDir, "coopr-store")
	if err := os.Mkdir(artifact, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifact, "private"), []byte("private\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifact, filepath.Join(contextDir, "artifact-alias")); err != nil {
		t.Fatal(err)
	}

	var snapshot string
	builder := &inspectingRunBuilder{inspect: func(options upstream.RunOptions) error {
		snapshot = options.ContextDir
		if snapshot == contextDir || filepath.Dir(snapshot) == contextDir {
			t.Fatalf("RUN context was not isolated: %q", snapshot)
		}
		wantMounts := []string{"type=bind,relabel=private,readonly,source=.,target=/src"}
		if !reflect.DeepEqual(options.RunMounts, wantMounts) {
			t.Fatalf("RUN mounts = %#v, want %#v", options.RunMounts, wantMounts)
		}
		for _, name := range []string{"included", "keep.log", ".dockerignore", "included-link"} {
			if _, err := os.Lstat(filepath.Join(snapshot, name)); err != nil {
				t.Errorf("snapshot omitted %q: %v", name, err)
			}
		}
		for _, name := range []string{"ignored", "discard.log", "coopr-store", "artifact-alias"} {
			if _, err := os.Lstat(filepath.Join(snapshot, name)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("snapshot exposed %q: %v", name, err)
			}
		}
		link, err := os.Readlink(filepath.Join(snapshot, "included-link"))
		if err != nil || link != "included" {
			t.Errorf("snapshot symlink = %q, %v", link, err)
		}
		info, err := os.Stat(snapshot)
		if err != nil || info.Mode().Perm() != 0o750 {
			t.Errorf("snapshot root mode = %v, %v", info.Mode().Perm(), err)
		}
		return nil
	}}
	err := applyOperationWithContext(builder, contextDir, []string{artifact}, Run{
		Command: []string{"/bin/true"},
		Mounts: []RunMount{{Type: "bind", Properties: map[string]string{
			"source": ".", "target": "/src", "readonly": "true",
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot == "" {
		t.Fatal("RUN did not receive a context snapshot")
	}
	if _, err := os.Lstat(filepath.Dir(snapshot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("context snapshot was not removed: %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(contextDir, "included")); err != nil || string(contents) != "include\n" {
		t.Fatalf("source context changed: %q, %v", contents, err)
	}
}

func TestContextBindSnapshotIsRemovedAfterRunFailure(t *testing.T) {
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "input"), []byte("input\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runErr := errors.New("run failed")
	var snapshot string
	builder := &inspectingRunBuilder{inspect: func(options upstream.RunOptions) error {
		snapshot = options.ContextDir
		return runErr
	}}
	err := applyOperationWithContext(builder, contextDir, nil, Run{
		Command: []string{"/bin/false"},
		Mounts:  []RunMount{{Type: "bind", Properties: map[string]string{"target": "/src"}}},
	})
	if !errors.Is(err, runErr) {
		t.Fatalf("RUN error = %v", err)
	}
	if _, err := os.Lstat(filepath.Dir(snapshot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed RUN context snapshot was not removed: %v", err)
	}
}

func TestRunWithoutContextBindUsesOriginalContext(t *testing.T) {
	contextDir := t.TempDir()
	builder := &inspectingRunBuilder{inspect: func(options upstream.RunOptions) error {
		if options.ContextDir != contextDir {
			t.Fatalf("RUN context = %q, want %q", options.ContextDir, contextDir)
		}
		return nil
	}}
	err := applyOperationWithContext(builder, contextDir, nil, Run{
		Command: []string{"/bin/true"},
		Mounts:  []RunMount{{Type: "tmpfs", Properties: map[string]string{"target": "/tmp"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPreparedRunInputCarriesCancellationIntoCacheLock(t *testing.T) {
	root := t.TempDir()
	mounts := []RunMount{{Type: "cache", Properties: map[string]string{
		"id": "critical", "target": "/cache", "sharing": "locked",
	}}}
	held, err := lockRunCacheMounts(context.Background(), root, mounts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.close() })
	called := false
	builder := &inspectingRunBuilder{inspect: func(upstream.RunOptions) error {
		called = true
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	prepared, err := prepareRunInput(ctx, builder, "", nil, Run{
		Command: []string{"/bin/true"}, Mounts: mounts, cacheLockRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	started := time.Now()
	err = prepared.operation.apply(builder, prepared.contextDir)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("prepared RUN error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("prepared RUN reached builder.run after cancellation")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("prepared RUN cancellation took %s", elapsed)
	}
}

func TestStageBindRunDoesNotSnapshotContext(t *testing.T) {
	const imageID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	builder := &inspectingRunBuilder{inspect: func(options upstream.RunOptions) error {
		if options.ContextDir != "" {
			t.Fatalf("RUN context = %q, want empty", options.ContextDir)
		}
		want := []string{"type=bind,from=" + imageID + ",source=/proof,target=/input"}
		if !reflect.DeepEqual(options.RunMounts, want) {
			t.Fatalf("RUN mounts = %#v, want %#v", options.RunMounts, want)
		}
		return nil
	}}
	err := applyOperationWithContext(builder, "", nil, Run{
		Command: []string{"/bin/true"},
		Mounts: []RunMount{{Type: "bind", Properties: map[string]string{
			"from": imageID, "source": "/proof", "target": "/input",
		}, BoundFrom: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBuildPlanContextBindUsesFilteredSnapshot(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless context bind build")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"included":      "visible\n",
		"ignored":       "hidden\n",
		".dockerignore": "ignored\n!graph-alias\n",
	} {
		if err := os.WriteFile(filepath.Join(contextDir, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runRoot := filepath.Join(contextDir, "run")
	graphRoot := filepath.Join(contextDir, "graph")
	store := StoreOptions{RunRoot: runRoot, GraphRoot: graphRoot, GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	if err := os.Symlink(graphRoot, filepath.Join(contextDir, "graph-alias")); err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(contextDir, "layout")
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf(`
from %q
run "test -f /src/included && test ! -e /src/ignored && test ! -e /src/run && test ! -e /src/graph && test ! -e /src/graph-alias && test ! -e /src/layout && cat /src/included >/result" network="none" {
  mount "bind" source="." target="/src" readonly="true"
}
`, base.reference))
	_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store:      store,
		ContextDir: contextDir, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout},
		ImageStoreDir: base.imageStoreDir, Pull: false, SignaturePolicyPath: policy,
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	last := manifest.Layers[len(manifest.Layers)-1]
	if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "result"); got != "visible\n" {
		t.Fatalf("context bind result = %q", got)
	}
}

type inspectingRunBuilder struct {
	recordingBuilder
	inspect func(upstream.RunOptions) error
}

func (builder *inspectingRunBuilder) run(command []string, options upstream.RunOptions) error {
	if builder.inspect != nil {
		return builder.inspect(options)
	}
	return builder.recordingBuilder.run(command, options)
}
