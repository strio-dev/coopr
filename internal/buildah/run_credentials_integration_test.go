package buildah

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildPlanSSHMountIsEphemeral(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(root, "agent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	plan := testPlan(t, fmt.Sprintf(`
from %q {
    run "test -S /run/buildkit/ssh_agent.0 && echo mounted >/proof" network="none" { mount "ssh" id="default" required="true" }
    run "test ! -e /run/buildkit/ssh_agent.0 && echo cleaned >/cleanup" network="none"
}
`, base.reference))
	layout := filepath.Join(root, "image")
	_, err = BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
		Output:              Output{Path: layout},
		SSH:                 []string{"default=" + listener.Addr().String()},
		SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) < 2 {
		t.Fatalf("image has %d layers, expected base and SSH proof", len(manifest.Layers))
	}
	cleanupLayer := manifest.Layers[len(manifest.Layers)-1]
	if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", cleanupLayer.Digest.Encoded()), "cleanup"); got != "cleaned\n" {
		t.Fatalf("SSH cleanup proof = %q", got)
	}
	proofLayer := manifest.Layers[len(manifest.Layers)-2]
	if got := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", proofLayer.Digest.Encoded()), "proof"); got != "mounted\n" {
		t.Fatalf("SSH mount proof = %q", got)
	}
}
