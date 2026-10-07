package buildah

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/buildcontext"
	"coopr/internal/planner"
)

func TestDynamicComponentOnbuildMaterializesLateExternalImageAndContext(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for late image binding")
	}
	for _, named := range []bool{false, true} {
		t.Run(fmt.Sprintf("named-context-%t", named), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			root := t.TempDir()
			store := StoreOptions{GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"), GraphDriverName: "vfs"}
			base := newLiveBusyBoxStorage(t, ctx, root, store)
			external, authority := newLiveBusyBoxRegistry(t, ctx)
			from := external
			var contexts []buildcontext.Spec
			if named {
				from = "late"
				contexts = []buildcontext.Spec{{Name: "late", Kind: buildcontext.DockerImage, Reference: external}}
			}
			localConfigComponentResolver(t, ctx, root, fmt.Sprintf("extend\nonbuild { copy \"/bin/busybox\" \"/late-tool\" from=%q }\n", from), false)
			def := parseWorkerDefinition(t, fmt.Sprintf("from %q as=\"base\"\ncomponent \"local:config\"\nfrom \"base\"\n", base.reference))
			layout := filepath.Join(root, "result")
			_, err := BuildDefinitionSupervised(ctx, def, planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{
				Store: store, ContextDir: root, Isolation: "rootless", ComponentStoreDir: filepath.Join(root, "components"),
				PlainHTTPRegistries: []string{authority}, SignaturePolicyPath: writeComponentTestPolicy(t, root), BuildContexts: contexts,
				Output: Output{Path: layout},
			})
			if err != nil {
				t.Fatal(err)
			}
			manifest, _ := readPlanImage(t, layout)
			last := manifest.Layers[len(manifest.Layers)-1]
			got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "late-tool")
			if len(got) < 1024 || !strings.HasPrefix(got, "\x7fELF") {
				t.Fatal("late external source did not supply the executable")
			}
		})
	}
}
