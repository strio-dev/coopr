package buildah

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/strio-dev/coopr/api"
)

func TestSelectedFileExportsCopyOnlyDeclaredPaths(t *testing.T) {
	root, output := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "reports", "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"tool": "executable\n", "reports/nested/result": "report\n", "private": "not selected\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("nested/result", filepath.Join(root, "reports", "latest")); err != nil {
		t.Fatal(err)
	}
	exports := []api.FileExport{{Source: "/tool", Destination: filepath.Join(output, "tool")}, {Source: "/reports", Destination: filepath.Join(output, "reports")}}
	receipts, err := exportFilePaths(t.Context(), root, nil, nil, exports, timestampPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"tool", "reports/nested/result"} {
		data, err := os.ReadFile(filepath.Join(output, name))
		if err != nil || string(data) != []string{"executable\n", "report\n"}[i] || receipts[i].Status != "passed" {
			t.Fatalf("selected payload %s: receipt=%+v bytes=%q err=%v", name, receipts[i], data, err)
		}
	}
	if link, err := os.Readlink(filepath.Join(output, "reports", "latest")); err != nil || link != "nested/result" {
		t.Fatalf("internal relative symlink changed: %q %v", link, err)
	}
	info, err := os.Stat(exports[0].Destination)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("executable mode changed: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(output, "private")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unselected path exported: %v", err)
	}
}

func TestSelectedFileExportsRejectUnsafePaths(t *testing.T) {
	for _, source := range []string{"relative", "/../escape", "/a/../../escape", "/glob*", "/nul\x00"} {
		if err := ValidateFileExports([]api.FileExport{{Source: source, Destination: filepath.Join(t.TempDir(), "out")}}); err == nil {
			t.Errorf("accepted source %q", source)
		}
	}
	for _, chain := range []bool{false, true} {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "reports", "d"), 0755); err != nil {
			t.Fatal(err)
		}
		target := "../../outside"
		if chain {
			if err := os.Symlink("..", filepath.Join(root, "reports", "d", "up")); err != nil {
				t.Fatal(err)
			}
			target = "d/up/../outside"
		}
		if err := os.Symlink(target, filepath.Join(root, "reports", "escape")); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(t.TempDir(), "reports")
		results, err := exportFilePaths(t.Context(), root, nil, nil, []api.FileExport{{Source: "/reports", Destination: destination}}, timestampPolicy{})
		if err == nil || len(results) != 1 || results[0].Status != "failed" {
			t.Fatalf("escaping directory link exported (chain=%t): %+v %v", chain, results, err)
		}
		if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unsafe output published: %v", err)
		}
	}
}

func TestSelectedFileExportPromotionRequiresMatchingCleanup(t *testing.T) {
	for _, mode := range []string{"complete", "uncertain", "canceled", "forged", "raced"} {
		t.Run(mode, func(t *testing.T) {
			exports := []api.FileExport{{Source: "/report", Destination: filepath.Join(t.TempDir(), "report")}}
			staged, directories, err := stageFileExports(exports)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(directories[0]) })
			if err := os.WriteFile(staged[0].Destination, []byte("diagnostic\n"), 0600); err != nil {
				t.Fatal(err)
			}
			receipts := []api.ExportResult{{Source: staged[0].Source, Destination: staged[0].Destination, Status: "passed"}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			if mode == "forged" {
				receipts[0].Destination = exports[0].Destination
			}
			if mode == "raced" {
				if err := os.WriteFile(exports[0].Destination, []byte("keep\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			results, err := publishFileExports(ctx, exports, staged, receipts, mode != "uncertain")
			data, readErr := os.ReadFile(exports[0].Destination)
			switch mode {
			case "complete":
				if err != nil || readErr != nil || string(data) != "diagnostic\n" || results[0].Status != "passed" {
					t.Fatalf("complete export: %+v %q %v %v", results, data, err, readErr)
				}
			case "raced":
				if err == nil || readErr != nil || string(data) != "keep\n" || results[0].Status != "failed" {
					t.Fatalf("racing output overwritten: %+v %q %v %v", results, data, err, readErr)
				}
			default:
				if !errors.Is(readErr, os.ErrNotExist) || results[0].Status != "skipped" || mode == "forged" && err == nil {
					t.Fatalf("unconfirmed export published: %+v %q %v %v", results, data, err, readErr)
				}
			}
		})
	}
}
