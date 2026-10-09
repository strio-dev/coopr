package buildah

import (
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"coopr/internal/planner"
)

func TestLocalComponentTypedOptionsBuildAndCache(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	f.write(t, "payload", "schema\n")
	f.write(t, "components/shared.coopr", `extend
arg "kind" "cache"
arg "permissions" "0644"
arg "linked" #true
run "printf cached >/cache/value; cat /cache/value >/proof" {
    mount "$kind" id="schema-options" target="/cache"
}
copy "payload" "/payload" chmod="${permissions}" link="${linked}"
`)
	for _, test := range []struct {
		name, permission string
		mode             int64
		cacheHits        int
	}{
		{name: "cold", permission: "0644", mode: 0o644},
		{name: "warm", permission: "0644", mode: 0o644, cacheHits: 2},
		{name: "changed", permission: "0600", mode: 0o600},
	} {
		t.Run(test.name, func(t *testing.T) {
			layout := filepath.Join(f.root, test.name)
			var logs strings.Builder
			_, err := BuildPlanSupervised(f.ctx, testPlan(t, fmt.Sprintf("from %q\ncomponent \"./components/shared.coopr\" permissions=%q\n", base.reference, test.permission)), SupervisedPlanOptions{
				Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun",
				Output: Output{Path: layout}, ComponentStoreDir: filepath.Join(f.root, "components"),
				CacheLocalDir: f.options.CacheLocalDir, SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath,
				Stdout: io.Discard, Stderr: &logs,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(logs.String(), "--> Using cache "); got != test.cacheHits {
				t.Fatalf("cache hits = %d, want %d:\n%s", got, test.cacheHits, logs.String())
			}
			manifest, _ := readPlanImage(t, layout)
			blob := filepath.Join(layout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded())
			if got := readLayerHeader(t, blob, "payload").Mode & 0o777; got != test.mode {
				t.Errorf("payload mode = %#o, want %#o", got, test.mode)
			}
			if got := readLayerFile(t, blob, "payload"); got != "schema\n" {
				t.Errorf("payload = %q", got)
			}
		})
	}
}

func TestOnBuildMountTypedOptionsExecuteInChildScope(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	f.write(t, "token", "child-secret\n")
	f.write(t, "global-input", "build-wide\n")
	f.options.Secrets = []string{"id=token,src=" + filepath.Join(f.contextDir, "token")}
	mounts, err := ParseTransientRunMounts([]string{"type=bind,source=global-input,target=/run/global-input"})
	if err != nil {
		t.Fatal(err)
	}
	def := parseWorkerDefinition(t, fmt.Sprintf(`from %q as="parent"
onbuild { arg "required" #true }
onbuild { arg "permissions" "0400" }
onbuild {
    run "test \"$(/bin/busybox stat -c %%a /run/token)\" = 400; cat /run/token /run/global-input >/proof" {
        mount "secret" id="token" target="/run/token" required="${required}" mode="${permissions}"
    }
}

from "parent"
`, base.reference))
	layout := filepath.Join(f.root, "inherited")
	_, err = BuildDefinitionSupervised(f.ctx, def, planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{
		Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun",
		Output: Output{Path: layout, Format: outputFormatDocker}, Secrets: f.options.Secrets, TransientRunMounts: mounts,
		SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := localComponentLastFile(t, layout, "proof"); got != "child-secret\nbuild-wide\n" {
		t.Fatalf("inherited secret result = %q", got)
	}
}

func TestLocalComponentCacheTracksBuildWideMountOptions(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	f.write(t, "components/mounted.coopr", `extend
run "if test -d /first; then printf first >/proof; else test -d /second && printf second >/proof; fi"
`)
	plan := testPlan(t, fmt.Sprintf("from %q\ncomponent \"./components/mounted.coopr\"\n", base.reference))
	for _, test := range []struct {
		name, target, proof string
		cacheHit            bool
	}{
		{name: "first cold", target: "/first", proof: "first"},
		{name: "first warm", target: "/first", proof: "first", cacheHit: true},
		{name: "changed cold", target: "/second", proof: "second"},
		{name: "changed warm", target: "/second", proof: "second", cacheHit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			mounts, err := ParseTransientRunMounts([]string{"type=tmpfs,target=" + test.target})
			if err != nil {
				t.Fatal(err)
			}
			layout := filepath.Join(f.root, test.name)
			var logs strings.Builder
			_, err = BuildPlanSupervised(f.ctx, plan, SupervisedPlanOptions{
				Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun",
				Output: Output{Path: layout}, ComponentStoreDir: filepath.Join(f.root, "components"),
				CacheLocalDir: f.options.CacheLocalDir, SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath,
				TransientRunMounts: mounts, Stdout: io.Discard, Stderr: &logs,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := localComponentLastFile(t, layout, "proof"); got != test.proof {
				t.Fatalf("mounted RUN proof = %q, want %q", got, test.proof)
			}
			if got := strings.Contains(logs.String(), "--> Using component cache "); got != test.cacheHit {
				t.Fatalf("component cache hit = %t, want %t:\n%s", got, test.cacheHit, logs.String())
			}
		})
	}
}
