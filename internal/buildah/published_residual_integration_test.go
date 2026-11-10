package buildah

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/componentstore"
	"github.com/strio-dev/coopr/internal/imageconfig"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/planner"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestPublishedResidualSnapshotsAndLateSelection(t *testing.T) {
	if testing.Short() {
		t.Skip("live immutable package replay")
	}
	for _, captureBoth := range []bool{false, true} {
		t.Run(fmt.Sprintf("capture-both=%t", captureBoth), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			root := t.TempDir()
			payloads := map[string][]byte{"one": []byte("immutable first snapshot\n"), "two": []byte("immutable second snapshot\n")}
			stamp := time.Unix(1700000000, 0)
			for name, data := range payloads {
				path := filepath.Join(root, name)
				if err := os.WriteFile(path, data, 0o640); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			policy := writeComponentTestPolicy(t, root)
			store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
			components := filepath.Join(root, "components")
			layout := filepath.Join(root, "published")
			body := ""
			if captureBoth {
				body = `copy "/value" "/other" from="two";`
			}
			source := parseWorkerDefinition(t, `arg "source" "one"
from "scratch" as="producer" { copy "one" "/one"; }
package as="one" { copy "/one" "/value" from="producer"; env "VALUE" "one"; workdir "/one"; onbuild { env "TRIGGER" "one"; }; }
package as="two" { copy "two" "/value"; env "VALUE" "two"; workdir "/two"; onbuild { env "TRIGGER" "two"; }; }
extend { from "${source}" as="selected" { `+body+` }; }
`)
			published, err := PublishDefinitionSupervised(ctx, source, planner.Options{Mode: planner.Publish, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout}, SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard, Jobs: 1})
			if err != nil {
				t.Fatal(err)
			}
			metadata := readComponentMetadata(t, ctx, layout, published.Root)
			count := 1
			if captureBoth {
				count = 2
			}
			if len(metadata.Packages) != count || metadata.Component.Output != "4" {
				t.Fatalf("wrong captured packages or original output: %+v", metadata)
			}
			artifact, err := orasoci.NewWithContext(ctx, layout)
			if err != nil {
				t.Fatal(err)
			}
			if err := componentstore.Put(ctx, components, artifact, published.Root, "residual"); err != nil {
				t.Fatal(err)
			}
			for name := range payloads {
				if err := os.Remove(filepath.Join(root, name)); err != nil {
					t.Fatal(err)
				}
			}
			var coldLayers []v1.Descriptor
			var coldImage string
			for _, attempt := range []string{"cold", "warm", "switch"} {
				name := "one"
				if attempt == "switch" {
					name = "two"
				}
				output := filepath.Join(root, attempt)
				definition := parseWorkerDefinition(t, fmt.Sprintf(`from "scratch" { component "residual" { arg "source" %q; }; }`, name))
				got, err := BuildDefinitionSupervised(ctx, definition, planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: output}, ComponentStoreDir: components, CacheLocalDir: filepath.Join(root, "cache"), SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard, Jobs: 1})
				if attempt == "switch" && !captureBoth {
					if err == nil || !strings.Contains(err.Error(), "was not captured") {
						t.Fatalf("uncaptured package selection: %v", err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				manifest, config := readPlanImage(t, output)
				configBytes, err := oci.ReadImageConfigLayout(ctx, output)
				if err != nil {
					t.Fatal(err)
				}
				fullConfig, err := imageconfig.Parse(configBytes)
				if err != nil {
					t.Fatal(err)
				}
				triggers, err := fullConfig.OnBuild()
				if err != nil || len(triggers) != 0 {
					t.Fatalf("inherited triggers were not consumed: %v, %v", triggers, err)
				}
				if config.Config.WorkingDir != "/"+name || !slices.Contains(config.Config.Env, "VALUE="+name) || !slices.Contains(config.Config.Env, "TRIGGER="+name) {
					t.Fatalf("wrong inherited/consumed configuration: %+v", config.Config)
				}
				if len(manifest.Layers) == 0 {
					t.Fatal("missing snapshot layer")
				}
				blob := filepath.Join(output, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
				if !bytes.Equal([]byte(readLayerFile(t, blob, "value")), payloads[name]) {
					t.Fatal("wrong immutable snapshot bytes")
				}
				header := readLayerHeader(t, blob, "value")
				if header.Mode&0o777 != 0o640 || header.Uid != 0 || header.Gid != 0 || !header.ModTime.Equal(stamp) {
					t.Fatalf("snapshot metadata changed: %+v", header)
				}
				if attempt == "cold" {
					coldLayers = manifest.Layers
					coldImage = got.ImageID
					if got.CacheStats.Misses == 0 {
						t.Fatal("cold component cache was not exercised")
					}
				}
				if attempt == "warm" && (!reflect.DeepEqual(coldLayers, manifest.Layers) || coldImage != got.ImageID || got.CacheStats.Hits == 0) {
					t.Fatalf("warm replay changed output or missed cache: %+v", got.CacheStats)
				}
			}
		})
	}
}
