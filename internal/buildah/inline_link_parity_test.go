package buildah

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/planner"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
)

func TestInlineLinkedSourcesMatchNativeLayerBatch(t *testing.T) {
	for _, kind := range []string{"copy", "add"} {
		for _, mixed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/mixed=%t", kind, mixed), func(t *testing.T) {
				f := newLocalComponentFixture(t)
				f.write(t, "same", "ordinary\n")
				ordinary := `"same" `
				if mixed {
					ordinary = `"same" `
				}
				document, err := definition.Parse(strings.NewReader(fmt.Sprintf(`from "scratch" { %s %s"/out/" link=#true; }`, kind, ordinary)))
				if err != nil {
					t.Fatal(err)
				}
				if !mixed {
					document.Instructions[0].Children[0].Arguments = []string{"/out/"}
				}
				document.Instructions[0].Children[0].InlineFiles = []definition.InlineFile{{Path: "same", Data: "inline\n"}, {Path: "second", Data: "second payload\n"}}
				wantLayers := 1
				if mixed {
					wantLayers = 2
				}
				var cold digest.Digest
				for attempt := range 2 {
					if attempt == 1 {
						labels, err := definition.Parse(strings.NewReader(`from "scratch" { label revision="warm"; }`))
						if err != nil {
							t.Fatal(err)
						}
						document.Instructions[0].Children = append(labels.Instructions[0].Children, document.Instructions[0].Children...)
					}
					plan, err := planner.Create(document, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH})
					if err != nil {
						t.Fatal(err)
					}
					layout := filepath.Join(f.root, fmt.Sprintf("coopr-%d", attempt))
					options := f.options
					options.Output = Output{Path: layout}
					if _, err := BuildPlan(f.ctx, plan, options); err != nil {
						t.Fatal(err)
					}
					manifest, _ := readPlanImage(t, layout)
					if len(manifest.Layers) != wantLayers {
						t.Fatalf("Coopr linked layers = %d, want native %d", len(manifest.Layers), wantLayers)
					}
					if attempt == 0 {
						cold = manifest.Layers[0].Digest
					} else if cold != manifest.Layers[0].Digest {
						t.Fatal("warm linked layer changed")
					}
					layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
					if got := readLayerFile(t, layer, "out/second"); got != "second payload\n" {
						t.Fatalf("batched second source = %q", got)
					}
				}
				file := filepath.Join(f.contextDir, "Containerfile")
				nativeSource := ""
				if mixed {
					nativeSource = "same "
				}
				if err := os.WriteFile(file, []byte(fmt.Sprintf("FROM scratch\n%s --link %s<<same <<second /out/\ninline\nsame\nsecond payload\nsecond\n", strings.ToUpper(kind), nativeSource)), 0600); err != nil {
					t.Fatal(err)
				}
				lease, err := acquireStore(f.options.Store)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := lease.Close(); err != nil {
						t.Error(err)
					}
				}()
				layout := filepath.Join(f.root, "native")
				_, _, err = imagebuildah.BuildDockerfiles(f.ctx, lease.store, define.BuildOptions{ContextDirectory: f.contextDir, Output: "oci:" + layout, Layers: true, PullPolicy: define.PullNever, Isolation: define.IsolationOCIRootless, Runtime: "crun", CommonBuildOpts: &define.CommonBuildOptions{}, Compression: define.Uncompressed, Out: io.Discard, Err: os.Stderr, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true}, file)
				if err != nil {
					t.Fatal(err)
				}
				manifest, _ := readPlanImage(t, layout)
				if len(manifest.Layers) != wantLayers {
					t.Fatalf("native linked layers = %d, want %d", len(manifest.Layers), wantLayers)
				}
			})
		}
	}
}

func TestInlineBatchDigestMatchesSeparateCalls(t *testing.T) {
	f := newLocalComponentFixture(t)
	lease, err := acquireStore(f.options.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}()
	builder, err := upstream.NewBuilder(f.ctx, lease.store, upstream.BuilderOptions{FromImage: "scratch", PullPolicy: define.PullNever, Isolation: define.IsolationChroot})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := builder.Delete(); err != nil {
			t.Error(err)
		}
	}()
	adapter := nativeBuilder{Builder: builder, isolation: define.IsolationChroot}
	sources := []definition.InlineFile{{Path: "first", Data: "one"}, {Path: "second", Data: "two"}}
	options := upstream.AddAndCopyOptions{Link: true, DryRun: true}
	for _, source := range sources {
		if err := addInlineData(adapter, "/out/", []definition.InlineFile{source}, options); err != nil {
			t.Fatal(err)
		}
	}
	separateKind, separateDigest := builder.ContentDigester.Digest()
	builder.ContentDigester.Restart()
	if err := addInlineData(adapter, "/out/", sources, options); err != nil {
		t.Fatal(err)
	}
	batchKind, batchDigest := builder.ContentDigester.Digest()
	t.Logf("separate %s:%s; batched %s:%s", separateKind, separateDigest, batchKind, batchDigest)
	if separateKind != batchKind || separateDigest != batchDigest {
		t.Fatal("batching unexpectedly changed native input digest; reassess cache version evidence")
	}
}

