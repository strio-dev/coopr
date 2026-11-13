package buildah

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildPlanDeclaredGraphRestoresOnlyTerminalImage(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	root, input := copyDirectoryCacheContext(t)
	policy := writeComponentTestPolicy(t, root)
	cache := CacheSpec{Transport: "oci-layout", Reference: filepath.Join(root, "cache")}
	plan := testPlan(t, `
from "scratch" as="base" {
    copy "busybox" "/bin/busybox"
    env "BASE" "build-only"
}
from "base" as="builder" {
    workdir "/workspace"
    copy "input" "/input"
    run network="none" {
        exec "/bin/busybox" "sh" "-ec" "/bin/busybox cat /input > /proof; /bin/busybox od -An -N16 -tx1 /dev/urandom >> /proof; /bin/busybox chmod 0640 /proof"
    }
}
from "scratch" {
    copy "/proof" "/proof" from="builder"
    env "CHANNEL" "preview"
    cmd { exec "/proof"; }
}
`)
	write := func(value string) {
		t.Helper()
		path := filepath.Join(input, "input")
		if err := os.WriteFile(path, []byte(value), 0640); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(0, 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	build := func(name string, publish bool) (Result, string) {
		t.Helper()
		lane := filepath.Join(root, name)
		layout := filepath.Join(lane, "layout")
		filesystems := []FilesystemOutput{{Type: "local", Path: filepath.Join(lane, "filesystem")}, {Type: "local", Path: filepath.Join(lane, "filesystem-copy")}}
		for _, filesystem := range filesystems {
			if _, err := os.Stat(filesystem.Path); !os.IsNotExist(err) {
				t.Fatalf("%s filesystem destination already exists: %v", name, err)
			}
		}
		epoch := int64(0)
		options := SupervisedPlanOptions{
			Store: cacheTestStore(lane), ContextDir: input, Isolation: "rootless", Runtime: "crun",
			Output:    Output{Path: layout, Format: "oci", CompressionFormat: "gzip", Filesystem: filesystems[0], Filesystems: filesystems[1:]},
			Timestamp: &epoch, RewriteTimestamp: true, SignaturePolicyPath: policy, Stdout: io.Discard,
		}
		if publish {
			options.CacheTo = []CacheSpec{cache}
		} else {
			options.CacheFrom = []CacheSpec{cache}
		}
		var diagnostics bytes.Buffer
		options.Stderr = &diagnostics
		result, err := BuildPlanSupervised(ctx, plan, options)
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, diagnostics.String())
		}
		if result.CacheStats.Errors != 0 {
			t.Fatalf("%s cache errors: %+v\n%s", name, result.CacheStats, diagnostics.String())
		}
		proof := localComponentLastFile(t, layout, "proof")
		for _, filesystem := range filesystems {
			path := filepath.Join(filesystem.Path, "proof")
			data, err := os.ReadFile(path)
			if err != nil || string(data) != proof {
				t.Fatalf("%s captured filesystem proof differs from image: %q, %v", name, data, err)
			}
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0640 {
				t.Fatalf("%s captured filesystem proof mode: %v, %v", name, info, err)
			}
		}
		return result, layout
	}
	write("before\n")
	cold, coldLayout := build("cold", true)
	coldManifest, coldImage := readPlanImage(t, coldLayout)
	proof := localComponentLastFile(t, coldLayout, "proof")
	if !strings.HasPrefix(proof, "before\n") || !slices.Equal(coldImage.Config.Cmd, []string{"/proof"}) || !slices.Contains(coldImage.Config.Env, "CHANNEL=preview") || slices.Contains(coldImage.Config.Env, "BASE=build-only") {
		t.Fatalf("cold output lost final-stage state: config=%+v proof=%q", coldImage.Config, proof)
	}
	warm, warmLayout := build("warm", false)
	lease, err := acquireStore(cacheTestStore(filepath.Join(root, "warm")))
	if err != nil {
		t.Fatal(err)
	}
	images, imagesErr := lease.store.Images()
	containers, containersErr := lease.store.Containers()
	closeErr := lease.Close()
	if imagesErr != nil || containersErr != nil || closeErr != nil {
		t.Fatalf("inspect warm native store: images=%v containers=%v close=%v", imagesErr, containersErr, closeErr)
	}
	if len(images) != 1 || images[0].ID != warm.ImageID || len(containers) != 0 {
		t.Fatalf("terminal hit materialized base/ancestor images or leaked builders: images=%d containers=%d final=%s", len(images), len(containers), warm.ImageID)
	}
	warmManifest, warmImage := readPlanImage(t, warmLayout)
	if warm.CacheStats.Hits == 0 || warm.CacheStats.Misses != 0 || warm.ManifestDigest != cold.ManifestDigest || warmManifest.Config.Digest != coldManifest.Config.Digest || !reflect.DeepEqual(warmImage, coldImage) || localComponentLastFile(t, warmLayout, "proof") != proof {
		t.Fatalf("terminal cache hit changed exact output/config or reexecuted RUN: cold=%+v warm=%+v", cold, warm)
	}
	// Same size and restored source mtime must not hide a changed captured input.
	write("after!\n")
	changed, changedLayout := build("changed", false)
	if changed.CacheStats.Misses == 0 || changed.ManifestDigest == cold.ManifestDigest || !strings.HasPrefix(localComponentLastFile(t, changedLayout, "proof"), "after!\n") {
		t.Fatalf("changed input reused the terminal result: cold=%+v changed=%+v", cold, changed)
	}
}
