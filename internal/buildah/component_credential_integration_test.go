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

	"coopr/internal/componentstore"
	"coopr/internal/planner"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestComponentInvocationSecretUsesConventionalInstructionCache(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	componentDir := filepath.Join(root, "components")
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	component := parseWorkerDefinition(t, `
extend
run "cat /run/secrets/token >/proof" network="none" { mount "secret" id="token" required="true" }
`)
	componentLayout := filepath.Join(root, "component")
	publication, err := PublishDefinitionSupervised(ctx, component, planner.Options{
		Mode: planner.Publish, Platform: "linux/" + runtime.GOARCH,
	}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
		Output:              Output{Path: componentLayout},
		SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := orasoci.NewWithContext(ctx, componentLayout)
	if err != nil {
		t.Fatal(err)
	}
	if err := componentstore.Put(ctx, componentDir, source, publication.Root, "credential"); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(root, "token")
	caller := parseWorkerDefinition(t, fmt.Sprintf("from %q\ncomponent \"local:credential\"\n", base.reference))
	build := func(name, value string, noCache bool) string {
		t.Helper()
		if err := os.WriteFile(secretPath, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		layout := filepath.Join(root, name)
		var logs strings.Builder
		_, err := BuildDefinitionSupervised(ctx, caller, planner.Options{
			Mode: planner.Build, Platform: "linux/" + runtime.GOARCH,
		}, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output: Output{Path: layout}, ComponentStoreDir: componentDir,
			Secrets:             []string{"id=token,src=" + secretPath},
			NoCache:             noCache,
			SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: &logs,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s worker progress:\n%s", name, logs.String())
		hit := strings.Contains(logs.String(), "--> Using cache ")
		if hit != (name == "second") {
			t.Errorf("%s instruction cache hit = %v", name, hit)
		}
		manifest, _ := readPlanImage(t, layout)
		last := manifest.Layers[len(manifest.Layers)-1]
		return readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof")
	}
	if got := build("first", "first\n", false); got != "first\n" {
		t.Fatalf("first component secret output = %q", got)
	}
	// Secret contents do not invalidate RUN, including inside components.
	if got := build("second", "second\n", false); got != "first\n" {
		t.Fatalf("warm component secret output = %q, want cached first value", got)
	}
	if got := build("no-cache", "second\n", true); got != "second\n" {
		t.Fatalf("uncached component secret output = %q, want current value", got)
	}
}
