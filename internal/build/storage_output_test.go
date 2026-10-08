package build

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageOutputRejectsInvalidCombinationsBeforeBuild(t *testing.T) {
	definition := filepath.Join(t.TempDir(), "missing.coopr")
	archive := filepath.Join(t.TempDir(), "image.oci.tar")
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"push without tag", Options{Push: true}, "--push requires --tag"},
		{"push to archive", Options{Push: true, Tag: "oci-archive:" + archive}, "--push cannot use"},
		{"malformed push target", Options{Push: true, Tag: "bad tag"}, "push target"},
		{"push with engine transport", Options{Push: true, Tag: "docker:app:dev"}, "--push cannot use"},
		{"digest as tag", Options{Tag: "localhost/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, "must not include a digest"},
		{"malformed tag", Options{Tag: "bad tag"}, "invalid local image tag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.File = definition
			_, err := Run(context.Background(), tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run error = %v; want %q before opening definition or contacting builder", err, tc.want)
			}
			if _, err := os.Stat(archive); !os.IsNotExist(err) {
				t.Fatalf("unexpected archive on invalid options: %v", err)
			}
		})
	}
}
