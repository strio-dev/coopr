package buildah

import (
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildPlanAddsAuthenticatedGitRepository(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless Buildah authenticated Git ADD build in short mode")
	}
	const token = "supervised-git-token"
	source, _ := gitHTTPFixture(t, "basic eC1hY2Nlc3MtdG9rZW46c3VwZXJ2aXNlZC1naXQtdG9rZW4=")
	parsed, err := url.Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	secret := filepath.Join(root, "git-token")
	if err := os.WriteFile(secret, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, "from \"scratch\"\nadd \""+source+"\" \"/source/\"\n")
	layout := filepath.Join(root, "layout")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store: cacheTestStore(root), ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout},
		Secrets: []string{"id=GIT_AUTH_TOKEN." + parsed.Host + ",src=" + secret}, Stdout: io.Discard, Stderr: io.Discard,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("layers = %d, want 1", len(manifest.Layers))
	}
	layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	if got := readLayerFile(t, layer, "source/proof"); got != "default branch\n" {
		t.Fatalf("authenticated Git ADD proof = %q", got)
	}
}
