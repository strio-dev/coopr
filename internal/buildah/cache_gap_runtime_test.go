package buildah

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strio-dev/coopr/internal/buildcontext"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/planner"
	"go.podman.io/image/v5/types"
)

func TestCacheGapAggregateRunAuthorizationAndFreshStoreReuse(t *testing.T) {
	requireLiveInstructionCache(t)
	for _, test := range []struct{ name, properties, entitlement string }{
		{"host", `network="host"`, "network.host"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			root := t.TempDir()
			base, _ := newLiveBusyBoxRegistry(t, ctx)
			resolver := localConfigComponentResolver(t, ctx, root, `extend { run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/proof" `+test.properties+`; }`, false)
			policy := writeComponentTestPolicy(t, root)
			cache := []CacheSpec{{Transport: "oci-layout", Reference: filepath.Join(root, "cache")}}
			source := testPlan(t, fmt.Sprintf("from %q { component \"config\"; }", base))
			build := func(name string, allow bool) (Result, error) {
				dir := filepath.Join(root, name)
				options := SupervisedPlanOptions{Store: cacheTestStore(dir), ContextDir: root, Isolation: "rootless", Runtime: "crun", ComponentStoreDir: resolver.ComponentStoreDir(), TLSVerify: new(false), SignaturePolicyPath: policy, CacheFrom: cache, CacheTo: cache, Output: Output{Path: filepath.Join(dir, "layout")}, Stdout: io.Discard, Stderr: io.Discard}
				if allow {
					options.Allow = []string{test.entitlement}
				}
				return BuildPlanSupervised(ctx, source, options)
			}
			cold, err := build("cold", true)
			if err != nil {
				t.Fatal(err)
			}
			warm, err := build("warm", true)
			if err != nil {
				t.Fatal(err)
			}
			if cold.CacheStats.Stored < 2 || warm.CacheStats.Hits != 1 || warm.CacheStats.Misses != 0 {
				t.Fatalf("cold=%+v warm=%+v", cold.CacheStats, warm.CacheStats)
			}
			cm, _ := readPlanImage(t, cold.Layout)
			wm, _ := readPlanImage(t, warm.Layout)
			if cm.Layers[len(cm.Layers)-1].Digest != wm.Layers[len(wm.Layers)-1].Digest {
				t.Fatal("whole-result hit reran random producer")
			}
			if _, err := build("denied", false); err == nil || !strings.Contains(err.Error(), "--allow "+test.entitlement) {
				t.Fatalf("warm unauthorized result=%v", err)
			}
		})
	}
}

