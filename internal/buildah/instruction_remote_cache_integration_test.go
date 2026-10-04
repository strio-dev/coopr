package buildah

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	"go.podman.io/image/v5/types"
)

func TestBuildPlanReusesExactInstructionImageFromRegistryAcrossStores(t *testing.T) {
	requireLiveInstructionCache(t)
	for _, format := range []string{"oci", "docker"} {
		for _, stages := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/%d-stages", format, stages), func(t *testing.T) {
				testExactInstructionImageFromRegistryAcrossStores(t, format, stages)
			})
		}
	}
}

func testExactInstructionImageFromRegistryAcrossStores(t *testing.T, format string, stages int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	base, host := newLiveBusyBoxRegistry(t, ctx)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	definition := "from \"" + base + "\" as=\"producer\"\nrun \"od -An -N16 -tx1 /dev/urandom | tr -d ' \\\\n' >/proof\" network=\"none\"\n"
	if stages == 2 {
		definition += "from \"producer\"\nrun \"cat /proof >/forwarded\" network=\"none\"\n"
	}
	plan := testPlan(t, definition)
	localCache := filepath.Join(root, "local-cache")
	build := func(name, cacheLocalDir, cacheRepository string) Result {
		t.Helper()
		baseDir := filepath.Join(root, name)
		result, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store:      StoreOptions{RunRoot: filepath.Join(baseDir, "run"), GraphRoot: filepath.Join(baseDir, "graph"), GraphDriverName: "vfs"},
			ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: Output{Path: filepath.Join(baseDir, "layout"), Format: format},
			ImageStoreDir: filepath.Join(baseDir, "images"), CacheLocalDir: cacheLocalDir, CacheRepository: cacheRepository,
			PlainHTTPRegistries: []string{host}, SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	cold := build("cold", "", host+"/coopr/cache")
	warm := build("warm", localCache, host+"/coopr/cache")
	if cold.CacheStats.Misses != stages || cold.CacheStats.Stored != stages || cold.CacheStats.Hits != 0 {
		t.Fatalf("cold instruction cache stats = %+v", cold.CacheStats)
	}
	if warm.CacheStats.Hits != stages || warm.CacheStats.Misses != 0 || warm.CacheStats.Stored != stages {
		t.Fatalf("warm instruction cache stats = %+v", warm.CacheStats)
	}
	if warm.ManifestDigest != cold.ManifestDigest {
		coldConfig, coldErr := oci.ReadImageConfigLayout(ctx, cold.Layout)
		warmConfig, warmErr := oci.ReadImageConfigLayout(ctx, warm.Layout)
		t.Fatalf("restored manifest = %s, want exact %s; cold config (%v)=%s; warm config (%v)=%s", warm.ManifestDigest, cold.ManifestDigest, coldErr, coldConfig, warmErr, warmConfig)
	}
	warmStore := StoreOptions{RunRoot: filepath.Join(root, "warm", "run"), GraphRoot: filepath.Join(root, "warm", "graph"), GraphDriverName: "vfs"}
	if count := instructionCacheRecordCount(t, warmStore); count != stages {
		t.Fatalf("remote hit seeded %d local instruction records, want %d", count, stages)
	}
	offline := build("offline", localCache, "")
	if offline.CacheStats.Hits != stages || offline.CacheStats.Misses != 0 {
		t.Fatalf("local seeded instruction cache stats = %+v", offline.CacheStats)
	}
	if offline.ManifestDigest != cold.ManifestDigest {
		t.Fatalf("local restored manifest = %s, want exact %s", offline.ManifestDigest, cold.ManifestDigest)
	}
}

func TestBuildPlanReusesDeclaredInputInstructionsFromRegistryAcrossStores(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	base, host := newLiveBusyBoxRegistry(t, ctx)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	build := func(name, repository string, planText string, configure ...func(*SupervisedPlanOptions)) Result {
		t.Helper()
		baseDir := filepath.Join(root, name)
		options := SupervisedPlanOptions{
			Store: StoreOptions{
				RunRoot: filepath.Join(baseDir, "run"), GraphRoot: filepath.Join(baseDir, "graph"), GraphDriverName: "vfs",
			},
			ContextDir: contextDir, Isolation: "rootless", Runtime: "crun", Output: Output{Path: filepath.Join(baseDir, "layout")},
			ImageStoreDir: filepath.Join(baseDir, "images"), CacheRepository: host + "/coopr/" + repository,
			PlainHTTPRegistries: []string{host}, SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		}
		for _, apply := range configure {
			apply(&options)
		}
		result, err := BuildPlanSupervised(ctx, testPlan(t, planText), options)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	t.Run("default network RUN", func(t *testing.T) {
		planText := "from \"" + base + "\"\nrun \"od -An -N16 -tx1 /dev/urandom | tr -d ' \\\\n' >/proof\"\n"
		cold := build("network-cold", "network", planText)
		warm := build("network-warm", "network", planText)
		if cold.CacheStats.Misses != 1 || cold.CacheStats.Stored != 1 || warm.CacheStats.Hits != 1 {
			t.Fatalf("default-network portable cache stats: cold=%+v warm=%+v", cold.CacheStats, warm.CacheStats)
		}
		if warm.ManifestDigest != cold.ManifestDigest {
			t.Fatalf("default-network restored manifest = %s, want %s", warm.ManifestDigest, cold.ManifestDigest)
		}
	})

	t.Run("cache mount RUN", func(t *testing.T) {
		planText := "from \"" + base + "\"\nrun \"od -An -N16 -tx1 /dev/urandom | tr -d ' \\\\n' >/proof; touch /cache/used\" network=\"none\" {\n  mount \"cache\" target=\"/cache\" id=\"portable-compile\"\n}\n"
		cold := build("cache-mount-cold", "cache-mount", planText)
		warm := build("cache-mount-warm", "cache-mount", planText)
		if cold.CacheStats.Misses != 1 || cold.CacheStats.Stored != 1 || warm.CacheStats.Hits != 1 || warm.ManifestDigest != cold.ManifestDigest {
			t.Fatalf("cache-mount portable result: cold=%s %+v warm=%s %+v", cold.ManifestDigest, cold.CacheStats, warm.ManifestDigest, warm.CacheStats)
		}
	})

	t.Run("secret mount RUN", func(t *testing.T) {
		planText := "from \"" + base + "\"\nrun \"cat /run/secrets/token >/proof\" network=\"none\" { mount \"secret\" id=\"token\" required=\"true\" }\n"
		secret := func(name, value string) func(*SupervisedPlanOptions) {
			path := filepath.Join(root, name+"-secret")
			if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
			return func(options *SupervisedPlanOptions) { options.Secrets = []string{"id=token,src=" + path} }
		}
		cold := build("secret-cold", "secret", planText, secret("cold", "first\n"))
		warm := build("secret-warm", "secret", planText, secret("warm", "second\n"))
		if cold.CacheStats.Misses != 1 || cold.CacheStats.Stored != 1 || warm.CacheStats.Hits != 1 || warm.ManifestDigest != cold.ManifestDigest {
			t.Fatalf("secret portable result: cold=%s %+v warm=%s %+v", cold.ManifestDigest, cold.CacheStats, warm.ManifestDigest, warm.CacheStats)
		}
	})

	t.Run("SSH mount RUN", func(t *testing.T) {
		planText := "from \"" + base + "\"\nrun \"test -S /run/buildkit/ssh_agent.0; od -An -N16 -tx1 /dev/urandom | tr -d ' \\\\n' >/proof\" network=\"none\" { mount \"ssh\" id=\"default\" required=\"true\" }\n"
		withAgent := func(name string) func(*SupervisedPlanOptions) {
			dir, err := os.MkdirTemp("", "coopr-"+name+"-*")
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(dir, "agent.sock"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close(); _ = os.RemoveAll(dir) })
			return func(options *SupervisedPlanOptions) { options.SSH = []string{"default=" + listener.Addr().String()} }
		}
		cold := build("ssh-cold", "ssh", planText, withAgent("cold-agent"))
		warm := build("ssh-warm", "ssh", planText, withAgent("warm-agent"))
		if cold.CacheStats.Misses != 1 || cold.CacheStats.Stored != 1 || warm.CacheStats.Hits != 1 || warm.ManifestDigest != cold.ManifestDigest {
			t.Fatalf("SSH portable result: cold=%s %+v warm=%s %+v", cold.ManifestDigest, cold.CacheStats, warm.ManifestDigest, warm.CacheStats)
		}
	})

	t.Run("context bind RUN", func(t *testing.T) {
		inputDir := filepath.Join(contextDir, "input")
		if err := os.MkdirAll(inputDir, 0o700); err != nil {
			t.Fatal(err)
		}
		value := filepath.Join(inputDir, "value")
		write := func(contents string) {
			t.Helper()
			if err := os.WriteFile(value, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		planText := "from \"" + base + "\"\nrun \"cat /input/value >/proof\" network=\"none\" {\n  mount \"bind\" source=\"input\" target=\"/input\" readonly=\"true\"\n}\n"
		write("one\n")
		cold := build("bind-cold", "bind", planText)
		warm := build("bind-warm", "bind", planText)
		if cold.CacheStats.Misses != 1 || cold.CacheStats.Stored != 1 || warm.CacheStats.Hits != 1 {
			t.Fatalf("bind portable cache stats: cold=%+v warm=%+v", cold.CacheStats, warm.CacheStats)
		}
		if warm.ManifestDigest != cold.ManifestDigest {
			t.Fatalf("bind restored manifest = %s, want %s", warm.ManifestDigest, cold.ManifestDigest)
		}
		write("two\n")
		changed := build("bind-changed", "bind", planText)
		if changed.CacheStats.Hits != 0 || changed.CacheStats.Misses != 1 || changed.ManifestDigest == cold.ManifestDigest {
			t.Fatalf("changed bind input reused portable result: cold=%s changed=%s stats=%+v", cold.ManifestDigest, changed.ManifestDigest, changed.CacheStats)
		}
	})

	t.Run("stage COPY", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(contextDir, "stage-value"), []byte("stage source\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		planText := "from \"" + base + "\" as=\"source\"\ncopy \"stage-value\" \"/value\"\nfrom \"" + base + "\"\ncopy \"/value\" \"/proof\" from=\"source\"\n"
		cold := build("stage-cold", "stage", planText)
		warm := build("stage-warm", "stage", planText)
		if cold.CacheStats.Misses != 2 || cold.CacheStats.Stored != 2 || warm.CacheStats.Hits != 2 {
			t.Fatalf("stage-COPY portable cache stats: cold=%+v warm=%+v", cold.CacheStats, warm.CacheStats)
		}
		if warm.ManifestDigest != cold.ManifestDigest {
			t.Fatalf("stage-COPY restored manifest = %s, want %s", warm.ManifestDigest, cold.ManifestDigest)
		}
	})

	t.Run("pinned remote ADD", func(t *testing.T) {
		payload := []byte("immutable remote input\n")
		var requests atomic.Int32
		modified := time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			response.Header().Set("Last-Modified", modified.Add(time.Duration(requests.Add(1))*time.Second).Format(http.TimeFormat))
			_, _ = response.Write(payload)
		}))
		t.Cleanup(server.Close)
		planText := "from \"" + base + "\"\nadd \"" + server.URL + "/payload\" \"/proof\" checksum=\"" + digest.FromBytes(payload).String() + "\"\n"
		cold := build("remote-add-cold", "remote-add", planText)
		warm := build("remote-add-warm", "remote-add", planText)
		if cold.CacheStats.Misses != 1 || cold.CacheStats.Stored != 1 || warm.CacheStats.Hits != 1 || warm.ManifestDigest != cold.ManifestDigest {
			t.Fatalf("pinned remote ADD portable result: cold=%s %+v warm=%s %+v", cold.ManifestDigest, cold.CacheStats, warm.ManifestDigest, warm.CacheStats)
		}
		if got := requests.Load(); got != 1 {
			t.Fatalf("pinned remote ADD requests = %d, want 1 after portable cache hit", got)
		}
	})

	t.Run("pinned Git ADD", func(t *testing.T) {
		source, commit := gitHTTPSubmoduleFixture(t, "")
		for _, keepGitDir := range []bool{false, true} {
			name := "git-clean"
			keep := ""
			if keepGitDir {
				name = "git-metadata"
				keep = " keep-git-dir=\"true\""
			}
			t.Run(name, func(t *testing.T) {
				planText := "from \"" + base + "\"\nadd \"" + source + "#" + commit + "\" \"/source/\" checksum=\"" + commit[:12] + "\"" + keep + "\n"
				cold := build(name+"-cold", name, planText)
				warm := build(name+"-warm", name, planText)
				if cold.CacheStats.Misses != 1 || cold.CacheStats.Stored != 1 || warm.CacheStats.Hits != 1 || warm.ManifestDigest != cold.ManifestDigest {
					t.Fatalf("pinned Git ADD portable result: cold=%s %+v warm=%s %+v", cold.ManifestDigest, cold.CacheStats, warm.ManifestDigest, warm.CacheStats)
				}
				manifest, _ := readPlanImage(t, filepath.Join(root, name+"-warm", "layout"))
				last := manifest.Layers[len(manifest.Layers)-1]
				layer := filepath.Join(root, name+"-warm", "layout", "blobs", "sha256", last.Digest.Encoded())
				if got := readLayerFile(t, layer, "source/deps/submodule/deps/nested/nested-proof"); got != "nested submodule\n" {
					t.Fatalf("cached recursive Git submodule = %q", got)
				}
				if got := layerContainsPath(t, layer, "source/.git/HEAD"); got != keepGitDir {
					t.Fatalf("cached Git metadata retained = %t, want %t", got, keepGitDir)
				}
			})
		}
	})
}

