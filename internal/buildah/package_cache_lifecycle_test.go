package buildah

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
)

func TestPackageCacheKeyIncludesOutputLifecycle(t *testing.T) {
	plan := testPublicationPlan(t, `package as="bundle" { copy "payload" "/payload" }
extend { copy "/payload" "/payload" from="bundle" }
`)
	root := t.TempDir()
	stages, outputs, err := validatePublicationGraph(plan, map[string]string{"bundle": filepath.Join(root, "bundle.tar")})
	if err != nil {
		t.Fatal(err)
	}
	options := PlanOptions{Store: StoreOptions{GraphDriverName: "vfs"}, ContextDir: root, Isolation: "rootless", Runtime: "crun"}
	// The context contents are a separate existing cache input.
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	key := func(controls LifecycleControls) digest.Digest {
		t.Helper()
		current := options
		current.Lifecycle = controls
		keys, eligible, err := packageCacheKeys(context.Background(), current, plan, stages, outputs, nil)
		if err != nil || !eligible {
			t.Fatalf("eligible=%v err=%v", eligible, err)
		}
		currentKey := keys[plan.Outputs[0]]
		priorKey := currentKey
		priorKey.Lowering = "coopr-buildah-package-v1"
		priorDigest, err := priorKey.Digest()
		if err != nil {
			t.Fatal(err)
		}
		result, err := keys[plan.Outputs[0]].Digest()
		if err != nil {
			t.Fatal(err)
		}
		if result == priorDigest {
			t.Fatal("pre-default package lowering reused the current key")
		}
		return result
	}
	baseline := key(LifecycleControls{})
	for _, test := range []struct {
		name     string
		controls LifecycleControls
	}{
		{"compat-volumes", LifecycleControls{CompatVolumes: true}},
		{"no-layers", LifecycleControls{NoLayers: true}},
		{"stage-labels", LifecycleControls{StageLabels: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if changed := key(test.controls); changed == baseline {
				t.Fatalf("%s reused the opposite output policy's whole-package key %s", test.name, changed)
			}
		})
	}
	if key(LifecycleControls{KeepFailed: true, KeepIntermediate: true, SaveStages: true}) != baseline {
		t.Fatal("retention-only lifecycle settings invalidated package output identity")
	}
}
