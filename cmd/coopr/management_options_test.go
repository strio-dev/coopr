package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"coopr/internal/buildah"
	"coopr/internal/imagestore"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage"
	"go.podman.io/storage"
)

func TestImageManagementOptions(t *testing.T) {
	root := newRootCommand()
	for path, options := range map[string][]string{
		"images":        {"quiet", "format", "filter", "all", "digests", "no-trunc", "noheading", "sort"},
		"image inspect": {"format"}, "image rm": {"force", "ignore", "all", "no-prune"},
		"image prune": {"filter"}, "system prune": {"filter"}, "system df": {"format", "verbose"},
	} {
		cmd, _, err := root.Find(strings.Fields(path))
		if err != nil {
			t.Fatal(err)
		}
		for _, option := range options {
			if cmd.Flags().Lookup(option) == nil {
				t.Errorf("%s missing %s", path, option)
			}
		}
	}
	cmd, _, err := root.Find([]string{"image", "list"})
	if err != nil || cmd.Name() != "ls" {
		t.Fatalf("list alias: %v, %v", cmd, err)
	}
}

func TestImageRemovalExitClassification(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{"missing", fmt.Errorf("missing: %w", storage.ErrImageUnknown), 1},
		{"missing-layer", storage.ErrLayerUnknown, 1},
		{"in-use", storage.ErrImageUsedByContainer, 2},
		{"other", errors.New("permission denied"), 125},
		{"mixed", errors.Join(storage.ErrImageUnknown, errors.New("permission denied")), 125},
		{"mixed-in-use", errors.Join(storage.ErrImageUnknown, storage.ErrImageUsedByContainer, errors.New("permission denied")), 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			var exit interface{ ExitCode() int }
			err := imageRemovalError(test.err)
			if !errors.As(err, &exit) || exit.ExitCode() != test.status {
				t.Fatalf("error %v status %v", err, exit)
			}
		})
	}
}

func TestImagesFilteringFormattingAndInspectPartialSuccess(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	var id string
	if err := buildah.WithStore(options, func(backend storage.Store) error {
		for _, name := range []string{"alpha", "beta"} {
			layout, descriptor, imageID := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: "amd64"}, name)
			if _, err := imagestore.FromStore(backend).WriteLayout(context.Background(), layout, descriptor, name); err != nil {
				return err
			}
			if name == "alpha" {
				id = imageID.Encoded()
				if err := backend.AddNames(id, []string{"localhost/alpha:second", "coopr.internal/cache:hidden"}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	runCommand := func(args ...string) (int, string, string) {
		t.Helper()
		var out, stderr bytes.Buffer
		code := run(append([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs"}, args...), &out, &stderr)
		return code, out.String(), stderr.String()
	}
	code, out, stderr := runCommand("images", "alpha", "--quiet", "--no-trunc")
	if code != 0 || strings.TrimSpace(out) != "sha256:"+id {
		t.Fatalf("quiet dedup: %d %q %q", code, out, stderr)
	}
	code, out, stderr = runCommand("images", "--filter", "label=test=alpha", "--format", "{{.Repository}}:{{.Tag}} {{.ID}}")
	if code != 0 || strings.Contains(out, "beta") || strings.Contains(out, "coopr.internal") || !strings.Contains(out, "localhost/alpha") {
		t.Fatalf("filtered template: %d %q %q", code, out, stderr)
	}
	code, out, stderr = runCommand("images", "--format", "json")
	var rows []struct {
		ID      string `json:"Id"`
		Size    int64
		Created int64
	}
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 3 {
		t.Fatalf("JSON: %d %q %q", code, out, stderr)
	}
	for _, row := range rows {
		if len(row.ID) != 64 {
			t.Fatalf("JSON truncated image ID: %+v", row)
		}
	}
	code, out, stderr = runCommand("image", "inspect", "alpha", "missing", "beta", "--format", "{{.Config.Labels.test}}")
	if code == 0 || strings.TrimSpace(out) != "alpha\nbeta" || !strings.Contains(stderr, "missing") {
		t.Fatalf("partial inspect: %d %q %q", code, out, stderr)
	}
	code, out, stderr = runCommand("image", "rm", "missing")
	if code != 1 || out != "" {
		t.Fatalf("missing rm: %d %q %q", code, out, stderr)
	}
	for _, flag := range []string{"--ignore", "--force"} {
		code, out, stderr = runCommand("image", "rm", flag, "missing")
		if code != 0 || out != "" {
			t.Fatalf("ignore rm: %d %q %q", code, out, stderr)
		}
	}
	code, out, stderr = runCommand("image", "rm", "beta", "missing")
	if code != 1 || !strings.Contains(out, "Deleted:") {
		t.Fatalf("partial rm: %d %q %q", code, out, stderr)
	}
	code, out, stderr = runCommand("image", "rm", "--all")
	if code != 0 || !strings.Contains(out, "Deleted:") {
		t.Fatalf("all rm: %d %q %q", code, out, stderr)
	}
}

func TestDiskUsageFormatsAndVerbose(t *testing.T) {
	row := diskUsageRow{Type: "Images", Total: 3, Active: "1", RawSize: 1024, RawReclaimable: 512, ReclaimableKnown: true}
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["TotalCount"] != float64(3) || decoded["RawSize"] != float64(1024) || decoded["Size"] != "1.024kB" {
		t.Fatalf("df JSON: %s", data)
	}
	var out bytes.Buffer
	if err := writeVerboseImageUsage(&out, []libimage.ImageDiskUsage{{ID: strings.Repeat("a", 64), Repository: "localhost/app", Tag: "latest", Created: time.Now(), Size: 1024, SharedSize: 512, UniqueSize: 512, Containers: 1}}, 2, 32); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"SHARED SIZE", "UNIQUE SIZE", "localhost/app", "Components space usage:"} {
		if !strings.Contains(out.String(), expected) {
			t.Fatalf("missing %q: %s", expected, &out)
		}
	}
	options := maintenanceStoreOptions(t.TempDir())
	var stderr bytes.Buffer
	status := run([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "system", "df", "--format", "json", "--verbose"}, &out, &stderr)
	if status == 0 || !strings.Contains(stderr.String(), "cannot combine") {
		t.Fatalf("conflicting df options: %d %s", status, &stderr)
	}
}
