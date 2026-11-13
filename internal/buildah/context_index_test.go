package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/planner"
	"go.podman.io/storage/pkg/idtools"
	"golang.org/x/sys/unix"
)

func TestCapturedCopyInputResolvesNativeNamesAndSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"a.txt": "before", "dir/value": "target"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("dir", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	identity := func(source string) digest.Digest {
		t.Helper()
		capture, err := captureContext(t.Context(), root, nil, nil, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = capture.cleanup() }()
		operation := planner.Operation{Instruction: definition.Instruction{Name: "copy", Arguments: []string{source, "/output/"}}}
		got, err := capturedCopyInput(t.Context(), capture, contextPolicy{directory: capture.directory}, operation)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	before := identity("*.txt")
	if err := os.Rename(filepath.Join(root, "a.txt"), filepath.Join(root, "b.txt")); err != nil {
		t.Fatal(err)
	}
	renamed := identity("*.txt")
	if renamed == before {
		t.Fatal("glob-matched rename with identical payload reused COPY input")
	}
	stamp := time.Unix(1900000000, 0)
	if err := os.Chtimes(filepath.Join(root, "b.txt"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if got := identity("*.txt"); got != renamed {
		t.Fatal("COPY timestamp-only change invalidated input")
	}
	if err := os.WriteFile(filepath.Join(root, "a\xff.txt"), []byte("binary"), 0640); err != nil {
		t.Fatal(err)
	}
	binaryBefore := identity("*.txt")
	if err := os.Rename(filepath.Join(root, "a\xff.txt"), filepath.Join(root, "a\xfe.txt")); err != nil {
		t.Fatal(err)
	}
	if got := identity("*.txt"); got == binaryBefore {
		t.Fatal("glob-matched binary filename rename reused COPY input")
	}
	linked := identity("alias/value")
	if err := os.WriteFile(filepath.Join(root, "dir", "value"), []byte("change"), 0640); err != nil {
		t.Fatal(err)
	}
	if got := identity("alias/value"); got == linked {
		t.Fatal("intermediate symlink hid a same-size target payload change")
	}
	linked = identity("alias/value")
	if err := os.Mkdir(filepath.Join(root, "other"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other", "value"), []byte("change"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if got := identity("alias/value"); got == linked {
		t.Fatal("symlink retarget lost the native exclusion coordinate")
	}
	if err := os.Symlink("dir/value", filepath.Join(root, "file-link")); err != nil {
		t.Fatal(err)
	}
	if got := identity("file-link"); got == "" {
		t.Fatal("top-level source symlink has no COPY identity")
	}
}

func TestPreparedRunContextSelectionIgnoresUnboundPayload(t *testing.T) {
	root := t.TempDir()
	for name, data := range map[string]string{"selected": "before", "unbound": "before"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Unix(123456789, 0)
	if err := os.Chtimes(filepath.Join(root, "selected"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	identity := func(source string) string {
		t.Helper()
		input, err := prepareRunInput(t.Context(), &recordingBuilder{}, root, nil, Run{Mounts: []RunMount{{Type: "bind", Properties: map[string]string{"source": source, "target": "/input"}}}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := input.cleanup(); err != nil {
				t.Error(err)
			}
		}()
		return input.resolved[0].Identity
	}
	before := identity("selected")
	if err := os.WriteFile(filepath.Join(root, "unbound"), []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	if after := identity("selected"); after != before {
		t.Fatal("unbound payload invalidated the selected read-only file", before, after)
	}
	if err := os.WriteFile(filepath.Join(root, "selected"), []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "selected"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if changed := identity("selected"); changed == before {
		t.Fatal("selected same-size/restored-time mutation reused input identity")
	}
}

func TestCapturedRunMountSelectionMatchesNativeSources(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "value"), []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"directory-link": "/dir", "file-link": "dir/value", "loop": "loop"} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	capture, err := captureContext(t.Context(), root, nil, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = capture.cleanup() }()
	mount := func(source string) RunMount {
		return RunMount{Type: "bind", Properties: map[string]string{"source": source, "target": "/input"}}
	}
	baseline, err := capture.runMountIdentity(t.Context(), []RunMount{mount("dir/value")})
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"/dir/value", "/../../dir/value", "directory-link/value", "file-link", "directory-link/../dir/value"} {
		got, err := capture.runMountIdentity(t.Context(), []RunMount{mount(source)})
		if err != nil || got != baseline {
			t.Fatalf("native source %q differs: %s %v", source, got, err)
		}
	}
	for _, source := range []string{"missing", "loop", "../outside"} {
		if got, err := capture.runMountIdentity(t.Context(), []RunMount{mount(source)}); err == nil {
			t.Fatalf("unavailable source %q accepted: %s", source, got)
		}
	}
	for _, property := range []string{"rw", "U"} {
		selected := mount("dir/value")
		selected.Properties[property] = "true"
		if got, err := capture.runMountIdentity(t.Context(), []RunMount{selected}); err != nil || got != capture.identity {
			t.Fatalf("mutating mount %s used selective identity: %s %v", property, got, err)
		}
	}
	if got, err := capture.runMountIdentity(t.Context(), []RunMount{mount("/")}); err != nil || got != capture.identity {
		t.Fatalf("single whole-context mount changed its complete identity: %s %v", got, err)
	}
}

func TestCapturedContextIdentityUsesMaterializedMetadata(t *testing.T) {
	root := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(filepath.Join(root, "source"), 0750); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(123456789, 0)
	for _, name := range []string{"input", "source/input", "\xff", "excluded"} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte("payload"), 0640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(root, "input"), filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("source/input", filepath.Join(root, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(root, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	identity := func() digest.Digest {
		t.Helper()
		capture, err := captureContext(t.Context(), root, []string{"excluded*"}, nil, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := capture.cleanup(); err != nil {
				t.Error(err)
			}
		}()
		if err := capture.identity.Validate(); err != nil {
			t.Fatal(err)
		}
		return capture.identity
	}
	baseline := identity()
	if got := identity(); got != baseline {
		t.Fatal("fresh capture roots/inodes changed identical input", baseline, got)
	}
	if err := os.WriteFile(filepath.Join(root, "excluded"), []byte("different excluded bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := identity(); got != baseline {
		t.Fatal("excluded payload changed retained identity", baseline, got)
	}
	if err := os.WriteFile(filepath.Join(root, "excluded-new"), []byte("excluded"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := identity(); got == baseline {
		t.Fatal("observable root mtime change was omitted")
	}
	if err := os.Chtimes(root, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if got := identity(); got != baseline {
		t.Fatal("excluded entry survived after restoring root mtime", baseline, got)
	}
	if err := os.Chmod(filepath.Join(root, "input"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := identity(); got == baseline {
		t.Fatal("materialized mode change was omitted")
	}
	if err := os.Chmod(filepath.Join(root, "input"), 0640); err != nil {
		t.Fatal(err)
	}
	if got := identity(); got != baseline {
		t.Fatal("ctime-only change invalidated materialized identity", baseline, got)
	}
	if err := unix.Setxattr(filepath.Join(root, "input"), "user.coopr-binary", []byte{0xff, 0, 0xfe}, 0); err == nil {
		if got := identity(); got == baseline {
			t.Fatal("materialized binary xattr was omitted")
		}
		if err := unix.Removexattr(filepath.Join(root, "input"), "user.coopr-binary"); err != nil {
			t.Fatal(err)
		}
	} else if !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EPERM) {
		t.Fatal(err)
	} else {
		t.Logf("runner does not permit user xattrs: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "hardlink"), []byte("payload"), 0640); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, filepath.Join(root, "hardlink")} {
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if got := identity(); got == baseline {
		t.Fatal("hardlink split with identical bytes/metadata was omitted")
	}
}

func TestCapturedContextMetadataDoesNotRereadPayload(t *testing.T) {
	source := t.TempDir()
	path := filepath.Join(source, "input")
	stamp := time.Unix(123456789, 0)
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	capture, err := captureContext(t.Context(), source, nil, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := capture.cleanup(); err != nil {
			t.Error(err)
		}
	}()
	selected, err := capture.index.Select(t.Context(), []string{"input"})
	if err != nil {
		t.Fatal(err)
	}
	copyOperation := planner.Operation{Instruction: definition.Instruction{Name: "copy", Arguments: []string{"input", "/input"}}}
	copySelected, err := capturedCopyInput(t.Context(), capture, contextPolicy{directory: capture.directory}, copyOperation)
	if err != nil {
		t.Fatal(err)
	}
	// Caller mutation must not change the execution-owned version. A new
	// capture must still observe same-size, restored-mtime content changes.
	if err := os.WriteFile(path, []byte("caller"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	owned := filepath.Join(capture.directory, "input")
	if data, err := os.ReadFile(owned); err != nil || string(data) != "before" {
		t.Fatalf("caller changed retained bytes: %q %v", data, err)
	}
	fresh, err := captureContext(t.Context(), source, nil, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := fresh.cleanup(); err != nil {
			t.Error(err)
		}
	}()
	if got, err := fresh.index.Select(t.Context(), []string{"input"}); err != nil || got == selected {
		t.Fatalf("same-stat caller mutation reused selected identity: %s %v", got, err)
	}
	if err := os.Chmod(owned, 0); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if _, err := os.ReadFile(owned); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("fixture did not deny payload reads: %v", err)
		}
	}
	if got, err := capture.index.Select(t.Context(), []string{"input"}); err != nil || got != selected {
		t.Fatalf("frozen selection reread bytes: %s %v", got, err)
	}
	if got, err := capturedCopyInput(t.Context(), capture, contextPolicy{directory: capture.directory}, copyOperation); err != nil || got != copySelected {
		t.Fatalf("COPY selection reread payload: %s %v", got, err)
	}
	payloads := map[string]contextPayload{"input": {digest: digest.FromString("before"), size: 6}}
	index, err := indexCapturedContext(t.Context(), capture.directory, payloads)
	if err != nil {
		t.Fatalf("metadata identity attempted a payload read: %v", err)
	}
	if _, err := index.Select(t.Context(), []string{"input"}); err != nil {
		t.Fatalf("selection attempted a payload read: %v", err)
	}
	for name, invalid := range map[string]map[string]contextPayload{
		"missing":    {},
		"wrong size": {"input": {digest: digest.FromString("before"), size: 7}},
		"extra":      {"input": payloads["input"], "absent": payloads["input"]},
		"bad digest": {"input": {digest: "sha256", size: 6}},
		"wrong link": {"input": {link: "absent"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := indexCapturedContext(t.Context(), capture.directory, invalid); err == nil {
				t.Fatal("unmatched materialized payload accepted")
			}
		})
	}
}

func nativePayloadArchive(t *testing.T, prefix string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, header := range []tar.Header{
		{Name: prefix + "input", Typeflag: tar.TypeReg, Size: 7},
		{Name: prefix + "hardlink", Typeflag: tar.TypeLink, Linkname: prefix + "input"},
	} {
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := writer.Write([]byte("payload")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestContextPayloadObserverNativeCoordinatesAndFailures(t *testing.T) {
	for _, prefix := range []string{"source/", "/"} {
		t.Run(prefix, func(t *testing.T) {
			input := nativePayloadArchive(t, prefix)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			stream, finish := observeContextPayloads(ctx, bytes.NewReader(input), "/source")
			got, err := io.ReadAll(stream)
			payloads, finishErr := finish(err)
			if err != nil || finishErr != nil || !bytes.Equal(got, input) {
				t.Fatalf("observer changed native stream: %v %v", err, finishErr)
			}
			if len(payloads) != 2 || payloads["input"].digest != digest.FromString("payload") || payloads["hardlink"].link != "input" {
				t.Fatalf("native root coordinates differ: %#v", payloads)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	stream, finish := observeContextPayloads(ctx, bytes.NewReader(nativePayloadArchive(t, "source/")), "/source")
	cancel()
	_, err := io.Copy(io.Discard, stream)
	if payloads, finishErr := finish(err); !errors.Is(finishErr, context.Canceled) || payloads != nil {
		t.Fatalf("canceled observer published payloads: %#v %v", payloads, finishErr)
	}
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for name, input := range map[string][]byte{
		"malformed":      bytes.Repeat([]byte{'x'}, 512),
		"truncated body": nativePayloadArchive(t, "source/")[:515],
	} {
		t.Run(name, func(t *testing.T) {
			stream, finish := observeContextPayloads(ctx, bytes.NewReader(input), "/source")
			_, err := io.Copy(io.Discard, stream)
			if payloads, finishErr := finish(err); finishErr == nil || payloads != nil {
				t.Fatalf("invalid archive published payloads: %#v %v", payloads, finishErr)
			} else if name == "truncated body" && !errors.Is(finishErr, io.ErrUnexpectedEOF) {
				t.Fatalf("native tar.Reader lost truncated body error: %v", finishErr)
			}
		})
	}

}

func TestIndexedCaptureCancellationRemovesPartialFiles(t *testing.T) {
	root, parent := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "input"), bytes.Repeat([]byte("payload"), 1<<16), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	duringCapture := &cancelDuringContextCapture{Context: ctx, cancel: cancel, onCancel: func() {}}
	if _, err := captureContext(duringCapture, root, nil, nil, nil, true, parent); !errors.Is(err, context.Canceled) {
		t.Fatalf("indexed capture cancellation lost error: %v", err)
	}
	if files, err := os.ReadDir(parent); err != nil || len(files) != 0 {
		t.Fatalf("indexed cancellation retained partial inputs: %v %v", files, err)
	}
}

func TestIndexedCaptureErrorsRemovePartialFiles(t *testing.T) {
	for _, putFailure := range []bool{false, true} {
		t.Run(fmt.Sprint("put=", putFailure), func(t *testing.T) {
			root, parent := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "input"), bytes.Repeat([]byte("payload"), 1<<16), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			excludes := []string{"["}
			var uidMap, gidMap []idtools.IDMap
			if putFailure {
				excludes = nil
				// Native copier activates mappings as a UID/GID pair. Omit
				// its required default UID 0, forcing rejection before chown.
				uidMap = []idtools.IDMap{{ContainerID: os.Geteuid() + 1, HostID: os.Geteuid(), Size: 1}}
				gidMap = []idtools.IDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}}
			}
			capture, err := captureContext(ctx, root, excludes, uidMap, gidMap, true, parent)
			if err == nil || errors.Is(err, context.DeadlineExceeded) || capture.identity != "" || capture.cleanup != nil {
				t.Fatalf("native copy error did not join without publication: capture=%+v error=%v", capture, err)
			}
			t.Logf("native copier failed before deadline: %v", err)
			if files, err := os.ReadDir(parent); err != nil || len(files) != 0 {
				t.Fatalf("failed indexed capture retained partial inputs: %v %v", files, err)
			}
		})
	}
}
