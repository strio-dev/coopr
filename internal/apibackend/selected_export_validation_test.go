package apibackend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/api"
)

func TestUnknownFileExportStateIsInvalidRequest(t *testing.T) {
	r := request(t)
	destination := filepath.Join(filepath.Dir(r.Options.ContextDir), "report")
	r.Options.Output.Exports = []api.FileExport{{State: "missing", Source: "/report", Destination: destination}}

	_, err := Execute(context.Background(), r, nil, nil)
	var failure *api.Error
	if !errors.As(err, &failure) || failure.Code != "invalid_request" {
		t.Fatalf("unknown export state error = %v, want invalid_request", err)
	}
	if !strings.Contains(failure.Message, `file export selects unknown state "missing"`) {
		t.Fatalf("unknown export state diagnostic = %q", failure.Message)
	}
	for _, path := range []string{r.Options.Store.RunRoot, r.Options.Store.GraphRoot, r.Options.Output.OCI, destination} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid selector mutated %s: %v", path, err)
		}
	}
}

func TestFileExportStateValidationAcceptsKnownAndDefaultStates(t *testing.T) {
	for _, selector := range []string{"", "result", "diagnostics"} {
		t.Run(selector, func(t *testing.T) {
			r := request(t)
			r.Program.States = append(r.Program.States, api.State{ID: "diagnostics", From: api.Source{Image: "scratch"}})
			r.Options.Output.Exports = []api.FileExport{{State: selector, Source: "/report", Destination: filepath.Join(filepath.Dir(r.Options.ContextDir), "report")}}
			if err := validate(r); err != nil {
				t.Fatalf("valid export selector %q rejected: %v", selector, err)
			}
		})
	}
}

func TestAliasedFileExportDestinationsAreInvalidRequest(t *testing.T) {
	r := request(t)
	root := filepath.Dir(r.Options.ContextDir)
	parent := filepath.Join(root, "reports")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "reports-alias")
	if err := os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	r.Options.Output.Exports = []api.FileExport{
		{Source: "/first", Destination: filepath.Join(parent, "report")},
		{Source: "/second", Destination: filepath.Join(alias, "report")},
	}

	_, err := Execute(context.Background(), r, nil, nil)
	var failure *api.Error
	if !errors.As(err, &failure) || failure.Code != "invalid_request" {
		t.Fatalf("aliased export destinations error = %v, want invalid_request", err)
	}
	if !strings.Contains(failure.Message, "destinations overlap") {
		t.Fatalf("aliased export destinations diagnostic = %q", failure.Message)
	}
	for _, path := range []string{r.Options.Store.RunRoot, r.Options.Store.GraphRoot, r.Options.Output.OCI, filepath.Join(parent, "report")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid destinations mutated %s: %v", path, err)
		}
	}
}
