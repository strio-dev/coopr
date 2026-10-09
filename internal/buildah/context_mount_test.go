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
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
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

func TestPreparedReadonlyContextBindUsesSharedRelabel(t *testing.T) {
	contextDir := t.TempDir()
	called := false
	builder := &inspectingRunBuilder{inspect: func(options upstream.RunOptions) error {
		called = true
		if len(options.RunMounts) != 1 || !strings.Contains(options.RunMounts[0], "relabel=shared") || !strings.Contains(options.RunMounts[0], "readonly") {
			t.Fatalf("prepared readonly mounts=%v", options.RunMounts)
		}
		return nil
	}}
	if err := applyOperationWithContext(builder, contextDir, nil, Run{contextPrepared: true, Command: []string{"/bin/true"}, Mounts: []RunMount{{Type: "bind", Properties: map[string]string{"target": "/src", "readonly": "true"}}}}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("RUN was not applied")
	}
	for _, shared := range []bool{false, true} {
		mount := RunMount{Type: "bind", SharedRelabel: shared, PrivateRelabel: !shared, Properties: map[string]string{"target": "/src", "relabel": "private"}}
		serialized, err := serializeRunMount(mount, contextDir)
		if err != nil || strings.Contains(serialized, "relabel=shared") || strings.Count(serialized, "relabel=private") != 1 {
			t.Fatalf("authored relabel precedence=%q error=%v", serialized, err)
		}
	}
	if _, err := serializeRunMount(RunMount{Type: "bind", SharedRelabel: true, PrivateRelabel: true, Properties: map[string]string{"target": "/src"}}, contextDir); err == nil {
		t.Fatal("accepted contradictory executor relabel modes")
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
	if testing.Short() {
		t.Skip("skipping a live rootless context bind build in short mode")
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
		Pull: false, SignaturePolicyPath: policy,
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

func TestBuildPlanWritableContextPersistsAcrossStagesAndCopy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping native integration in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	plan := testPlan(t, fmt.Sprintf(`from %q as="producer"
run "printf generated >/src/generated" network="none" { mount "bind" target="/src" rw="true" }
from "producer"
run "cat /src/generated >/proof" network="none" { mount "bind" target="/src" }
copy "generated" "/copied"
`, base.reference))
	for iteration := 0; iteration < 2; iteration++ {
		layout := filepath.Join(root, fmt.Sprintf("layout-%d", iteration))
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{Store: store, ContextDir: contextDir, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout}, Stdout: io.Discard, Stderr: os.Stderr})
		if err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		last := filepath.Join(layout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded())
		if got := readLayerFile(t, last, "copied"); got != "generated" {
			t.Fatalf("copy = %q", got)
		}
		if _, err := os.Stat(filepath.Join(contextDir, "generated")); !os.IsNotExist(err) {
			t.Fatalf("host context mutated: %v", err)
		}
	}
}

func TestPinnedBuildahWritableContextPersistsForBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping native integration in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0700); err != nil {
		t.Fatal(err)
	}
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	file := filepath.Join(root, "Containerfile")
	source := fmt.Sprintf(`FROM %s AS producer
RUN --network=none --mount=type=bind,target=/src,rw printf generated >/src/generated
FROM producer
RUN --network=none --mount=type=bind,target=/src cat /src/generated >/proof
COPY generated /copied
`, base.reference)
	if err := os.WriteFile(file, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	lease, err := acquireStore(store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Errorf("close native store: %v", err)
		}
	}()
	export := filepath.Join(root, "native-rootfs")
	_, _, err = imagebuildah.BuildDockerfiles(ctx, lease.store, define.BuildOptions{ContextDirectory: contextDir, Layers: true, Isolation: define.IsolationOCIRootless, Runtime: "crun", CommonBuildOpts: &define.CommonBuildOptions{}, Compression: define.Uncompressed, Out: io.Discard, Err: os.Stderr, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true, BuildOutputs: []string{"type=local,dest=" + export}}, file)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"proof", "copied"} {
		got, err := os.ReadFile(filepath.Join(export, name))
		if err != nil || string(got) != "generated" {
			t.Fatalf("pinned Buildah %s=%q error=%v", name, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(contextDir, "generated")); !os.IsNotExist(err) {
		t.Fatalf("upstream host context mutated: %v", err)
	}
}

func TestDirectBuildWritableContextRetainsProtectedExclusions(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	for _, protected := range []bool{false, true} {
		source := "generated"
		if protected {
			source = "protected"
		}
		layout := filepath.Join(f.root, fmt.Sprintf("direct-%t", protected))
		_, err := Build(f.ctx, Request{
			Base: base.reference, Store: f.options.Store, ContextDir: f.contextDir,
			ContextArtifacts: []string{filepath.Join(f.contextDir, "protected")},
			Isolation:        "rootless", Runtime: "crun", Output: Output{Path: layout},
			Operations: []Operation{
				Run{Command: []string{"/bin/sh", "-c", "printf generated >/src/generated; /bin/busybox mkdir /src/protected; printf secret >/src/protected/secret"}, Network: "none", Mounts: []RunMount{{Type: "bind", Properties: map[string]string{"target": "/src", "rw": "true"}}}},
				Copy{Sources: []string{source}, Destination: "/proof"},
			},
		})
		if protected {
			if err == nil || !strings.Contains(err.Error(), "filtered out") {
				t.Fatalf("direct build copied recreated protected context: %v", err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			if got := localComponentLastFile(t, layout, "proof"); got != "generated" {
				t.Fatalf("direct context result=%q", got)
			}
		}
		if _, err := os.Stat(filepath.Join(f.contextDir, "generated")); !os.IsNotExist(err) {
			t.Fatalf("direct build modified host context: %v", err)
		}
	}
}

func TestDirectBuildWritableContextHonorsExplicitIgnore(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	f.write(t, ".dockerignore", "keep\ngenerated\n")
	f.write(t, "keep", "included by explicit policy")
	f.write(t, "hidden", "excluded by explicit policy")
	ignoreFile := filepath.Join(f.root, "custom.ignore")
	if err := os.WriteFile(ignoreFile, []byte("hidden\n"), 0600); err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(f.root, "custom-ignore")
	_, err := Build(f.ctx, Request{
		Base: base.reference, Store: f.options.Store, ContextDir: f.contextDir, IgnoreFile: ignoreFile,
		Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout},
		Operations: []Operation{
			Run{Command: []string{"/bin/sh", "-c", "test -f /src/keep && test ! -e /src/hidden && printf explicit >/src/generated"}, Network: "none", Mounts: []RunMount{{Type: "bind", Properties: map[string]string{"target": "/src", "rw": "true"}}}},
			Copy{Sources: []string{"generated"}, Destination: "/proof"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := localComponentLastFile(t, layout, "proof"); got != "explicit" {
		t.Fatalf("explicit policy result=%q", got)
	}
	if _, err := os.Stat(filepath.Join(f.contextDir, "generated")); !os.IsNotExist(err) {
		t.Fatalf("explicit-policy build modified host context: %v", err)
	}
}