func TestCacheGapNamedContextWholeComponentReuse(t *testing.T) {
	f := newLocalComponentFixture(t)
	base, _ := newLiveBusyBoxRegistry(t, f.ctx)
	f.options.Resolver = localConfigComponentResolver(t, f.ctx, f.root, `extend { from "tools" { run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/proof" network="none"; } }`, false)
	resolver, err := oci.NewResolver(oci.Options{ComponentStoreDir: f.options.Resolver.ComponentStoreDir(), TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	f.options.Resolver = resolver
	f.options.BuildContexts = []buildcontext.Spec{{Name: "tools", Kind: buildcontext.DockerImage, Reference: base}}
	f.options.SystemContext.DockerInsecureSkipTLSVerify = types.OptionalBoolFalse
	source := parseWorkerDefinition(t, `from "scratch" { component "config"; }`)
	var first string
	for _, attempt := range []string{"cold", "warm"} {
		options := f.options
		options.Store = cacheTestStore(filepath.Join(f.root, attempt))
		options.Output = Output{Path: filepath.Join(f.root, attempt, "layout")}
		plan, err := planner.Create(source, planner.Options{Mode: planner.Build, BuildContexts: options.BuildContexts})
		if err != nil {
			t.Fatal(err)
		}
		result, err := BuildPlan(f.ctx, plan, options)
		if err != nil {
			t.Fatal(err)
		}
		proof := localComponentLastFile(t, result.Layout, "proof")
		if attempt == "cold" {
			first = proof
			if result.CacheStats.Stored < 2 {
				t.Fatalf("cold=%+v", result.CacheStats)
			}
		} else if result.CacheStats.Hits != 1 || result.CacheStats.Misses != 0 || proof != first {
			t.Fatalf("warm=%+v proof=%q first=%q", result.CacheStats, proof, first)
		}
	}
}

func TestCacheGapDynamicNestedAndDeferredGraphsReuseInstructions(t *testing.T) {
	for _, mode := range []string{"nested", "dynamic", "deferred"} {
		t.Run(mode, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			base, _ := newLiveBusyBoxRegistry(t, f.ctx)
			resolver, err := oci.NewResolver(oci.Options{ComponentStoreDir: f.options.Resolver.ComponentStoreDir(), TLSVerify: new(false)})
			if err != nil {
				t.Fatal(err)
			}
			f.options.Resolver = resolver
			f.options.SystemContext.DockerInsecureSkipTLSVerify = types.OptionalBoolFalse
			f.write(t, "inner.coopr", `extend { env returned="yes"; }`)
			source := `extend { component "./inner.coopr"; run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/proof" network="none"; }`
			if mode == "dynamic" {
				source = `extend as="initial" { component "./inner.coopr"; }
extend { from "initial" { run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/proof" network="none"; } }`
			}
			if mode == "deferred" {
				componentSourceLayout(t, f)
				source = `extend as="initial" { run "cp -a /ctx/input /ctx/out" network="none" { mount "bind" target="/ctx" rw="true"; } }
extend { from "oci:out:latest" after="initial" { run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/proof" network="none"; } }`
			}
			f.write(t, "outer.coopr", source)
			var proof string
			for _, attempt := range []string{"cold", "warm"} {
				f.options.Store = cacheTestStore(filepath.Join(f.root, attempt, "image-store"))
				result, layout := f.build(t, attempt, fmt.Sprintf("from %q { component \"./outer.coopr\"; }", base))
				got := localComponentLastFile(t, layout, "proof")
				if attempt == "cold" {
					proof = got
				} else if got != proof || (result.CacheStats.Hits == 0) {
					t.Fatalf("%s did not reuse instruction cache: %+v proof=%q cold=%q", mode, result.CacheStats, got, proof)
				}
			}
			if mode != "deferred" {
				f.write(t, "inner.coopr", `extend { env returned="changed"; }`)
				f.options.Store = cacheTestStore(filepath.Join(f.root, "changed", "image-store"))
				_, layout := f.build(t, "changed", fmt.Sprintf("from %q { component \"./outer.coopr\"; }", base))
				_, image := readPlanImage(t, layout)
				if !strings.Contains("\n"+strings.Join(image.Config.Env, "\n")+"\n", "\nreturned=changed\n") {
					t.Fatalf("nested selection was skipped: %v", image.Config.Env)
				}
			}
		})
	}
}

func TestCacheGapPackageAuthorizationBeforeFreshStoreReuse(t *testing.T) {
	requireLiveInstructionCache(t)
	for _, test := range []struct{ name, properties, entitlement string }{
		{"host", `network="host"`, "network.host"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			root := t.TempDir()
			base, host := newLiveBusyBoxRegistry(t, ctx)
			resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false), ComponentStoreDir: filepath.Join(root, "components")})
			if err != nil {
				t.Fatal(err)
			}
			plan := testPublicationPlan(t, `from "`+base+`" as="producer" { run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/payload" `+test.properties+`; }
package as="payload" { copy "/payload" "/payload" from="producer"; }
extend { copy "/payload" "/payload" from="payload"; }`)
			policy := writeComponentTestPolicy(t, root)
			build := func(name string, allow bool) (oci.Package, StoreOptions, error) {
				store := cacheTestStore(filepath.Join(root, name))
				options := PublicationOptions{PlanOptions: PlanOptions{Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Resolver: resolver, CacheRepository: host + "/coopr/package-auth-cache", SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root}}, PackagePaths: map[string]string{"payload": filepath.Join(root, name, "payload.tar")}}
				if allow {
					options.Allow = []string{test.entitlement}
				}
				packages, err := PublishPlan(ctx, plan, options)
				return packages["payload"], store, err
			}
			cold, _, err := build("cold", true)
			if err != nil {
				t.Fatal(err)
			}
			warm, store, err := build("warm", true)
			if err != nil {
				t.Fatal(err)
			}
			if cold.Descriptor.Digest != warm.Descriptor.Digest {
				t.Fatal("package producer ran again on warm lookup")
			}
			lease, err := acquireStore(store)
			if err != nil {
				t.Fatal(err)
			}
			images, imageErr := lease.store.Images()
			closeErr := lease.Close()
			if imageErr != nil || closeErr != nil {
				t.Fatalf("inspect warm store: %v / %v", imageErr, closeErr)
			}
			if len(images) != 0 {
				t.Fatalf("package cache hit executed into fresh store: %d images", len(images))
			}
			if _, _, err := build("denied", false); err == nil || !strings.Contains(err.Error(), "--allow "+test.entitlement) {
				t.Fatalf("unauthorized warm package: %v", err)
			}
		})
	}
}

