package buildah

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/moby/buildkit/util/system"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/planner"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
	"go.podman.io/storage"
)

func TestStageEntryDefaultsMatchNativeBuildah(t *testing.T) {
	f := newLocalComponentFixture(t)
	_, layout := f.build(t, "scratch", `from "scratch" { env observed="$PATH"; }`)
	_, got := readPlanImage(t, layout)
	lease, err := acquireStore(f.options.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}()
	file := filepath.Join(f.contextDir, "Containerfile")
	if err := os.WriteFile(file, []byte("FROM scratch\nENV observed=$PATH\n"), 0600); err != nil {
		t.Fatal(err)
	}
	nativeLayout := filepath.Join(f.root, "native")
	_, _, err = imagebuildah.BuildDockerfiles(f.ctx, lease.store, define.BuildOptions{ContextDirectory: f.contextDir, Output: "oci:" + nativeLayout, Layers: true, PullPolicy: define.PullNever, Isolation: define.IsolationOCIRootless, Runtime: "crun", CommonBuildOpts: &define.CommonBuildOptions{}, Compression: define.Uncompressed, Out: io.Discard, Err: os.Stderr, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true}, file)
	if err != nil {
		t.Fatal(err)
	}
	_, native := readPlanImage(t, nativeLayout)
	for _, key := range []string{"PATH", "observed"} {
		var actual, expected string
		for _, entry := range got.Config.Env {
			if len(entry) > len(key) && entry[:len(key)+1] == key+"=" {
				actual = entry[len(key)+1:]
			}
		}
		for _, entry := range native.Config.Env {
			if len(entry) > len(key) && entry[:len(key)+1] == key+"=" {
				expected = entry[len(key)+1:]
			}
		}
		if actual != expected || actual != system.DefaultPathEnvUnix {
			t.Fatalf("%s Coopr=%q native=%q", key, actual, expected)
		}
	}
	if got.Config.WorkingDir != native.Config.WorkingDir || got.Config.WorkingDir != "/" {
		t.Fatalf("working directory Coopr=%q native=%q", got.Config.WorkingDir, native.Config.WorkingDir)
	}
}

func TestStageEntryDefaultsPackageAndComponentReturn(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "payload", "payload")
	f.write(t, "shared.coopr", `package as="assets" {
 env packaged="$PATH"
 copy "payload" "/payload"
}
extend {
 from "assets" { env PATH=""; }
}
`)
	for _, attempt := range []string{"cold", "warm", "fresh-store"} {
		if attempt == "fresh-store" {
			f.options.Store = cacheTestStore(filepath.Join(f.root, "fresh-store"))
		}
		result, layout := f.build(t, attempt, `from "scratch" {
 component "./shared.coopr"
 env observed="$PATH"
}
`)
		if result.CacheStats.Errors != 0 || attempt != "cold" && result.CacheStats.Hits == 0 {
			t.Fatalf("%s component cache stats: %+v", attempt, result.CacheStats)
		}
		_, image := readPlanImage(t, layout)
		values := map[string]string{}
		for _, entry := range image.Config.Env {
			for i := range entry {
				if entry[i] == '=' {
					values[entry[:i]] = entry[i+1:]
					break
				}
			}
		}
		if values["packaged"] != system.DefaultPathEnvUnix || values["PATH"] != "" || values["observed"] != "" || image.Config.WorkingDir != "/" {
			t.Fatalf("%s config: %+v", attempt, image.Config)
		}
	}
}

func TestStageEntryDefaultsOutputUnsetStaysAbsent(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.options.ImageControls.UnsetEnv = []string{"PATH"}
	_, layout := f.build(t, "unset", `from "scratch" { env observed="$PATH"; }`)
	_, image := readPlanImage(t, layout)
	for _, entry := range image.Config.Env {
		if strings.HasPrefix(entry, "PATH=") {
			t.Fatalf("output unset PATH restored: %v", image.Config.Env)
		}
	}
	if !slices.Contains(image.Config.Env, "observed="+system.DefaultPathEnvUnix) {
		t.Fatalf("stage did not see initial PATH: %v", image.Config.Env)
	}
}

