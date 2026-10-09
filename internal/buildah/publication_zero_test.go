package buildah

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"
)

func TestValidatePublicationGraphAcceptsConfigOnlyComponent(t *testing.T) {
	plan := testPublicationPlan(t, "extend\nenv CONFIG_ONLY=\"yes\"\n")
	stages, outputs, err := validatePublicationGraph(plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 0 || len(outputs) != 0 {
		t.Fatalf("config-only publication stages=%v outputs=%v", stages, outputs)
	}
	packages, err := PublishPlan(context.Background(), plan, PublicationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 0 {
		t.Fatalf("config-only publication packages=%v", packages)
	}
}

func TestPublishPlanSupervisedWritesConfigOnlyComponentLayout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live config-only component publication in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	output := filepath.Join(root, "layout")
	plan := testPublicationPlan(t, "extend\nenv CONFIG_ONLY=\"yes\"\n")
	result, err := PublishPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store: StoreOptions{
			RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
		},
		ContextDir: root, Isolation: "rootless", Output: Output{Path: output},
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Root.Digest == "" || result.Layout != output {
		t.Fatalf("config-only publication result = %+v", result)
	}
}
