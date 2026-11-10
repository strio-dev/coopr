package coopr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunNetworkValidationBeforeResourceMutation(t *testing.T) {
	wasInitialized := initialized.Swap(true)
	defer initialized.Store(wasInitialized)
	for _, network := range []string{"", "none", "host", "default", "invalid"} {
		t.Run(network, func(t *testing.T) {
			root := t.TempDir()
			mode := network
			if network == "invalid" {
				mode = filepath.Join(root, "missing-netns")
			}
			options := Options{Network: mode, Store: StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph")}}
			// Missing OCI output stops valid modes before execution; invalid network must
			// be rejected earlier, without creating stores or staging directories.
			_, err := Execute(t.Context(), Program{Output: "out", States: []State{{ID: "out", From: Source{Image: "scratch"}}}}, options)
			want := "OCI output"
			if network == "invalid" {
				want = "RUN network"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("network %q: %v", mode, err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("preflight mutated resources: %v %v", entries, err)
			}
		})
	}
}