func TestStageEntryDefaultsExternalMissingPathNativeDifferential(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "payload", "payload")
	f.options.ImageControls.UnsetEnv = []string{"PATH"}
	base, _ := f.build(t, "base", `from "scratch" { copy "payload" "/payload"; }`)
	const reference = "localhost/coopr-defaults-base:test"
	if err := WithStore(f.options.Store, func(store storage.Store) error { return store.AddNames(base.ImageID, []string{reference}) }); err != nil {
		t.Fatal(err)
	}

	for _, metadata := range []bool{false, true} {
		t.Run(fmt.Sprintf("metadata=%t", metadata), func(t *testing.T) {
			name := fmt.Sprintf("external-%t", metadata)
			body, tail := "", ""
			if metadata {
				body = `env observed="$PATH";`
				tail = "ENV observed=$PATH\n"
			}
			def, err := definition.Parse(strings.NewReader(fmt.Sprintf(`from %q { %s }`, reference, body)))
			if err != nil {
				t.Fatal(err)
			}
			result, err := BuildDefinitionSupervised(f.ctx, def, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH}, SupervisedPlanOptions{Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun", Output: Output{Path: filepath.Join(f.root, name)}, SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath, Stdout: io.Discard, Stderr: io.Discard})
			if err != nil {
				t.Fatal(err)
			}
			_, got := readPlanImage(t, result.Layout)
			file := filepath.Join(f.contextDir, "Containerfile")
			if err := os.WriteFile(file, []byte("FROM "+reference+"\n"+tail), 0600); err != nil {
				t.Fatal(err)
			}
			lease, err := acquireStore(f.options.Store)
			if err != nil {
				t.Fatal(err)
			}
			nativeID, _, buildErr := imagebuildah.BuildDockerfiles(f.ctx, lease.store, define.BuildOptions{ContextDirectory: f.contextDir, Layers: true, PullPolicy: define.PullNever, Isolation: define.IsolationOCIRootless, Runtime: "crun", CommonBuildOpts: &define.CommonBuildOptions{}, Compression: define.Uncompressed, Out: io.Discard, Err: os.Stderr, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true}, file)
			if buildErr != nil {
				_ = lease.Close()
				t.Fatal(buildErr)
			}
			nativeResult, exportErr := exportStoredImageVariantRaw(f.ctx, lease.store, nativeID, Output{Path: filepath.Join(f.root, name+"-native")}, nil, nil)
			closeErr := lease.Close()
			if exportErr != nil || closeErr != nil {
				t.Fatalf("export=%v close=%v", exportErr, closeErr)
			}
			_, native := readPlanImage(t, nativeResult.Layout)
			if !slices.Equal(got.Config.Env, native.Config.Env) || got.Config.WorkingDir != native.Config.WorkingDir {
				t.Fatalf("Coopr=%+v native=%+v", got.Config, native.Config)
			}
			if !metadata && (result.ImageID != base.ImageID || nativeID != base.ImageID) {
				t.Fatalf("FROM-only changed base: Coopr=%s native=%s base=%s", result.ImageID, nativeID, base.ImageID)
			}
		})
	}
}

func TestStageEntryDefaultsComponentReturnMissingPath(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "payload", "payload")
	f.options.ImageControls.UnsetEnv = []string{"PATH"}
	base, _ := f.build(t, "base", `from "scratch" { copy "payload" "/payload"; }`)
	f.options.ImageControls.UnsetEnv = nil
	const reference = "localhost/coopr-defaults-return:test"
	if err := WithStore(f.options.Store, func(store storage.Store) error { return store.AddNames(base.ImageID, []string{reference}) }); err != nil {
		t.Fatal(err)
	}
	f.write(t, "shared.coopr", `extend { from "`+reference+`" {}; }`)
	for _, attempt := range []string{"cold", "warm"} {
		_, layout := f.build(t, attempt, `from "scratch" { component "./shared.coopr"; env observed="$PATH"; }`)
		_, image := readPlanImage(t, layout)
		for _, entry := range image.Config.Env {
			if strings.HasPrefix(entry, "PATH=") {
				t.Fatalf("%s returned missing PATH was reseeded: %v", attempt, image.Config.Env)
			}
		}
		if !slices.Contains(image.Config.Env, "observed=") {
			t.Fatalf("%s post-return expansion: %v", attempt, image.Config.Env)
		}
	}
}

func TestStageEntryDefaultsCallerEnvironmentOrderCaches(t *testing.T) {
	for _, placement := range []string{"none", "before-env", "after-env"} {
		t.Run(placement, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.write(t, "noop.coopr", "extend {}\n")
			f.write(t, "payload", "payload")
			before, after := "", ""
			switch placement {
			case "before-env":
				// Isolate caller ENV replay from wall-clock metadata history.
				epoch := int64(0)
				f.options.Timestamp = &epoch
				before = `copy "payload" "/payload";`
			case "after-env":
				after = `copy "payload" "/payload";`
			}
			source := `from "scratch" { ` + before + ` env A="old"; env B="keep"; env A="new"; ` + after + ` component "./noop.coopr"; }`
			for _, attempt := range []string{"cold", "warm"} {
				result, layout := f.build(t, attempt, source)
				if result.CacheStats.Errors != 0 || attempt == "warm" && result.CacheStats.Hits == 0 {
					t.Fatalf("%s cache stats: %+v", attempt, result.CacheStats)
				}
				_, image := readPlanImage(t, layout)
				if !slices.Equal(image.Config.Env, []string{"PATH=" + system.DefaultPathEnvUnix, "A=new", "B=keep"}) {
					t.Fatalf("%s ENV order: %v", attempt, image.Config.Env)
				}
			}
		})
	}
}