func TestBuildPlanNoCacheReplacesPortableInstructionImage(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	base, host := newLiveBusyBoxRegistry(t, ctx)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, "from \""+base+"\"\nrun \"od -An -N16 -tx1 /dev/urandom | tr -d ' \\\\n' >/proof\" network=\"none\"\n")
	repository := host + "/coopr/no-cache-instruction"
	build := func(name string, noCache bool) Result {
		t.Helper()
		baseDir := filepath.Join(root, name)
		result, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store:      StoreOptions{RunRoot: filepath.Join(baseDir, "run"), GraphRoot: filepath.Join(baseDir, "graph"), GraphDriverName: "vfs"},
			ContextDir: root, Isolation: "rootless", Runtime: "crun", NoCache: noCache,
			Output: Output{Path: filepath.Join(baseDir, "layout")}, ImageStoreDir: filepath.Join(baseDir, "images"),
			CacheRepository: repository, PlainHTTPRegistries: []string{host}, SignaturePolicyPath: policy,
			Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	cold := build("cold", false)
	refreshed := build("refreshed", true)
	if refreshed.ManifestDigest == cold.ManifestDigest {
		t.Fatalf("NoCache build reused portable manifest %s", cold.ManifestDigest)
	}
	if refreshed.CacheStats.Hits != 0 || refreshed.CacheStats.Misses != 0 || refreshed.CacheStats.Stored != 1 {
		t.Fatalf("NoCache portable instruction stats = %+v", refreshed.CacheStats)
	}
	warm := build("warm", false)
	if warm.CacheStats.Hits != 1 || warm.ManifestDigest != refreshed.ManifestDigest {
		t.Fatalf("warm portable instruction result: stats=%+v manifest=%s, want refreshed %s", warm.CacheStats, warm.ManifestDigest, refreshed.ManifestDigest)
	}
}

