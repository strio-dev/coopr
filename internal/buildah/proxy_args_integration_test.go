package buildah

import (
	"context"
	"encoding/json"
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

func TestBuildDefinitionPredefinedProxyArgReusesRunCache(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live predefined proxy argument build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	def, err := definition.Parse(strings.NewReader(fmt.Sprintf(`
from %q
run "printf '%%s' \"$HTTP_PROXY\" >/proof" network="none"
`, base.reference)))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		value   string
		noCache bool
		want    string
	}{
		{value: "http://proxy-one", want: "http://proxy-one"},
		{value: "http://proxy-two", want: "http://proxy-one"},
		{value: "http://proxy-two", noCache: true, want: "http://proxy-two"},
	}
	for index, test := range cases {
		layout := filepath.Join(root, fmt.Sprintf("layout-%d", index))
		_, err := BuildDefinitionSupervised(ctx, def, planner.Options{
			Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH,
			Arguments: map[string]string{"HTTP_PROXY": test.value},
		}, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output:              Output{Path: layout},
			NoCache:             test.noCache,
			SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, layout)
		config, err := json.Marshal(image)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []string{"http://proxy-one", "http://proxy-two"} {
			if strings.Contains(string(config), value) {
				t.Fatalf("undeclared proxy value %q leaked into image config/history", value)
			}
		}
		last := manifest.Layers[len(manifest.Layers)-1]
		proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof")
		if proof != test.want {
			t.Fatalf("build %d proof = %q, want %q", index+1, proof, test.want)
		}
	}
}

func TestPublishDefinitionPredefinedProxyArgReachesPackageRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live component proxy argument build in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	def, err := definition.Parse(strings.NewReader(fmt.Sprintf(`
from %q as="producer"
run "printf '%%s' \"$HTTP_PROXY\" >/proof" network="none"
package as="bundle"
copy "/proof" "/proof" from="producer"
extend
copy "/proof" "/proof" from="bundle"
`, base.reference)))
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(root, "component")
	result, err := PublishDefinitionSupervised(ctx, def, planner.Options{
		Mode: planner.Publish, Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Arguments: map[string]string{"HTTP_PROXY": "http://package-proxy"},
	}, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
		Output:              Output{Path: layout},
		SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := readComponentMetadata(t, ctx, layout, result.Root)
	if len(metadata.Packages) != 1 || metadata.Packages[0].Stage != "bundle" {
		t.Fatalf("published packages = %+v", metadata.Packages)
	}
	descriptor := metadata.Packages[0].Descriptor
	proof := readLayerFile(t, filepath.Join(layout, "blobs", "sha256", descriptor.Digest.Encoded()), "proof")
	if proof != "http://package-proxy" {
		t.Fatalf("package RUN proxy proof = %q", proof)
	}
	if strings.Contains(string(metadata.Packages[0].Config), "http://package-proxy") {
		t.Fatal("undeclared proxy value leaked into package image config/history")
	}
}
