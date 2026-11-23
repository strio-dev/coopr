package coopr

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/api"
)

func TestExecutePreflightsAliasedFileExportDestinations(t *testing.T) {
	wasInitialized := initialized.Swap(true)
	defer initialized.Store(wasInitialized)
	root := t.TempDir()
	parent := filepath.Join(root, "outputs")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	program := Program{Output: "out", States: []State{{ID: "out", From: Source{Image: "scratch"}}}}
	for _, tc := range []struct {
		name, destination, want string
	}{
		{"same output", "report", "selected export destinations overlap"},
		{"nested output", "report/child", "selected export destinations overlap"},
		{"disjoint outputs", "report-other", "jobs must not be negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Stop after preflight even if a conflicting destination is accepted.
			options := Options{
				Jobs:  -1,
				Store: StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph")},
				Output: Output{Exports: []api.FileExport{
					{Source: "/one", Destination: filepath.Join(parent, "report")},
					{Source: "/two", Destination: filepath.Join(alias, tc.destination)},
				}},
			}
			if _, err := Execute(t.Context(), program, options); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Execute: got %v, want %q", err, tc.want)
			}
			for _, name := range []string{options.Store.RunRoot, options.Store.GraphRoot, options.Output.Exports[0].Destination, options.Output.Exports[1].Destination} {
				if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("preflight mutated %s: %v", name, err)
				}
			}
		})
	}
}
