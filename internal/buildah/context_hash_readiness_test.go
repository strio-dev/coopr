package buildah

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/opencontainers/runtime-spec/specs-go"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"golang.org/x/sys/unix"
)

func TestPackageContextIdentityHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := captureBuildContext(ctx, PlanOptions{ContextDir: filepath.Join(t.TempDir(), "unavailable")}, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled package input hash: %v", err)
	}
}

func TestPackageContextIdentityMatchesRetainedView(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "included"), 0750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "included", "input")
	for name, data := range map[string]string{
		"included/input": "before", "ignored": "ignored", "output": "output", ".cooprignore": "ignored\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(path, filepath.Join(root, "included", "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("included/input", filepath.Join(root, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(path, "user.coopr-context", []byte("retained"), 0); err != nil {
		if !errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.EPERM) {
			t.Fatal(err)
		}
		t.Logf("runner does not permit user xattrs: %v", err)
	}
	prepared, captured, err := captureBuildContext(t.Context(), PlanOptions{
		ContextDir: root, ContextArtifacts: []string{filepath.Join(root, "output")},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := captured.cleanup(); err != nil {
			t.Error(err)
		}
	}()
	for _, excluded := range []string{"ignored", "output"} {
		if _, err := os.Stat(filepath.Join(prepared.ContextDir, excluded)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("capture retained excluded %s: %v", excluded, err)
		}
	}
	if err := os.WriteFile(path, []byte("after!"), 0640); err != nil {
		t.Fatal(err)
	}
	rootInfo, err := os.Stat(prepared.ContextDir)
	if err != nil {
		t.Fatal(err)
	}
	// A producer may recreate an excluded path; the retained policy must still
	// remove it. Keep root time unchanged so equality tests the effective view.
	if err := os.WriteFile(filepath.Join(prepared.ContextDir, "output"), []byte("excluded producer output"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(prepared.ContextDir, rootInfo.ModTime(), rootInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	prepared.ContextArtifacts = append(prepared.ContextArtifacts, filepath.Dir(prepared.ContextDir))
	_, got, err := captureBuildContext(t.Context(), prepared, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = got.cleanup() }()
	if got.identity != captured.identity {
		t.Fatalf("package identity differs from retained native view: got=%s want=%s", got.identity, captured.identity)
	}
}

func TestPreparedContextCapturePreservesExistingOwnership(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input")
	if err := os.WriteFile(path, []byte("already mapped"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	builder := nativeBuilder{Builder: &upstream.Builder{IDMappingOptions: define.IDMappingOptions{
		UIDMap: []specs.LinuxIDMapping{{ContainerID: 0, HostID: uint32(os.Getuid() + 100000), Size: 1}},
		GIDMap: []specs.LinuxIDMapping{{ContainerID: 0, HostID: uint32(os.Getgid() + 100000), Size: 1}},
	}}}
	input, err := prepareRunInput(t.Context(), builder, root, nil, Run{contextPrepared: true, Mounts: []RunMount{{Type: "bind", Properties: map[string]string{"target": "/src", "readonly": "true"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := input.cleanup(); err != nil {
			t.Error(err)
		}
	}()
	after, err := os.Stat(filepath.Join(input.contextDir, "input"))
	if err != nil {
		t.Fatal(err)
	}
	a, b := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	if a.Uid != b.Uid || a.Gid != b.Gid {
		t.Fatalf("prepared input ownership was mapped again: before=%d:%d after=%d:%d", a.Uid, a.Gid, b.Uid, b.Gid)
	}
}

// Trigger ordinary context cancellation at the first stream check, after the
// preflight check and capture setup. This makes the pipe-abort regression
// deterministic without depending on disk speed or a large temporary file.
type cancelDuringContextCapture struct {
	context.Context
	cancel   context.CancelFunc
	onCancel func()
	checks   atomic.Int32
}

func (ctx *cancelDuringContextCapture) Err() error {
	if ctx.checks.Add(1) == 2 {
		ctx.onCancel()
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestContextSnapshotHonorsCancellation(t *testing.T) {
	for _, duringCapture := range []bool{false, true} {
		t.Run(map[bool]string{false: "before capture", true: "during capture"}[duringCapture], func(t *testing.T) {
			root, parent := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "input"), []byte("payload"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			captureContext := ctx
			var copiedBeforeCancel atomic.Bool
			if duringCapture {
				captureContext = &cancelDuringContextCapture{Context: ctx, cancel: cancel, onCancel: func() {
					matches, _ := filepath.Glob(filepath.Join(parent, "coopr-context-*", "context", "input"))
					for _, path := range matches {
						if data, err := os.ReadFile(path); err == nil && string(data) == "payload" {
							copiedBeforeCancel.Store(true)
						}
					}
				}}
			} else {
				cancel()
				root = filepath.Join(root, "unavailable")
			}
			_, cleanup, err := snapshotContext(captureContext, root, nil, nil, nil, parent)
			if cleanup != nil {
				defer func() { _ = cleanup() }()
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("capture cancellation error = %v", err)
			}
			if copiedBeforeCancel.Load() {
				t.Fatal("cancellation was checked only after the entire input had been copied")
			}
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 0 {
				t.Fatalf("canceled capture retained files: %v, %v", entries, err)
			}
			if !duringCapture {
				_, err := prepareRunInput(ctx, &recordingBuilder{}, root, nil, Run{contextPrepared: true, Mounts: []RunMount{{Type: "bind", Properties: map[string]string{"target": "/src"}}}})
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("RUN preparation ignored cancellation before policy resolution: %v", err)
				}
			}
		})
	}
}

func TestPreparedContextRunRetainsCapturedInput(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	operation := Run{contextPrepared: true, Command: []string{"true"}, Mounts: []RunMount{{Type: "bind", Properties: map[string]string{"target": "/src", "readonly": "true"}}}}
	input, err := prepareRunInput(t.Context(), &recordingBuilder{}, root, nil, operation)
	if err != nil {
		t.Fatal(err)
	}
	if input.cleanup != nil {
		defer func() {
			if err := input.cleanup(); err != nil {
				t.Error(err)
			}
		}()
	}
	if err := os.WriteFile(path, []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	builder := &inspectingRunBuilder{inspect: func(options upstream.RunOptions) error {
		data, err := os.ReadFile(filepath.Join(options.ContextDir, "input"))
		if err != nil || string(data) != "before" {
			t.Fatalf("RUN observed changed input after identity calculation: %q, %v", data, err)
		}
		return nil
	}}
	if err := applyOperationWithContext(builder, input.contextDir, nil, input.operation); err != nil {
		t.Fatal(err)
	}
	next, err := prepareRunInput(t.Context(), &recordingBuilder{}, root, nil, operation)
	if err != nil {
		t.Fatal(err)
	}
	if next.cleanup != nil {
		defer func() {
			if err := next.cleanup(); err != nil {
				t.Error(err)
			}
		}()
	}
	if data, err := os.ReadFile(filepath.Join(next.contextDir, "input")); err != nil || string(data) != "after!" {
		t.Fatalf("later RUN lost the writable context update: %q, %v", data, err)
	}
	if len(input.resolved) != 1 || len(next.resolved) != 1 || input.resolved[0].Identity == next.resolved[0].Identity {
		t.Fatal("changed context reused the earlier input identity")
	}
}

// A prepared context is deliberately writable across stages. Read-only access
// in one RUN does not authorize memoizing its identity for the whole build.
func TestPreparedContextIdentityDetectsSameSizeRestoredTimeMutation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(123456789, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	operation := Run{contextPrepared: true, Mounts: []RunMount{{Type: "bind", Properties: map[string]string{"target": "/src", "readonly": "true"}}}}
	identity := func() string {
		input, err := prepareRunInput(t.Context(), &recordingBuilder{}, root, nil, operation)
		if err != nil {
			t.Fatal(err)
		}
		if input.cleanup != nil {
			defer func() {
				if err := input.cleanup(); err != nil {
					t.Error(err)
				}
			}()
		}
		if len(input.resolved) != 1 || input.resolved[0].Kind != "context" {
			t.Fatal("missing content identity", input.resolved)
		}
		return input.resolved[0].Identity
	}
	first := identity()
	if repeated := identity(); repeated != first {
		t.Fatal("unchanged context produced a different identity", first, repeated)
	}
	if err := os.WriteFile(path, []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("fixture did not retain inode, size and mtime", before, after, err)
	}
	if changed := identity(); changed == first {
		t.Fatal("same-stat mutation reused the earlier content identity")
	}
}

// A mapping may change bytes without a pathname write or closing its writable
// file descriptor. Keep this case when introducing any incremental input index.
func TestPreparedContextIdentityDetectsMappedMutation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := file.WriteString("before"); err != nil {
		t.Fatal(err)
	}
	mapped, err := unix.Mmap(int(file.Fd()), 0, 6, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Munmap(mapped); err != nil {
			t.Error(err)
		}
	})
	// Fault the writable page before establishing the comparison timestamp.
	// Keep it dirty: flushing here can write-protect the page and make a later
	// fault update timestamps, hiding an already-writable mapping's mutation.
	mapped[0] = 'b'
	stamp := time.Unix(123456789, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	identity := func() string {
		t.Helper()
		input, err := prepareRunInput(t.Context(), &recordingBuilder{}, root, nil, Run{
			contextPrepared: true,
			Mounts:          []RunMount{{Type: "bind", Properties: map[string]string{"target": "/src", "readonly": "true"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if input.cleanup != nil {
			if err := input.cleanup(); err != nil {
				t.Fatal(err)
			}
		}
		if len(input.resolved) != 1 || input.resolved[0].Kind != "context" {
			t.Fatal("missing mapped input identity", input.resolved)
		}
		return input.resolved[0].Identity
	}
	first := identity()
	before, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	copy(mapped, "after!")
	after, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() {
		t.Fatal("mapped mutation replaced or resized the fixture")
	}
	if changed := identity(); changed == first {
		t.Fatal("mapped bytes reused the previous content identity")
	}
}

// Measures the existing real Buildah-selected stream; it is not a substitute
// hash, an assumed immutable input or a claimed incremental speed improvement.
func BenchmarkPreparedContextHash8MiB(b *testing.B) {
	root := b.TempDir()
	const size = 8 << 20
	if err := os.WriteFile(filepath.Join(root, "input"), bytes.Repeat([]byte("x"), size), 0600); err != nil {
		b.Fatal(err)
	}
	operation := Run{contextPrepared: true, Mounts: []RunMount{{Type: "bind", Properties: map[string]string{"target": "/src", "readonly": "true"}}}}
	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		input, err := prepareRunInput(b.Context(), &recordingBuilder{}, root, nil, operation)
		if err != nil {
			b.Fatal(err)
		}
		if input.cleanup != nil {
			if err := input.cleanup(); err != nil {
				b.Fatal(err)
			}
		}
	}
}
