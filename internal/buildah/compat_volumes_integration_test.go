package buildah

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/definition"
	"coopr/internal/planner"
)

func TestBuildDefinitionCompatVolumesPreservesRunBoundaryButAllowsCopy(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for live compatibility-volume coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	if err := os.WriteFile(filepath.Join(root, "copied"), []byte("copied"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := fmt.Sprintf(`from %q
run "/bin/busybox mkdir -p /vol; printf before >/vol/value" network="none"
volume "/vol"
run "printf during >/vol/value" network="none"
copy "copied" "/vol/copied"
run "printf '%%s:%%s' \"$(/bin/busybox cat /vol/value)\" \"$(/bin/busybox cat /vol/copied)\" >/proof" network="none"
`, base.reference)
	def, err := definition.Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name   string
		compat bool
		want   string
	}{
		{name: "normal", want: "during:copied"},
		{name: "compat", compat: true, want: "before:copied"},
	} {
		t.Run(test.name, func(t *testing.T) {
			layout := filepath.Join(root, "layout-"+test.name)
			_, err := BuildDefinitionSupervised(ctx, def, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH}, SupervisedPlanOptions{
				Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
				Lifecycle: LifecycleControls{CompatVolumes: test.compat},
				Output:    Output{Path: layout}, ImageStoreDir: base.imageStoreDir,
				Stdout: io.Discard, Stderr: os.Stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			manifest, _ := readPlanImage(t, layout)
			last := filepath.Join(layout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded())
			if got := readLayerFile(t, last, "proof"); got != test.want {
				t.Fatalf("proof = %q, want %q", got, test.want)
			}
		})
	}
}
