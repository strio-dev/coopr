package buildah

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
)

func TestWorkDirMetadataMatchesNativeBuildah(t *testing.T) {
	for _, steps := range [][]string{
		{"/one/../two"}, {"/one/../two/"}, {"/one//two//"}, {"/base/", "nested", "../next"}, {"/absolute"}, {"/"},
	} {
		t.Run(strings.Join(steps, ";"), func(t *testing.T) {
			f := newLocalComponentFixture(t)
			var nativeSource, kdl strings.Builder
			nativeSource.WriteString("FROM scratch\n")
			kdl.WriteString("from \"scratch\" {\n")
			for _, value := range steps {
				fmt.Fprintf(&nativeSource, "WORKDIR %s\n", value)
				fmt.Fprintf(&kdl, "workdir %q\n", value)
			}
			kdl.WriteString("}\n")
			file := filepath.Join(f.contextDir, "Containerfile")
			if err := os.WriteFile(file, []byte(nativeSource.String()), 0600); err != nil {
				t.Fatal(err)
			}
			nativeLayout := filepath.Join(f.root, "native")
			lease, err := acquireStore(f.options.Store)
			if err != nil {
				t.Fatal(err)
			}
			_, _, nativeErr := imagebuildah.BuildDockerfiles(f.ctx, lease.store, define.BuildOptions{ContextDirectory: f.contextDir, Output: "oci:" + nativeLayout, Layers: true, PullPolicy: define.PullNever, Isolation: define.IsolationOCIRootless, Runtime: "crun", CommonBuildOpts: &define.CommonBuildOptions{}, Compression: define.Uncompressed, Out: io.Discard, Err: os.Stderr, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true}, file)
			closeErr := lease.Close()
			if nativeErr != nil || closeErr != nil {
				t.Fatalf("actual native build: %v / %v", nativeErr, closeErr)
			}
			_, native := readPlanImage(t, nativeLayout)
			t.Logf("actual native build working directory=%q", native.Config.WorkingDir)
			logical := imageconfig.New()
			for _, value := range steps {
				if err := logical.Apply(definition.Instruction{Name: "workdir", Arguments: []string{value}}); err != nil {
					t.Fatal(err)
				}
			}
			logicalBytes, err := logical.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			var logicalConfig struct{ Config struct{ WorkingDir string } }
			if err := json.Unmarshal(logicalBytes, &logicalConfig); err != nil {
				t.Fatal(err)
			}
			restored, err := imageconfig.Parse(logicalBytes)
			if err != nil {
				t.Fatal(err)
			}
			roundtrip, err := restored.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			if string(logicalBytes) != string(roundtrip) {
				t.Fatal("logical configuration cache roundtrip changed WORKDIR")
			}
			logicalDir := logicalConfig.Config.WorkingDir
			if logicalDir != native.Config.WorkingDir {
				t.Errorf("logical working directory=%q native=%q", logicalDir, native.Config.WorkingDir)
			}
			for _, attempt := range []string{"cold", "warm", "fresh-store"} {
				if attempt == "fresh-store" {
					f.options.Store = cacheTestStore(filepath.Join(f.root, "fresh-native-store"))
				}
				var progress bytes.Buffer
				layout := filepath.Join(f.root, attempt)
				result, err := BuildPlanSupervised(f.ctx, testPlan(t, kdl.String()), SupervisedPlanOptions{
					Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun", CacheLocalDir: f.options.CacheLocalDir,
					Output: Output{Path: layout}, SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath, Stdout: io.Discard, Stderr: &progress,
				})
				if err != nil {
					t.Fatal(err)
				}
				_, got := readPlanImage(t, layout)
				if got.Config.WorkingDir != native.Config.WorkingDir {
					t.Errorf("%s working directory=%q native=%q", attempt, got.Config.WorkingDir, native.Config.WorkingDir)
				}
				if attempt == "warm" && !strings.Contains(progress.String(), "--> Using cache ") {
					t.Errorf("warm native cache was not reused: %s", progress.String())
				}
				if attempt == "fresh-store" && result.CacheStats.Hits == 0 {
					t.Errorf("%s did not restore the instruction cache: %+v", attempt, result.CacheStats)
				}
			}
		})
	}
}
