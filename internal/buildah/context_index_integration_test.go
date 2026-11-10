package buildah

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCapturedRunContextCacheWithoutSessionManager(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	writeLinkedRunContext(t, contextDir)
	if err := os.Chmod(contextDir, 0750); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(123456789, 0)
	if err := os.Chtimes(contextDir, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	// Only this test and its children see the unusable endpoint. The explicit
	// cgroupfs path must not require a systemd user-session manager.
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+filepath.Join(root, "absent-bus"))
	plan := testPlan(t, `from "scratch" {
 copy "busybox" "/busybox"
 run network="none" {
  mount "bind" target="/src"
  exec "/busybox" "sh" "-ec" "/busybox stat -c '%a:%u:%g:%Y' /src >/metadata; /busybox cat /src/proof >/proof; /busybox od -An -N16 -tx1 /dev/urandom >/nonce"
 }
}`)
	store := cacheTestStore(filepath.Join(root, "store"))
	build := func(name string, wantTime int64) string {
		t.Helper()
		layout := filepath.Join(root, name)
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: contextDir, Isolation: "rootless", Runtime: "crun",
			RunControls: RunControls{CgroupManager: "cgroupfs", CgroupManagerSet: true},
			Output:      Output{Path: layout}, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		last := manifest.Layers[len(manifest.Layers)-1]
		blob := filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded())
		if got := readLayerFile(t, blob, "metadata"); got != fmt.Sprintf("750:0:0:%d\n", wantTime) {
			t.Fatalf("RUN did not see its keyed root metadata: %q", got)
		}
		if got := readLayerFile(t, blob, "proof"); got != "linked-run\n" {
			t.Fatalf("RUN did not see its captured payload: %q", got)
		}
		nonce := readLayerFile(t, blob, "nonce")
		if strings.TrimSpace(nonce) == "" {
			t.Fatal("RUN produced no execution nonce")
		}
		return nonce
	}
	cold := build("cold", stamp.Unix())
	if got := build("warm", stamp.Unix()); got != cold {
		t.Fatal("unchanged captured inputs reran RUN")
	}
	stamp = stamp.Add(time.Second)
	if err := os.Chtimes(contextDir, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	changed := build("root-changed", stamp.Unix())
	if changed == cold {
		t.Fatal("root-mtime change reused the earlier RUN")
	}
	if got := build("root-changed-warm", stamp.Unix()); got != changed {
		t.Fatal("unchanged new root metadata reran RUN")
	}
}

func TestSelectiveRunContextCachePreservesHardlinks(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	writeLinkedRunContext(t, contextDir)
	stamp := time.Unix(123456789, 0)
	write := func(name string) {
		t.Helper()
		path := filepath.Join(contextDir, name)
		if err := os.WriteFile(path, []byte("payload"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	write("a")
	write("unbound")
	if err := os.Link(filepath.Join(contextDir, "a"), filepath.Join(contextDir, "b")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+filepath.Join(root, "absent-bus"))
	plan := testPlan(t, `from "scratch" {
 copy "busybox" "/busybox"
 run network="none" {
  mount "bind" source="a" target="/left"
  mount "bind" source="b" target="/right"
  exec "/busybox" "sh" "-ec" "if /busybox test /left -ef /right; then echo same >/relation; else echo different >/relation; fi; /busybox stat -c '%h' /left /right >/links; /busybox cat /left /right >/payload; /busybox od -An -N16 -tx1 /dev/urandom >/nonce"
 }
}`)
	store := cacheTestStore(filepath.Join(root, "store"))
	build := func(name, relation string, links int) string {
		t.Helper()
		layout := filepath.Join(root, name)
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: contextDir, Isolation: "rootless", Runtime: "crun",
			RunControls: RunControls{CgroupManager: "cgroupfs", CgroupManagerSet: true},
			Output:      Output{Path: layout}, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		blob := filepath.Join(layout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded())
		if got := readLayerFile(t, blob, "relation"); got != relation+"\n" {
			t.Fatalf("%s inode relation=%q want=%s", name, got, relation)
		}
		if got := readLayerFile(t, blob, "links"); got != fmt.Sprintf("%d\n%d\n", links, links) {
			t.Fatalf("%s link counts=%q want=%d", name, got, links)
		}
		if got := readLayerFile(t, blob, "payload"); got != "payloadpayload" {
			t.Fatalf("%s selected payload=%q", name, got)
		}
		return readLayerFile(t, blob, "nonce")
	}
	cold := build("cold", "same", 2)
	if err := os.WriteFile(filepath.Join(contextDir, "unbound"), []byte("changed unrelated bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := build("unbound-changed", "same", 2); got != cold {
		t.Fatal("unbound payload invalidated native RUN")
	}
	alias := filepath.Join(contextDir, "outside-alias")
	if err := os.Link(filepath.Join(contextDir, "a"), alias); err != nil {
		t.Fatal(err)
	}
	if got := build("outside-link", "same", 3); got == cold {
		t.Fatal("outside link count change replayed native RUN")
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if got := build("outside-link-removed", "same", 2); got != cold {
		t.Fatal("restored declared inputs did not reuse their completed result")
	}
	if err := os.Remove(filepath.Join(contextDir, "b")); err != nil {
		t.Fatal(err)
	}
	write("b")
	for _, name := range []string{"a", "b"} {
		if err := os.Link(filepath.Join(contextDir, name), filepath.Join(contextDir, name+"-alias")); err != nil {
			t.Fatal(err)
		}
	}
	independent := build("independent", "different", 2)
	if independent == cold {
		t.Fatal("cross-mount relation with equal link counts replayed native RUN")
	}
	if got := build("independent-warm", "different", 2); got != independent {
		t.Fatal("unchanged independent view reran native RUN")
	}
}