func TestCacheGapNamedContextContentInvalidation(t *testing.T) {
	for _, source := range []string{
		`extend { from "assets" { env selected="yes"; } }`,
		`extend { copy "/payload" "/payload" from="assets"; }`,
	} {
		t.Run(source, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.options.Resolver = localConfigComponentResolver(t, f.ctx, f.root, source, false)
			f.options.BuildContexts = []buildcontext.Spec{{Name: "assets", Kind: buildcontext.Local, Path: filepath.Join(f.contextDir, "assets")}}
			for _, attempt := range []struct {
				name, value string
				hit         bool
			}{
				{"cold", "one", false}, {"warm", "one", true}, {"changed", "two", false}, {"changed-warm", "two", true},
			} {
				if !attempt.hit {
					f.write(t, "assets/payload", attempt.value)
				}
				options := f.options
				options.Store = cacheTestStore(filepath.Join(f.root, attempt.name, "image-store"))
				options.Output = Output{Path: filepath.Join(f.root, attempt.name, "layout")}
				definition := parseWorkerDefinition(t, `from "scratch" { component "config"; }`)
				plan, err := planner.Create(definition, planner.Options{Mode: planner.Build, BuildContexts: options.BuildContexts})
				if err != nil {
					t.Fatal(err)
				}
				result, err := BuildPlan(f.ctx, plan, options)
				if err != nil {
					t.Fatal(err)
				}
				if got := localComponentLastFile(t, result.Layout, "payload"); got != attempt.value {
					t.Fatalf("%s payload=%q", attempt.name, got)
				}
				if attempt.hit {
					if result.CacheStats.Hits != 1 || result.CacheStats.Misses != 0 {
						t.Fatalf("%s wanted whole hit: %+v", attempt.name, result.CacheStats)
					}
				} else if result.CacheStats.Misses == 0 {
					t.Fatalf("%s reused stale context: %+v", attempt.name, result.CacheStats)
				}
			}
		})
	}
}

func TestCacheGapNestedWarmCacheStillChecksAuthorization(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	f.write(t, "inner.coopr", `extend { run "printf authorized >/proof" network="host"; }`)
	f.write(t, "outer.coopr", `extend { component "./inner.coopr"; }`)
	source := fmt.Sprintf("from %q { component \"./outer.coopr\"; }", base.reference)
	f.options.Allow = []string{"network.host"}
	for _, attempt := range []string{"cold", "warm"} {
		if _, layout := f.build(t, attempt, source); localComponentLastFile(t, layout, "proof") != "authorized" {
			t.Fatal("nested producer proof missing")
		}
	}
	f.options.Allow = nil
	f.options.Output = Output{Path: filepath.Join(f.root, "denied")}
	if _, err := BuildPlan(f.ctx, testPlan(t, source), f.options); err == nil || !strings.Contains(err.Error(), "--allow network.host") {
		t.Fatalf("nested warm authorization=%v", err)
	}
}

