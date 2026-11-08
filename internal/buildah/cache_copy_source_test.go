package buildah

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/strio-dev/coopr/internal/definition"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"golang.org/x/sys/unix"
)

func TestBuildPlanWarmSimpleCopyUsesNativeAppliedDigest(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "source"), []byte("warm\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	store := cacheTestStore(root)
	plan := testPlan(t, `from "scratch" {
    copy "source" "/new-parent/renamed"
}
`)
	wantXattr := ""
	build := func(name string, force bool, wantCache string) digest.Digest {
		t.Helper()
		layout := filepath.Join(root, name)
		lookup := ""
		if _, err := BuildPlan(ctx, plan, PlanOptions{
			Store: store, ContextDir: contextDir, Isolation: "rootless", Output: Output{Path: layout}, NoCache: force,
			Progress: func(event OperationProgress) {
				if event.Instruction == "copy" && event.Operation == 2 && event.Phase == "lookup" && event.State == "completed" {
					lookup = event.Cache
				}
			},
		}); err != nil {
			t.Fatal(err)
		}
		if lookup != wantCache {
			t.Fatalf("%s COPY lookup = %q, want %q", name, lookup, wantCache)
		}
		manifest, _ := readPlanImage(t, layout)
		if len(manifest.Layers) != 1 {
			t.Fatalf("%s layers = %d, want 1", name, len(manifest.Layers))
		}
		layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
		want, err := os.ReadFile(filepath.Join(contextDir, "source"))
		if err != nil {
			t.Fatal(err)
		}
		if got := readLayerFile(t, layer, "new-parent/renamed"); got != string(want) {
			t.Fatalf("%s exported payload = %q, want %q", name, got, want)
		}
		info, err := os.Stat(filepath.Join(contextDir, "source"))
		if err != nil {
			t.Fatal(err)
		}
		header := readLayerHeader(t, layer, "new-parent/renamed")
		if header.Mode&0o777 != int64(info.Mode().Perm()) || header.PAXRecords["SCHILY.xattr.user.coopr-cache"] != wantXattr {
			t.Fatalf("%s exported metadata = %+v, want mode %o, xattr %q", name, header, info.Mode().Perm(), wantXattr)
		}
		return manifest.Layers[0].Digest
	}
	coldLayer := build("cold", false, "miss")
	// Buildah excludes source timestamps from its content digest.
	future := time.Unix(1900000000, 0)
	if err := os.Chtimes(filepath.Join(contextDir, "source"), future, future); err != nil {
		t.Fatal(err)
	}
	warmLayer := build("warm", false, "hit")
	if warmLayer != coldLayer {
		t.Fatalf("warm layer %s differs from cold %s", warmLayer, coldLayer)
	}
	forcedLayer := build("forced", true, "miss")
	forcedHeader := readLayerHeader(t, filepath.Join(root, "forced", "blobs", "sha256", forcedLayer.Encoded()), "new-parent/renamed")
	if forcedLayer == warmLayer || !forcedHeader.ModTime.Equal(future) {
		t.Fatal("forced COPY reused the older timestamped layer")
	}
	previousLayer := forcedLayer
	for _, change := range []struct {
		name  string
		apply func() error
	}{
		{"content", func() error {
			return os.WriteFile(filepath.Join(contextDir, "source"), []byte("changed payload\n"), 0o640)
		}},
		{"mode", func() error { return os.Chmod(filepath.Join(contextDir, "source"), 0o600) }},
		{"xattr", func() error {
			if err := unix.Setxattr(filepath.Join(contextDir, "source"), "user.coopr-cache", []byte("changed"), 0); err != nil {
				return err
			}
			wantXattr = "changed"
			return nil
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			if err := change.apply(); err != nil {
				t.Fatal(err)
			}
			changedLayer := build("changed-"+change.name, false, "miss")
			if changedLayer == previousLayer {
				t.Fatalf("changed %s reused preceding layer %s", change.name, previousLayer)
			}
			previousLayer = changedLayer
		})
	}
}

func TestBuildahCopyPreservesDigesterAndHashesAppliedContent(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("payload"), 0o640); err != nil {
		t.Fatal(err)
	}
	lease, err := acquireStore(cacheTestStore(filepath.Join(root, "store")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}()
	builder, err := upstream.NewBuilder(ctx, lease.store, upstream.BuilderOptions{FromImage: "scratch", PullPolicy: define.PullNever, Isolation: define.IsolationChroot})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := builder.Delete(); err != nil {
			t.Error(err)
		}
	}()
	builder.ContentDigester.Start("")
	if _, err := builder.ContentDigester.Hash().Write([]byte("previous")); err != nil {
		t.Fatal(err)
	}
	beforeType, beforeDigest := builder.ContentDigester.Digest()
	adapter := nativeBuilder{Builder: builder, isolation: define.IsolationChroot}
	operation := Copy{Sources: []string{"source"}, Destination: "/created/copied"}
	applied, err := applyCopyAddWithDigestContext(ctx, adapter, root, nil, operation)
	if err != nil {
		t.Fatal(err)
	}
	afterType, afterDigest := builder.ContentDigester.Digest()
	if beforeType != afterType || beforeDigest != afterDigest {
		t.Fatal("apply changed previous native digester state")
	}
	// Consecutive applications must not accumulate the previous input digest.
	if repeated, err := applyCopyAddWithDigestContext(ctx, adapter, root, nil, operation); err != nil || repeated != applied {
		t.Fatalf("consecutive digest=%s, err=%v; want %s", repeated, err, applied)
	}
	inline := Copy{InlineFiles: []definition.InlineFile{{Path: "first", Data: "one"}, {Path: "second", Data: "two"}}, Destination: "/inline/"}
	inlineApplied, err := applyCopyAddWithDigestContext(ctx, adapter, root, nil, inline)
	if err != nil {
		t.Fatal(err)
	}
	inline.InlineFiles[1].Data = "changed"
	changedInline, err := applyCopyAddWithDigestContext(ctx, adapter, root, nil, inline)
	if err != nil || changedInline == inlineApplied {
		t.Fatalf("changed inline digest %s: %v", changedInline, err)
	}
	afterType, afterDigest = builder.ContentDigester.Digest()
	if beforeType != afterType || beforeDigest != afterDigest {
		t.Fatal("inline apply changed previous native digester state")
	}
	mount, err := builder.Mount(builder.MountLabel)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"created/copied": "payload", "inline/first": "one", "inline/second": "changed"} {
		if data, err := os.ReadFile(filepath.Join(mount, path)); err != nil || string(data) != want {
			t.Fatalf("applied %s = %q, %v; want %q", path, data, err, want)
		}
	}
}