func TestInlineLinkedBatchRespectsLayerGroupingAndNoLayers(t *testing.T) {
	for _, kind := range []string{"copy", "add"} {
		for _, noLayers := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/no-layers=%t", kind, noLayers), func(t *testing.T) {
				f := newLocalComponentFixture(t)
				f.write(t, "same", "ordinary\n")
				document, err := definition.Parse(strings.NewReader(fmt.Sprintf(`from "scratch" { layer { %s "same" "/out/" link=#true; }; }`, kind)))
				if err != nil {
					t.Fatal(err)
				}
				document.Instructions[0].Children[0].Children[0].InlineFiles = []definition.InlineFile{{Path: "same", Data: "inline\n"}, {Path: "second", Data: "second payload\n"}}
				plan, err := planner.Create(document, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH})
				if err != nil {
					t.Fatal(err)
				}
				for attempt := range 2 {
					layout := filepath.Join(f.root, fmt.Sprintf("layout-%d", attempt))
					options := f.options
					options.Output = Output{Path: layout}
					options.Lifecycle.NoLayers = noLayers
					if _, err := BuildPlan(f.ctx, plan, options); err != nil {
						t.Fatal(err)
					}
					manifest, _ := readPlanImage(t, layout)
					if len(manifest.Layers) != 1 {
						t.Fatalf("grouped/no-layers inline instruction emitted %d layers", len(manifest.Layers))
					}
					layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
					for path, want := range map[string]string{"out/same": "ordinary\n", "out/second": "second payload\n"} {
						if got := readLayerFile(t, layer, path); got != want {
							t.Fatalf("%s = %q, want %q", path, got, want)
						}
					}
				}
			})
		}
	}
}

func TestInlineLinkedBatchSurvivesWorkersAndPortableCache(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "same", "ordinary\n")
	document, err := definition.Parse(strings.NewReader(`from "scratch" as="side" { copy "same" "/side"; }
from "scratch" as="primary" { layer { copy "same" "/out/" link=#true; }; }
from "primary" { copy "/side" "/side" from="side"; }`))
	if err != nil {
		t.Fatal(err)
	}
	document.Instructions[1].Children[0].Children[0].InlineFiles = []definition.InlineFile{{Path: "same", Data: "inline\n"}, {Path: "second", Data: "second payload\n"}}
	plan, err := planner.Create(document, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH})
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []string{"cold", "warm", "fresh-store"} {
		store := f.options.Store
		if attempt == "fresh-store" {
			store = cacheTestStore(filepath.Join(f.root, "fresh-store-storage"))
		}
		layout := filepath.Join(f.root, attempt)
		result, err := BuildPlanSupervised(f.ctx, plan, SupervisedPlanOptions{Store: store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout}, Jobs: 2, CacheLocalDir: filepath.Join(f.root, "portable"), SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath, Stdout: io.Discard, Stderr: os.Stderr})
		if err != nil {
			t.Fatal(err)
		}
		if attempt == "fresh-store" && result.CacheStats.Hits == 0 {
			t.Fatalf("%s did not restore instruction cache: %+v", attempt, result.CacheStats)
		}
		manifest, _ := readPlanImage(t, layout)
		if len(manifest.Layers) != 2 {
			t.Fatalf("%s output layers = %d, want compact group plus side copy", attempt, len(manifest.Layers))
		}
		first := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
		if got := readLayerFile(t, first, "out/second"); got != "second payload\n" {
			t.Fatalf("%s grouped payload = %q", attempt, got)
		}
		last := filepath.Join(layout, "blobs", "sha256", manifest.Layers[1].Digest.Encoded())
		if got := readLayerFile(t, last, "side"); got != "ordinary\n" {
			t.Fatalf("%s worker payload = %q", attempt, got)
		}
	}
}
