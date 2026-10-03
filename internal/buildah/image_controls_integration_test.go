package buildah

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestImageControlsApplyAcrossColdAndWarmOutputs(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, `from "`+base.reference+`"
run "/bin/busybox printf \"$FLAG\" >/flag" network="none"
label current="yes" remove="yes"
`)
	forced := int64(123)
	for attempt := range 2 {
		layout := filepath.Join(root, "controls-"+string(rune('0'+attempt)))
		flat := layout + ".tar"
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			ImageStoreDir: base.imageStoreDir, SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
			Output: Output{Path: layout, Squash: attempt == 1, Filesystem: FilesystemOutput{Type: "tar", Path: flat}}, Timestamp: &forced,
			ImageControls: ImageControls{
				Env: []string{"FLAG=from-cli"}, UnsetEnv: []string{"FLAG"},
				Labels: []string{"cli=yes"}, UnsetLabels: []string{"remove"}, DropInheritedLabels: true,
				Annotations: []string{"org.example.test=yes"}, DropInheritedAnnotations: true, OmitHistory: true,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, layout)
		if len(image.History) != 0 {
			t.Fatalf("attempt %d history = %+v", attempt, image.History)
		}
		if slices.Contains(image.Config.Env, "FLAG=from-cli") {
			t.Fatalf("attempt %d retained unset environment: %q", attempt, image.Config.Env)
		}
		if image.Config.Labels["current"] != "yes" || image.Config.Labels["cli"] != "yes" || image.Config.Labels["remove"] != "" {
			t.Fatalf("attempt %d labels = %#v", attempt, image.Config.Labels)
		}
		if manifest.Annotations["org.example.test"] != "yes" {
			t.Fatalf("attempt %d annotations = %#v", attempt, manifest.Annotations)
		}
		file, err := os.Open(flat)
		if err != nil {
			t.Fatal(err)
		}
		reader := tar.NewReader(file)
		found := false
		for {
			header, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if header.Name == "flag" || header.Name == "./flag" {
				data, err := io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != "from-cli" || header.ModTime.Unix() != forced {
					t.Fatalf("attempt %d flag = %q mtime=%d", attempt, data, header.ModTime.Unix())
				}
				found = true
			}
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatalf("attempt %d flattened output lacks flag", attempt)
		}
	}
}
