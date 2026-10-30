package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/strio-dev/coopr/internal/buildcontext"
)

func TestArchiveContextsKeepPrivateEnvelope(t *testing.T) {
	for _, mode := range []int64{0o755, 0o777} {
		for _, source := range []string{"archive", "http"} {
			t.Run(source+"/"+strconv.FormatInt(mode, 8), func(t *testing.T) {
				shared := t.TempDir()
				if err := os.Chmod(shared, 0o777); err != nil {
					t.Fatal(err)
				}
				t.Setenv("TMPDIR", shared)
				var data bytes.Buffer
				writer := tar.NewWriter(&data)
				if err := writer.WriteHeader(&tar.Header{Name: ".", Typeflag: tar.TypeDir, Mode: mode}); err != nil {
					t.Fatal(err)
				}
				if err := writer.WriteHeader(&tar.Header{Name: "message", Mode: 0o644, Size: 5}); err != nil {
					t.Fatal(err)
				}
				if _, err := io.WriteString(writer, "hello"); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				var primary PrimaryContext
				var cleanup func() error
				var err error
				if source == "archive" {
					primary, cleanup, err = MaterializeArchiveContext(context.Background(), bytes.NewReader(data.Bytes()))
				} else {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						_, _ = w.Write(data.Bytes())
					}))
					defer server.Close()
					primary, cleanup, err = MaterializePrimaryContext(context.Background(), buildcontext.Spec{Kind: buildcontext.HTTPArchive, Reference: server.URL}, nil, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := cleanup(); err != nil {
						t.Error(err)
					}
				}()
				info, err := os.Stat(primary.Path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != os.FileMode(mode) {
					t.Fatalf("archive root mode = %o, want %o", info.Mode().Perm(), mode)
				}
				envelope := filepath.Dir(primary.Path)
				info, err = os.Stat(envelope)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o700 {
					t.Fatalf("temporary envelope mode = %o, want 700", info.Mode().Perm())
				}
				message, err := os.ReadFile(filepath.Join(primary.Path, "message"))
				if err != nil || string(message) != "hello" {
					t.Fatalf("archive payload = %q, %v", message, err)
				}
				if err := cleanup(); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(envelope); !os.IsNotExist(err) {
					t.Fatalf("temporary envelope remains after cleanup: %v", err)
				}
			})
		}
	}
}

func TestArchiveContextsCleanUpExtractionFailure(t *testing.T) {
	for _, source := range []string{"archive", "http"} {
		t.Run(source, func(t *testing.T) {
			temporary := t.TempDir()
			t.Setenv("TMPDIR", temporary)
			var err error
			if source == "archive" {
				_, _, err = MaterializeArchiveContext(context.Background(), bytes.NewReader([]byte("not a tar archive")))
			} else {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(w, "not a tar archive")
				}))
				defer server.Close()
				_, _, err = MaterializePrimaryContext(context.Background(), buildcontext.Spec{Kind: buildcontext.HTTPArchive, Reference: server.URL}, nil, nil)
			}
			if err == nil {
				t.Fatal("invalid archive was accepted")
			}
			entries, err := os.ReadDir(temporary)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("failed extraction left temporary files: %v", entries)
			}
		})
	}
}
