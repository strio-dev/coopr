package main

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/imagestore"
	"coopr/internal/transfer"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	imagecopy "go.podman.io/image/v5/copy"
)

func TestPullNativePlatformFlags(t *testing.T) {
	for _, args := range [][]string{{"--arch=arm64"}, {"--os=linux", "--arch=arm", "--variant=v7"}, {"--platform=linux/arm/v7"}} {
		cmd := newImagePullCommand()
		if err := cmd.ParseFlags(args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	cmd := newImagePullCommand()
	cmd.SetArgs([]string{"--platform=linux/arm64", "--arch=amd64", "example.invalid/image"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "platform") {
		t.Fatalf("conflicting selectors: %v", err)
	}
}

func TestPushNativeCompressionSwitches(t *testing.T) {
	for _, args := range [][]string{{"--compress"}, {"--compress=false"}} {
		cmd := &cobra.Command{Use: "push"}
		var flags pushTransferFlags
		flags.addTo(cmd, false)
		if err := cmd.ParseFlags(args); err != nil {
			t.Errorf("%v: %v", args, err)
			continue
		}
		var options transfer.Options
		if err := flags.apply(cmd, &options); err != nil {
			t.Fatal(err)
		}
		native := &imagecopy.Options{PreserveDigests: true}
		if err := options.Push.Apply(native); err != nil {
			t.Fatal(err)
		}
		want := args[0] == "--compress"
		if native.DestinationCtx.DirForceCompress != want {
			t.Fatalf("%v: context=%+v", args, native.DestinationCtx)
		}
		if want && native.PreserveDigests {
			t.Fatal("compression must allow manifest changes")
		}
	}
}

func TestImagesNativeHistoryPresentation(t *testing.T) {
	ctx := context.Background()
	options := maintenanceStoreOptions(t.TempDir())
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	layout, manifest, _ := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: "amd64"}, "history")
	if _, err := store.WriteLayout(ctx, layout, manifest, "history-current"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	args := []string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "images", "--history"}
	if status := run(args, &stdout, &stderr); status != 0 {
		t.Fatalf("history: %d %s", status, &stderr)
	}
	if !strings.Contains(stdout.String(), "HISTORY") || !strings.Contains(stdout.String(), "<none>") {
		t.Fatalf("native history column missing: %s", &stdout)
	}
}

func TestPullPlatformSelection(t *testing.T) {
	for _, test := range []struct {
		args              []string
		os, arch, variant string
	}{
		{[]string{"--arch=aarch64"}, "linux", "arm64", ""},
		{[]string{"--os=windows", "--arch=amd64"}, "windows", "amd64", ""},
		{[]string{"--arch=arm", "--variant=v7"}, "linux", "arm", "v7"},
		{[]string{"--platform=linux/arm/v6"}, "linux", "arm", "v6"},
		{[]string{"--platform=local"}, runtime.GOOS, runtime.GOARCH, ""},
	} {
		cmd := newImagePullCommand()
		if err := cmd.ParseFlags(test.args); err != nil {
			t.Fatal(err)
		}
		value, err := cmd.Flags().GetString("platform")
		if err != nil {
			t.Fatal(err)
		}
		platform, err := pullPlatform(cmd, value)
		if err != nil {
			t.Fatal(err)
		}
		if platform.OS != test.os || platform.Architecture != test.arch || platform.Variant != test.variant {
			t.Fatalf("%v: %+v", test.args, platform)
		}
	}
}

func TestImageNameHistorySkipsCurrentName(t *testing.T) {
	for _, test := range []struct {
		history []string
		want    string
	}{
		{nil, "<none>"},
		{[]string{"localhost/current:latest"}, "<none>"},
		{[]string{"localhost/current:latest", "localhost/old:latest"}, "localhost/old:latest"},
		{[]string{"localhost/old:latest", "localhost/current:latest"}, "localhost/old:latest, localhost/current:latest"},
	} {
		row := imageListRow{Repository: "localhost/current", Tag: "latest", history: test.history}
		if got := row.History(); got != test.want {
			t.Fatalf("%v: got %q, want %q", test.history, got, test.want)
		}
	}
}

func TestPushOmitsUnusedBlobCacheCompressionOption(t *testing.T) {
	cmd := &cobra.Command{Use: "push"}
	var flags pushTransferFlags
	flags.addTo(cmd, false)
	if cmd.Flags().Lookup("disable-compression") != nil || cmd.Flags().ShorthandLookup("D") != nil {
		t.Fatal("push exposes unused blob-cache compression option")
	}
}
