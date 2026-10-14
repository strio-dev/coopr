package buildah

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
)

func TestInlineCopyAddMatchesNativeHeredocCollisionAndExclusions(t *testing.T) {
	for _, test := range []struct {
		kind string
		from bool
	}{{"copy", false}, {"add", false}, {"copy", true}} {
		kind := test.kind
		t.Run(fmt.Sprintf("%s/from=%t", kind, test.from), func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.write(t, "same", "ordinary\n")
			f.write(t, ".containerignore", "*\n!same\n")
			source := fmt.Sprintf(`from "scratch" { %s "same" "/out/" chmod="0640" chown="12:34" { exclude "only"; }; }`, kind)
			if test.from {
				source = `from "scratch" as="source" { copy "same" "/same"; }` + "\n" + strings.Replace(source, `"/out/" chmod=`, `"/out/" from="source" chmod=`, 1)
			}
			document, err := definition.Parse(strings.NewReader(source))
			if err != nil {
				t.Fatal(err)
			}
			document.Instructions[len(document.Instructions)-1].Children[0].InlineFiles = []definition.InlineFile{{Path: "same", Data: "inline\n"}, {Path: "only", Data: "kept\n"}}
			plan, err := planner.Create(document, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH})
			if err != nil {
				t.Fatal(err)
			}
			var cold digest.Digest
			for attempt := range 2 {
				layout := filepath.Join(f.root, fmt.Sprintf("coopr-%d", attempt))
				options := f.options
				options.Output = Output{Path: layout}
				if _, err := BuildPlan(f.ctx, plan, options); err != nil {
					t.Fatal(err)
				}
				manifest, _ := readPlanImage(t, layout)
				if len(manifest.Layers) != 1 {
					t.Fatalf("instruction layers = %d", len(manifest.Layers))
				}
				if attempt == 0 {
					cold = manifest.Layers[0].Digest
				} else if cold != manifest.Layers[0].Digest {
					t.Fatal("warm cache changed instruction layer")
				}
				layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
				for path, want := range map[string]string{"out/same": "ordinary\n", "out/only": "kept\n"} {
					if got := readLayerFile(t, layer, path); got != want {
						t.Fatalf("%s = %q, want %q", path, got, want)
					}
					header := readLayerHeader(t, layer, path)
					if header.Mode != 0640 || header.Uid != 12 || header.Gid != 34 {
						t.Fatalf("%s metadata = %o %d:%d", path, header.Mode, header.Uid, header.Gid)
					}
				}
			}
			wantRecords := 1
			if test.from {
				wantRecords = 2
			}
			if count := instructionCacheRecordCount(t, f.options.Store); count != wantRecords {
				t.Fatalf("cache records = %d, want %d", count, wantRecords)
			}
			document.Instructions[len(document.Instructions)-1].Children[0].InlineFiles[1].Data = "changed\n"
			changedPlan, err := planner.Create(document, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH})
			if err != nil {
				t.Fatal(err)
			}
			changedLayout := filepath.Join(f.root, "changed")
			changedOptions := f.options
			changedOptions.Output = Output{Path: changedLayout}
			if _, err := BuildPlan(f.ctx, changedPlan, changedOptions); err != nil {
				t.Fatal(err)
			}
			changedManifest, _ := readPlanImage(t, changedLayout)
			if len(changedManifest.Layers) != 1 || changedManifest.Layers[0].Digest == cold {
				t.Fatal("changed inline payload reused old instruction layer")
			}
			changedLayer := filepath.Join(changedLayout, "blobs", "sha256", changedManifest.Layers[0].Digest.Encoded())
			if got := readLayerFile(t, changedLayer, "out/only"); got != "changed\n" {
				t.Fatalf("changed inline payload = %q", got)
			}
			if count := instructionCacheRecordCount(t, f.options.Store); count != wantRecords+1 {
				t.Fatalf("changed inline cache records = %d, want %d", count, wantRecords+1)
			}
			file := filepath.Join(f.contextDir, "Containerfile")
			contents := fmt.Sprintf("FROM scratch\n%s --chmod=0640 --chown=12:34 --exclude=only same <<same <<only /out/\ninline\nsame\nkept\nonly\n", strings.ToUpper(kind))
			if test.from {
				contents = "FROM scratch AS source\nCOPY same /same\n" + strings.Replace(contents, " --chmod=", " --from=source --chmod=", 1)
			}
			if err := os.WriteFile(file, []byte(contents), 0600); err != nil {
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
			if len(manifest.Layers) != 1 {
				t.Fatalf("native instruction layers = %d", len(manifest.Layers))
			}
			layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
			for path, want := range map[string]string{"out/same": "ordinary\n", "out/only": "kept\n"} {
				if got := readLayerFile(t, layer, path); got != want {
					t.Fatalf("native %s = %q, want %q", path, got, want)
				}
			}
		})
	}
}
