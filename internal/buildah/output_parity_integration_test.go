package buildah

import (
	"context"
	"coopr/internal/buildcontext"
	"coopr/internal/imageconfig"
	"coopr/internal/planner"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/storage"
)

func TestBuildPlanCompressionAppliesOnCacheHits(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live output compression")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "proof"), []byte("compressed proof\n"), 0600); err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(root, "compression.conf")
	if err := os.WriteFile(module, []byte("[engine]\ncompression_format=\"gzip\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	options := PlanOptions{Store: StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}, ContextDir: contextDir, Isolation: "rootless", Network: "none", RunControls: RunControls{ConfigModules: []string{module}}}
	plan := testPlan(t, "from \"scratch\"\ncopy \"proof\" \"/proof\"\n")
	blobDirectory := filepath.Join(root, "blob-cache")
	if err := os.Mkdir(blobDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	var seedImage string
	for i, force := range []bool{true, true, false} {
		options.Output = Output{Path: filepath.Join(root, fmt.Sprintf("force-default-%d", i)), DisableCompression: true, ForceCompression: boolPointer(force)}
		result, err := BuildPlan(ctx, plan, options)
		if err != nil {
			t.Fatal(err)
		}
		manifest := readLayoutManifest(t, result.Layout)
		want := v1.MediaTypeImageLayer
		if force {
			want = v1.MediaTypeImageLayerGzip
		}
		if len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != want {
			t.Fatalf("default compression force=%t: %#v", force, manifest.Layers)
		}
	}
	for i, algorithm := range []string{"gzip", "zstd", "zstd:chunked", "gzip", ""} {
		options.Output = Output{Path: filepath.Join(root, fmt.Sprintf("output-%d", i)), CompressionFormat: algorithm, DisableCompression: true, BlobDirectory: blobDirectory}
		result, err := BuildPlan(ctx, plan, options)
		if err != nil {
			t.Fatal(err)
		}
		seedImage = result.ImageID
		manifest := readLayoutManifest(t, result.Layout)
		want := v1.MediaTypeImageLayerGzip
		if algorithm == "" {
			want = v1.MediaTypeImageLayer
		}
		if algorithm == "zstd" || algorithm == "zstd:chunked" {
			want = v1.MediaTypeImageLayerZstd
		}
		if algorithm == "zstd:chunked" && manifest.Layers[0].Annotations["io.github.containers.zstd-chunked.manifest-checksum"] == "" {
			t.Fatalf("chunked layer annotations missing: %v", manifest.Layers[0].Annotations)
		}
		if len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != want {
			t.Fatalf("%s layers: %#v", algorithm, manifest.Layers)
		}
	}
	selection, found, err := nativeFixtureSelection(ctx, options.Store, seedImage, v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
	if err != nil || !found {
		t.Fatalf("compression base lookup: %t %v", found, err)
	}
	unchanged := testPlan(t, "from \"compression-base\"\n")
	options.ResolvedBases = map[ResolvedBaseKey]ResolvedImageSource{{Reference: "compression-base", Platform: unchanged.Platform}: {ImageID: selection.ImageID, Root: selection.Root, Selected: selection.Manifest, ConfigData: selection.ConfigData}}
	options.Output = Output{Path: filepath.Join(root, "unchanged-base"), CompressionFormat: "zstd", DisableCompression: true}
	result, err := BuildPlan(ctx, unchanged, options)
	if err != nil {
		t.Fatal(err)
	}
	unchangedManifest := readLayoutManifest(t, result.Layout)
	if result.ImageID != seedImage || len(unchangedManifest.Layers) != 1 || unchangedManifest.Layers[0].MediaType != v1.MediaTypeImageLayerZstd {
		t.Fatalf("unchanged base compression: %s %#v", result.ImageID, unchangedManifest)
	}
	level := 1
	options.Output = Output{Path: filepath.Join(root, "docker-output"), Format: "docker", CompressionFormat: "gzip", CompressionLevel: &level, ForceCompression: boolPointer(false), DisableCompression: true}
	result, err = BuildPlan(ctx, plan, options)
	if err != nil {
		t.Fatal(err)
	}
	dockerManifest := readLayoutManifest(t, result.Layout)
	if dockerManifest.MediaType != define.Dockerv2ImageManifest || len(dockerManifest.Layers) != 1 || dockerManifest.Layers[0].MediaType != manifest.DockerV2Schema2LayerMediaType {
		t.Fatalf("Docker compressed output: %#v", dockerManifest)
	}
	entries, err := os.ReadDir(blobDirectory)
	if err != nil || len(entries) == 0 {
		t.Fatalf("native blob cache is empty: %v", err)
	}
}

func readLayoutManifest(t *testing.T, layout string) v1.Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(layout, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 1 {
		t.Fatalf("manifest index: %#v", index)
	}
	ref := index.Manifests[0].Digest
	data, err = os.ReadFile(filepath.Join(layout, "blobs", ref.Algorithm().String(), ref.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestBuildPlanSavesAndLabelsStages(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live saved stages")
	}
	for _, save := range []bool{false, true} {
		t.Run(map[bool]string{false: "remove", true: "save"}[save], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			root := t.TempDir()
			contextDir := filepath.Join(root, "context")
			if err := os.Mkdir(contextDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(contextDir, "proof"), []byte("saved proof\n"), 0600); err != nil {
				t.Fatal(err)
			}
			options := PlanOptions{Store: StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}, ContextDir: contextDir, Isolation: "rootless", Network: "none", Lifecycle: LifecycleControls{NoLayers: true, SaveStages: save, StageLabels: save}, Output: Output{Path: filepath.Join(root, "layout"), DisableCompression: true}}
			plan := testPlan(t, "from \"scratch\" as=\"producer\"\ncopy \"proof\" \"/proof\"\nfrom \"scratch\" as=\"final\"\ncopy \"/proof\" \"/proof\" from=\"producer\"\n")
			_, err := BuildPlan(ctx, plan, options)
			if err != nil {
				t.Fatal(err)
			}
			store, err := storage.GetStore(storage.StoreOptions{RunRoot: options.Store.RunRoot, GraphRoot: options.Store.GraphRoot, GraphDriverName: "vfs"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := store.Shutdown(false); err != nil {
					t.Errorf("shutdown native store: %v", err)
				}
			}()
			images, err := store.Images()
			if err != nil {
				t.Fatal(err)
			}

			nativeRoot := filepath.Join(root, "upstream")
			nativeStore, err := storage.GetStore(storage.StoreOptions{RunRoot: filepath.Join(nativeRoot, "run"), GraphRoot: filepath.Join(nativeRoot, "graph"), GraphDriverName: "vfs"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := nativeStore.Shutdown(false); err != nil {
					t.Errorf("shutdown native store: %v", err)
				}
			}()
			nativeFile := filepath.Join(root, "Containerfile")
			if err := os.WriteFile(nativeFile, []byte("FROM scratch AS producer\nCOPY proof /proof\nFROM scratch AS final\nCOPY --from=producer /proof /proof\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := imagebuildah.BuildDockerfiles(ctx, nativeStore, define.BuildOptions{ContextDirectory: contextDir, Layers: false, SaveStages: save, StageLabels: save, Isolation: define.IsolationOCIRootless, CommonBuildOpts: &define.CommonBuildOptions{}, Compression: define.Uncompressed, Out: io.Discard, Err: io.Discard, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true}, nativeFile); err != nil {
				t.Fatal(err)
			}
			nativeImages, err := nativeStore.Images()
			if err != nil {
				t.Fatal(err)
			}
			if len(images) != len(nativeImages) {
				t.Fatalf("Coopr/native saved stage counts: %d/%d", len(images), len(nativeImages))
			}
			want := 1
			if save {
				want = 2
			}
			if len(images) != want {
				t.Fatalf("saved=%t images=%d want=%d", save, len(images), want)
			}

			if save {
				nativeLabels := map[string]string{}
				for _, image := range nativeImages {
					data, err := nativeStore.ImageBigData(image.ID, "sha256:"+image.ID)
					if err != nil {
						t.Fatal(err)
					}
					var config v1.Image
					if err := json.Unmarshal(data, &config); err != nil {
						t.Fatal(err)
					}
					nativeLabels[config.Config.Labels["io.buildah.stage.name"]] = config.Config.Labels["io.buildah.stage.base"]
				}
				found := map[string]bool{}

				for _, image := range images {
					data, err := store.ImageBigData(image.ID, "sha256:"+image.ID)
					if err != nil {
						t.Fatal(err)
					}
					var config v1.Image
					if err := json.Unmarshal(data, &config); err != nil {
						t.Fatal(err)
					}
					found[config.Config.Labels["io.buildah.stage.name"]] = true
					if config.Config.Labels["io.buildah.stage.base"] != nativeLabels[config.Config.Labels["io.buildah.stage.name"]] {
						t.Fatalf("stage base labels: %v", config.Config.Labels)
					}
				}
				if !found["producer"] || !found["final"] {
					t.Fatalf("saved labels: %v", found)
				}
			}
		})
	}
}

func TestSavedStageLabelsSkipFromOnlyAndKeepTaggedIntermediates(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live stage label parity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "proof"), []byte("proof\n"), 0600); err != nil {
		t.Fatal(err)
	}
	options := PlanOptions{Store: cacheTestStore(filepath.Join(root, "coopr")), ContextDir: root, Isolation: "rootless", Lifecycle: LifecycleControls{NoLayers: true, SaveStages: true, StageLabels: true}, Output: Output{Path: filepath.Join(root, "layout"), DisableCompression: true}}
	plan := testPlan(t, "from \"scratch\" as=\"producer\"\ncopy \"proof\" \"/proof\"\nfrom \"producer\" as=\"final\"\n")
	result, err := BuildPlan(ctx, plan, options)
	if err != nil {
		t.Fatal(err)
	}
	_, config := readPlanImage(t, result.Layout)
	nativeStore, err := storage.GetStore(storage.StoreOptions{RunRoot: filepath.Join(root, "native", "run"), GraphRoot: filepath.Join(root, "native", "graph"), GraphDriverName: "vfs"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := nativeStore.Shutdown(false); err != nil {
			t.Errorf("shutdown native store: %v", err)
		}
	}()
	file := filepath.Join(root, "Containerfile")
	if err := os.WriteFile(file, []byte("FROM scratch AS producer\nCOPY proof /proof\nFROM producer AS final\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id, _, err := imagebuildah.BuildDockerfiles(ctx, nativeStore, define.BuildOptions{ContextDirectory: root, Layers: false, SaveStages: true, StageLabels: true, Isolation: define.IsolationOCIRootless, CommonBuildOpts: &define.CommonBuildOptions{}, Compression: define.Uncompressed, Out: io.Discard, Err: io.Discard, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true}, file)
	if err != nil {
		t.Fatal(err)
	}
	data, err := nativeStore.ImageBigData(id, "sha256:"+id)
	if err != nil {
		t.Fatal(err)
	}
	var nativeConfig v1.Image
	if err := json.Unmarshal(data, &nativeConfig); err != nil {
		t.Fatal(err)
	}
	if config.Config.Labels["io.buildah.stage.name"] != nativeConfig.Config.Labels["io.buildah.stage.name"] || config.Config.Labels["io.buildah.stage.name"] != "producer" || len(config.History) != len(nativeConfig.History) {
		t.Fatalf("FROM-only metadata/history mismatch: Coopr=%v/%d native=%v/%d", config.Config.Labels, len(config.History), nativeConfig.Config.Labels, len(nativeConfig.History))
	}

	options.Store = cacheTestStore(filepath.Join(root, "retagged"))
	options.Lifecycle = LifecycleControls{NoLayers: true}
	options.Output.Path = filepath.Join(root, "retagged-layout")
	plan = testPlan(t, "from \"scratch\" as=\"producer\"\ncopy \"proof\" \"/proof\"\nfrom \"scratch\"\ncopy \"/proof\" \"/proof\" from=\"producer\"\n")
	_, err = executePlanGraph(ctx, plan, options, plan.Stages, plan.Stages[len(plan.Stages)-1].ID, nil, func(store storage.Store, stage planner.Stage, id string, _ *imageconfig.Config, _ *PackageRootMetadata) error {
		if stage.Name == "producer" {
			return store.SetNames(id, []string{"localhost/retained:latest"})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.GetStore(storage.StoreOptions{RunRoot: options.Store.RunRoot, GraphRoot: options.Store.GraphRoot, GraphDriverName: "vfs"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := store.Shutdown(false); err != nil {
			t.Errorf("shutdown native store: %v", err)
		}
	}()
	if _, err := store.Image("localhost/retained:latest"); err != nil {
		t.Fatalf("tagged intermediate removed: %v", err)
	}
}

func TestPortableInstructionCacheUsesSelectedCompression(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for compressed instruction cache")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(contextDir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cacheDir := filepath.Join(root, "cache")
	plan := testPlan(t, "from \"scratch\"\ncopy \"first\" \"/first\"\ncopy \"second\" \"/second\"\n")
	options := PlanOptions{Store: cacheTestStore(filepath.Join(root, "cold")), ContextDir: contextDir, Isolation: "rootless", Output: Output{Path: filepath.Join(root, "cold-layout"), CompressionFormat: "zstd"}, CacheTo: []CacheSpec{{Transport: "oci-layout", Reference: cacheDir}}}
	cold, err := BuildPlan(ctx, plan, options)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(cacheDir, "blobs", "sha256"))
	if err != nil {
		t.Fatal(err)
	}
	imageCount := 0
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(cacheDir, "blobs", "sha256", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var manifest v1.Manifest
		if json.Unmarshal(data, &manifest) != nil || manifest.Config.MediaType != v1.MediaTypeImageConfig {
			continue
		}
		imageCount++
		for _, layer := range manifest.Layers {
			if layer.MediaType != v1.MediaTypeImageLayerZstd {
				t.Fatalf("cache-to layer compression: %#v", layer)
			}
		}
	}
	if imageCount < 2 {
		t.Fatalf("portable cache images: %d", imageCount)
	}
	options.Store = cacheTestStore(filepath.Join(root, "warm"))
	options.Output.Path = filepath.Join(root, "warm-layout")
	options.CacheTo = nil
	options.CacheFrom = []CacheSpec{{Transport: "oci-layout", Reference: cacheDir}}
	warm, err := BuildPlan(ctx, plan, options)
	if err != nil {
		t.Fatal(err)
	}
	if cold.CacheStats.Stored != 2 || warm.CacheStats.Hits != 2 {
		t.Fatalf("compressed cache reuse: cold=%#v warm=%#v", cold.CacheStats, warm.CacheStats)
	}
	_, coldConfig := readPlanImage(t, cold.Layout)
	_, warmConfig := readPlanImage(t, warm.Layout)
	if len(coldConfig.History) != len(warmConfig.History) || len(warmConfig.RootFS.DiffIDs) != 2 {
		t.Fatalf("compressed cache history/rootfs mismatch: %#v %#v", coldConfig, warmConfig)
	}
}

func TestStageBaseAnnotationsMatchNativeBuildah(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for native stage provenance comparison")
	}
	for _, mode := range []string{"direct", "policy", "named", "digest-multiple-names"} {
		t.Run(mode, func(t *testing.T) { testStageBaseAnnotations(t, mode) })
	}
}

func testStageBaseAnnotations(t *testing.T, mode string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	options := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, options)
	if err := os.WriteFile(filepath.Join(root, "proof"), []byte("stage provenance\n"), 0600); err != nil {
		t.Fatal(err)
	}
	from := base.reference
	if mode == "digest-multiple-names" {
		lease, err := acquireStore(options)
		if err != nil {
			t.Fatal(err)
		}
		selected, err := lease.store.Image(base.reference)
		if err != nil {
			t.Fatal(err)
		}
		if err := lease.store.SetNames(selected.ID, []string{"registry.invalid/other:latest", base.reference}); err != nil {
			t.Fatal(err)
		}
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
		fixture := liveBusyBoxImage(t, ctx)
		from = "fixture.local/coopr/busybox@" + fixture.manifest.Digest.String()
	}
	policyFile := ""
	var contexts []buildcontext.Spec
	var nativeContexts map[string]*define.AdditionalBuildContext
	if mode == "policy" {
		from = "registry.invalid/provenance:latest"
		policyFile = filepath.Join(root, "source-policy.json")
		if err := os.WriteFile(policyFile, []byte(fmt.Sprintf(`{"rules":[{"action":"CONVERT","selector":{"identifier":"docker-image://%s"},"updates":{"identifier":"docker-image://%s"}}]}`, from, base.reference)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "named" {
		from = "tools"
		contexts = []buildcontext.Spec{{Name: from, Kind: buildcontext.DockerImage, Reference: base.reference}}
		nativeContexts = map[string]*define.AdditionalBuildContext{from: {IsImage: true, Value: base.reference}}
	}
	def := parseWorkerDefinition(t, fmt.Sprintf("from %q as=\"producer\"\ncopy \"proof\" \"/proof\"\nfrom \"producer\"\ncopy \"proof\" \"/second\"\n", from))
	result, err := BuildDefinitionSupervised(ctx, def, planner.Options{Mode: planner.Build, BuildContexts: contexts}, SupervisedPlanOptions{SourcePolicyFile: policyFile, PullPolicy: "never", Jobs: 2, Store: options, ContextDir: root, Isolation: "rootless", Output: Output{Path: filepath.Join(root, "coopr-layout")}})
	if err != nil {
		t.Fatal(err)
	}
	nativeFile := filepath.Join(root, "Containerfile")
	if err := os.WriteFile(nativeFile, []byte(fmt.Sprintf("FROM %s AS producer\nCOPY proof /proof\nFROM producer\nCOPY proof /second\n", from)), 0600); err != nil {
		t.Fatal(err)
	}
	lease, err := acquireStore(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}()
	nativeLayout := filepath.Join(root, "native-layout")
	if _, _, err := imagebuildah.BuildDockerfiles(ctx, lease.store, define.BuildOptions{SourcePolicyFile: policyFile, AdditionalBuildContexts: nativeContexts, ContextDirectory: root, Output: "oci:" + nativeLayout, Layers: true, PullPolicy: define.PullNever, Compression: define.Gzip, CommonBuildOpts: &define.CommonBuildOptions{}, Out: io.Discard, Err: io.Discard, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true}, nativeFile); err != nil {
		t.Fatal(err)
	}
	actual, _ := readPlanImage(t, result.Layout)
	native, _ := readPlanImage(t, nativeLayout)
	if actual.Annotations[v1.AnnotationBaseImageDigest] != native.Annotations[v1.AnnotationBaseImageDigest] {
		t.Fatalf("stage base digest Coopr=%q native=%q", actual.Annotations[v1.AnnotationBaseImageDigest], native.Annotations[v1.AnnotationBaseImageDigest])
	}
	if actual.Annotations[v1.AnnotationBaseImageName] != native.Annotations[v1.AnnotationBaseImageName] {
		t.Fatalf("stage base name Coopr=%q native=%q", actual.Annotations[v1.AnnotationBaseImageName], native.Annotations[v1.AnnotationBaseImageName])
	}
}