func TestCacheGapUnpinnedHTTPAddPortableReuseAndInvalidation(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	var payload atomic.Value
	payload.Store("one")
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Last-Modified", "Tue, 02 Jan 2024 03:04:05 GMT")
		_, _ = io.WriteString(w, payload.Load().(string))
	}))
	t.Cleanup(server.Close)
	for _, attempt := range []struct {
		name, value string
		hit         bool
	}{{"cold", "one", false}, {"warm", "one", true}, {"changed", "two", false}, {"changed-warm", "two", true}} {
		payload.Store(attempt.value)
		before := requests.Load()
		dir := filepath.Join(root, attempt.name)
		result, err := BuildPlanSupervised(ctx, testPlan(t, fmt.Sprintf("from \"scratch\" { add %q \"/proof\"; }", server.URL+"/payload")), SupervisedPlanOptions{Store: cacheTestStore(dir), ContextDir: root, Isolation: "rootless", Output: Output{Path: filepath.Join(dir, "layout")}, CacheLocalDir: filepath.Join(root, "cache"), Stdout: io.Discard, Stderr: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		if localComponentLastFile(t, result.Layout, "proof") != attempt.value {
			t.Fatal("remote ADD reused stale content")
		}
		if requests.Load() <= before {
			t.Fatal("unpinned remote input was not revalidated")
		}
		if attempt.hit {
			if result.CacheStats.Hits != 1 || result.CacheStats.Misses != 0 {
				t.Fatalf("%s portable ADD did not hit: %+v", attempt.name, result.CacheStats)
			}
		} else if result.CacheStats.Misses != 1 {
			t.Fatalf("%s portable ADD did not miss: %+v", attempt.name, result.CacheStats)
		}
	}
}

func TestCacheGapUnpinnedGitAddPortableReuseAndInvalidation(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	working := filepath.Join(root, "working")
	repository := filepath.Join(root, "repository.git")
	runGit(t, "init", "-b", "main", working)
	runGit(t, "-C", working, "config", "user.name", "Coopr Test")
	runGit(t, "-C", working, "config", "user.email", "coopr@example.invalid")
	commit := func(value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(working, "proof"), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, "-C", working, "add", "proof")
		runGit(t, "-C", working, "commit", "-m", value)
	}
	commit("one")
	runGit(t, "clone", "--bare", working, repository)
	backend, err := exec.LookPath("git-http-backend")
	if err != nil {
		t.Fatal(err)
	}
	handler := &cgi.Handler{Path: backend, Root: "/", Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); handler.ServeHTTP(w, r) }))
	t.Cleanup(server.Close)
	for _, attempt := range []struct {
		name, value string
		hit         bool
	}{{"cold", "one", false}, {"warm", "one", true}, {"changed", "two", false}, {"changed-warm", "two", true}} {
		if attempt.name == "changed" {
			commit("two")
			runGit(t, "-C", repository, "fetch", working, "main:main")
		}
		before := requests.Load()
		dir := filepath.Join(root, attempt.name)
		result, err := BuildPlanSupervised(ctx, testPlan(t, fmt.Sprintf("from \"scratch\" { add %q \"/source/\"; }", server.URL+"/repository.git#main")), SupervisedPlanOptions{Store: cacheTestStore(dir), ContextDir: root, Isolation: "rootless", Output: Output{Path: filepath.Join(dir, "layout")}, CacheLocalDir: filepath.Join(root, "cache"), Stdout: io.Discard, Stderr: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		if localComponentLastFile(t, result.Layout, "source/proof") != attempt.value {
			t.Fatal("Git ADD reused stale branch content")
		}
		if requests.Load() <= before {
			t.Fatal("Git branch was not resolved again")
		}
		if attempt.hit {
			if result.CacheStats.Hits != 1 || result.CacheStats.Misses != 0 {
				t.Fatalf("%s portable Git ADD did not hit: %+v", attempt.name, result.CacheStats)
			}
		} else if result.CacheStats.Misses != 1 {
			t.Fatalf("%s portable Git ADD did not miss: %+v", attempt.name, result.CacheStats)
		}
	}
}

func TestCacheGapDynamicNestedAndDeferredPackageProducersReuseInstructions(t *testing.T) {
	for _, mode := range []string{"nested", "dynamic", "deferred"} {
		t.Run(mode, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			base, _ := newLiveBusyBoxRegistry(t, f.ctx)
			resolver, err := oci.NewResolver(oci.Options{ComponentStoreDir: f.options.Resolver.ComponentStoreDir(), TLSVerify: new(false)})
			if err != nil {
				t.Fatal(err)
			}
			f.options.Resolver = resolver
			f.options.SystemContext.DockerInsecureSkipTLSVerify = types.OptionalBoolFalse
			f.write(t, "inner.coopr", `extend { env returned="yes"; }`)
			command := `run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/proof; printf '%s' \"$returned\" >/revision" network="none";`
			producer := fmt.Sprintf(`from %q as="producer" { component "./inner.coopr"; %s }`, base, command)
			if mode == "dynamic" {
				producer = fmt.Sprintf(`from %q as="initial" { component "./inner.coopr"; }
from "initial" as="producer" { %s }`, base, command)
			}
			if mode == "deferred" {
				componentSourceLayout(t, f)
				producer = fmt.Sprintf(`from %q as="initial" { run "cp -a /ctx/input /ctx/out" network="none" { mount "bind" target="/ctx" rw="true"; } }
from "oci:out:latest" as="producer" after="initial" { %s }`, base, command)
			}
			f.write(t, "outer.coopr", producer+`
package as="assets" { copy "/proof" "/revision" "/" from="producer"; }
extend { copy "/proof" "/revision" "/" from="assets"; }`)
			var first string
			for _, attempt := range []string{"cold", "warm"} {
				f.options.Store = cacheTestStore(filepath.Join(f.root, attempt, "image-store"))
				result, layout := f.build(t, attempt, `from "scratch" { component "./outer.coopr"; }`)
				proof := localComponentLastFile(t, layout, "proof")
				if attempt == "cold" {
					first = proof
				} else if proof != first || result.CacheStats.Hits == 0 {
					t.Fatalf("package producer reran uncached: %+v cold=%q warm=%q", result.CacheStats, first, proof)
				}
			}
			if mode != "deferred" {
				f.write(t, "inner.coopr", `extend { env returned="changed"; }`)
				f.options.Store = cacheTestStore(filepath.Join(f.root, "changed", "image-store"))
				_, layout := f.build(t, "changed", `from "scratch" { component "./outer.coopr"; }`)
				if revision := localComponentLastFile(t, layout, "revision"); revision != "changed" {
					t.Fatalf("package nested component change not resolved: %q", revision)
				}
				if proof := localComponentLastFile(t, layout, "proof"); proof == first {
					t.Fatal("changed producer execution configuration reused old instruction")
				}
			}
		})
	}
}