func TestBuildPlanReusesScratchCopyFromRegistryAcrossStores(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	_, host := newLiveBusyBoxRegistry(t, ctx)
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("scratch cache proof\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, "from \"scratch\"\ncopy \"payload\" \"/proof\"\n")
	resolver, err := oci.NewResolver(oci.Options{PlainHTTPRegistries: []string{host}, ImageStoreDir: filepath.Join(root, "images")})
	if err != nil {
		t.Fatal(err)
	}
	build := func(name string) Result {
		t.Helper()
		baseDir := filepath.Join(root, name)
		result, err := BuildPlan(ctx, plan, PlanOptions{
			Store:      StoreOptions{RunRoot: filepath.Join(baseDir, "run"), GraphRoot: filepath.Join(baseDir, "graph"), GraphDriverName: "vfs"},
			ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: Output{Path: filepath.Join(baseDir, "layout")},
			Resolver: resolver, CacheRepository: host + "/coopr/scratch-cache",
			SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	cold := build("cold")
	warm := build("warm")
	if cold.CacheStats.Misses != 1 || cold.CacheStats.Stored != 1 || cold.CacheStats.Hits != 0 {
		t.Fatalf("cold scratch cache stats = %+v", cold.CacheStats)
	}
	if warm.CacheStats.Hits != 1 || warm.CacheStats.Misses != 0 {
		t.Fatalf("warm scratch cache stats = %+v", warm.CacheStats)
	}
	if warm.ManifestDigest != cold.ManifestDigest {
		t.Fatalf("scratch restored manifest = %s, want exact %s", warm.ManifestDigest, cold.ManifestDigest)
	}
}

func TestBuildPlanPromotesOrdinaryInstructionHitToRegistry(t *testing.T) {
	requireLiveInstructionCache(t)
	for _, format := range []string{"oci", "docker"} {
		t.Run(format, func(t *testing.T) {
			testOrdinaryInstructionHitPromotion(t, format)
		})
	}
}

func testOrdinaryInstructionHitPromotion(t *testing.T, format string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	base, host := newLiveBusyBoxRegistry(t, ctx)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, "from \""+base+"\"\nrun \"od -An -N16 -tx1 /dev/urandom | tr -d ' \\\\n' >/proof\" network=\"none\"\n")
	repository := host + "/coopr/promoted-cache"
	sharedDir := filepath.Join(root, "shared")
	build := func(name, baseDir, cacheRepository string) Result {
		t.Helper()
		result, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store:      StoreOptions{RunRoot: filepath.Join(baseDir, "run"), GraphRoot: filepath.Join(baseDir, "graph"), GraphDriverName: "vfs"},
			ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: Output{Path: filepath.Join(root, name+"-layout"), Format: format},
			ImageStoreDir: filepath.Join(baseDir, "images"), CacheRepository: cacheRepository,
			PlainHTTPRegistries: []string{host}, SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	cold := build("cold", sharedDir, "")
	promoted := build("promoted", sharedDir, repository)
	remote := build("remote", filepath.Join(root, "fresh"), repository)
	if promoted.CacheStats.Stored != 1 || promoted.CacheStats.Errors != 0 {
		t.Fatalf("promoted local hit cache stats = %+v", promoted.CacheStats)
	}
	if remote.CacheStats.Hits != 1 || remote.CacheStats.Misses != 0 {
		t.Fatalf("fresh remote cache stats = %+v", remote.CacheStats)
	}
	if promoted.ManifestDigest != cold.ManifestDigest || remote.ManifestDigest != cold.ManifestDigest {
		t.Fatalf("promoted manifests: cold=%s promoted=%s remote=%s", cold.ManifestDigest, promoted.ManifestDigest, remote.ManifestDigest)
	}
}

func TestFailedBuildDoesNotPublishInstructionImages(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	base, host := newLiveBusyBoxRegistry(t, ctx)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	failed := testPlan(t, "from \""+base+"\"\nrun \"printf checkpoint >/proof\" network=\"none\"\nrun \"exit 17\" network=\"none\"\n")
	options := SupervisedPlanOptions{
		Store:      StoreOptions{RunRoot: filepath.Join(root, "failed", "run"), GraphRoot: filepath.Join(root, "failed", "graph"), GraphDriverName: "vfs"},
		ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: Output{Path: filepath.Join(root, "failed", "layout")},
		ImageStoreDir: filepath.Join(root, "failed", "images"), CacheRepository: host + "/coopr/cache",
		PlainHTTPRegistries: []string{host}, SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
	}
	if _, err := BuildPlanSupervised(ctx, failed, options); err == nil || !strings.Contains(err.Error(), "exit status") {
		t.Fatalf("failed build error = %v", err)
	}
	success := testPlan(t, "from \""+base+"\"\nrun \"printf checkpoint >/proof\" network=\"none\"\n")
	options.Store = StoreOptions{RunRoot: filepath.Join(root, "success", "run"), GraphRoot: filepath.Join(root, "success", "graph"), GraphDriverName: "vfs"}
	options.ImageStoreDir = filepath.Join(root, "success", "images")
	options.Output.Path = filepath.Join(root, "success", "layout")
	result, err := BuildPlanSupervised(ctx, success, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.CacheStats.Hits != 0 || result.CacheStats.Misses != 1 || result.CacheStats.Stored != 1 {
		t.Fatalf("build after failed publication stats = %+v", result.CacheStats)
	}
}
