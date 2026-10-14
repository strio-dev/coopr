package buildah

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"coopr/internal/cache"
	"coopr/internal/imageconfig"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
)

func TestAggregateCachesInvalidateOldWorkdirSemantics(t *testing.T) {
	f := newLocalComponentFixture(t)
	const workdir = "/a/../b/"
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
	if err := os.WriteFile(file, []byte("FROM scratch\nWORKDIR "+workdir+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	nativeID, _, err := imagebuildah.BuildDockerfiles(f.ctx, lease.store, define.BuildOptions{ContextDirectory: f.contextDir, PullPolicy: define.PullNever, Isolation: define.IsolationOCIRootless, Runtime: "crun", CommonBuildOpts: &define.CommonBuildOptions{}, SystemContext: f.options.SystemContext, Out: io.Discard, Err: io.Discard, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true}, file)
	if err != nil {
		t.Fatal(err)
	}
	nativeRaw, err := packageImageConfig(f.ctx, lease.store, nativeID, f.options.SystemContext)
	if err != nil {
		t.Fatal(err)
	}
	var native v1.Image
	if err := json.Unmarshal(nativeRaw, &native); err != nil {
		t.Fatal(err)
	}
	expected := native.Config.WorkingDir
	if expected == "/b" || expected == "" {
		t.Fatalf("native oracle did not distinguish lexical path: %q", expected)
	}
	t.Logf("actual native WORKDIR %q produces %q", workdir, expected)

	t.Run("component", func(t *testing.T) {
		legacy, _ := f.build(t, "legacy-component", `from "scratch" { workdir "/b"; }`)
		raw, err := packageImageConfig(f.ctx, lease.store, legacy.ImageID, f.options.SystemContext)
		if err != nil {
			t.Fatal(err)
		}
		logical, err := imageconfig.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		staging := t.TempDir()
		store, err := cache.NewLocalStore(f.ctx, filepath.Join(f.root, "component-semantic-cache"), staging, nil)
		if err != nil {
			t.Fatal(err)
		}
		executor := &graphExecutor{store: lease.store, options: f.options}
		resolved := &ResolvedComponentPlan{Identity: digest.FromString("authored-workdir-" + workdir), Plan: testPlan(t, `from "scratch" { workdir "/a/../b/"; }`)}
		current, err := new(componentCache).key(executor, validComponentTestKey().Input, resolved, nil, v1.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}, false)
		if err != nil {
			t.Fatal(err)
		}
		old := current
		old.Lowering = "coopr-buildah-component-image-v7"
		producer := &componentCache{stagingDir: staging, writeStores: []cache.Store{store}}
		producer.record(f.ctx, executor, old, legacy.ImageID, digest.Digest(legacy.ManifestDigest), logical, &PackageRootMetadata{Mode: 0755}, f.options.SystemContext, legacy.ImageID, false)
		if err := producer.publish(f.ctx); err != nil {
			t.Fatal(err)
		}
		if producer.stats.Errors != 0 || producer.stats.Stored != 1 {
			t.Fatalf("legacy cache not published: %+v", producer.stats)
		}
		consumer := &componentCache{stagingDir: t.TempDir(), readStores: []cache.Store{store}}
		entry, _, err := consumer.lookup(f.ctx, executor, current, f.options.SystemContext, componentCacheCaller{ImageID: legacy.ImageID})
		if err != nil {
			t.Fatal(err)
		}
		if entry.ImageID != "" {
			t.Fatal("current component semantics reused normalized legacy WORKDIR image")
		}
		fresh, layout := f.build(t, "current-component", `from "scratch" { workdir "/a/../b/"; }`)
		_, image := readPlanImage(t, layout)
		if image.Config.WorkingDir != expected {
			t.Fatalf("fresh component config=%q native=%q", image.Config.WorkingDir, expected)
		}
		raw, err = packageImageConfig(f.ctx, lease.store, fresh.ImageID, f.options.SystemContext)
		if err != nil {
			t.Fatal(err)
		}
		logical, err = imageconfig.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		producer.record(f.ctx, executor, current, fresh.ImageID, digest.Digest(fresh.ManifestDigest), logical, &PackageRootMetadata{Mode: 0755}, f.options.SystemContext, fresh.ImageID, false)
		if err := producer.publish(f.ctx); err != nil {
			t.Fatal(err)
		}
		warm := &componentCache{stagingDir: t.TempDir(), readStores: []cache.Store{store}}
		entry, config, err := warm.lookup(f.ctx, executor, current, f.options.SystemContext, componentCacheCaller{ImageID: fresh.ImageID})
		if err != nil || entry.ImageID == "" || config == nil || warm.stats.Hits != 1 {
			t.Fatalf("current component warm lookup: %+v %+v %v", entry, warm.stats, err)
		}
		data, err := config.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &image); err != nil {
			t.Fatal(err)
		}
		if image.Config.WorkingDir != expected {
			t.Fatalf("warm component config=%q native=%q", image.Config.WorkingDir, expected)
		}
	})

	t.Run("instruction-replay", func(t *testing.T) {
		instructionFixture := f
		instructionFixture.options.CacheLocalDir = filepath.Join(f.root, "instruction-semantic-cache")
		for attempt, name := range []string{"cold", "warm"} {
			instructionFixture.options.Store = cacheTestStore(filepath.Join(f.root, "instruction-"+name+"-store"))
			result, layout := instructionFixture.build(t, "instruction-"+name, `from "scratch" { workdir "/a/../b/"; }`)
			_, image := readPlanImage(t, layout)
			if image.Config.WorkingDir != expected {
				t.Fatalf("%s instruction metadata=%q native=%q", name, image.Config.WorkingDir, expected)
			}
			if attempt == 1 && result.CacheStats.Hits < 1 {
				t.Fatalf("warm instruction did not restore cached filesystem: %+v", result.CacheStats)
			}
		}
	})

	t.Run("package", func(t *testing.T) {
		plan := testPublicationPlan(t, `package as="bundle" { workdir "/a/../b/"; }
extend { copy "/b" "/b" from="bundle"; }`)
		originalTar := filepath.Join(f.root, "original-package.tar")
		options := f.options
		options.CacheLocalDir = filepath.Join(f.root, "original-package-cache")
		_, err := PublishPlan(f.ctx, plan, PublicationOptions{PlanOptions: options, PackagePaths: map[string]string{"bundle": originalTar}})
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(options.CacheLocalDir, "index.json"))
		if err != nil {
			t.Fatal(err)
		}
		var index v1.Index
		if err := json.Unmarshal(data, &index); err != nil {
			t.Fatal(err)
		}
		var record cache.PackageRecord
		for _, artifact := range index.Manifests {
			data, err := os.ReadFile(filepath.Join(options.CacheLocalDir, "blobs", "sha256", artifact.Digest.Encoded()))
			if err != nil {
				t.Fatal(err)
			}
			var manifest v1.Manifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.ArtifactType != cache.PackageArtifactType {
				continue
			}
			data, err = os.ReadFile(filepath.Join(options.CacheLocalDir, "blobs", "sha256", manifest.Config.Digest.Encoded()))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
		}
		if record.Stage == "" {
			t.Fatal("missing actual producer package cache record")
		}
		record.Key.Lowering = "coopr-buildah-package-v2"
		var old v1.Image
		if err := json.Unmarshal(record.Config, &old); err != nil {
			t.Fatal(err)
		}
		old.Config.WorkingDir = "/b"
		record.Config, err = json.Marshal(old)
		if err != nil {
			t.Fatal(err)
		}
		cacheDir := filepath.Join(f.root, "package-semantic-cache")
		store, err := cache.NewLocalStore(f.ctx, cacheDir, t.TempDir(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.PutPackage(f.ctx, record.Key, record, originalTar); err != nil {
			t.Fatal(err)
		}
		for attempt, name := range []string{"current", "warm"} {
			root := filepath.Join(f.root, name+"-package")
			options := f.options
			options.Store = cacheTestStore(root)
			options.CacheLocalDir = cacheDir
			packages, err := PublishPlan(f.ctx, plan, PublicationOptions{PlanOptions: options, PackagePaths: map[string]string{"bundle": filepath.Join(root, "bundle.tar")}})
			if err != nil {
				t.Fatal(err)
			}
			var image v1.Image
			if err := json.Unmarshal(packages["bundle"].Config, &image); err != nil {
				t.Fatal(err)
			}
			if image.Config.WorkingDir != expected {
				t.Fatalf("%s package reused stale config=%q native=%q", name, image.Config.WorkingDir, expected)
			}
			lease, err := acquireStore(options.Store)
			if err != nil {
				t.Fatal(err)
			}
			images, imageErr := lease.store.Images()
			closeErr := lease.Close()
			if imageErr != nil || closeErr != nil {
				t.Fatalf("inspect %s producer: %v %v", name, imageErr, closeErr)
			}
			if (len(images) > 0) != (attempt == 0) {
				t.Fatalf("%s package images=%d; first must execute, second must use aggregate", name, len(images))
			}
		}
	})
}
