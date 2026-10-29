package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/imagestore"
	commonconfig "go.podman.io/common/pkg/config"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func nativeDefaultsImage(t *testing.T, options buildah.StoreOptions, names ...string) {
	t.Helper()
	store, err := imagestore.NewWithOptions(buildah.NativeStoreOptions(options))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	for _, name := range names {
		layout, desc, _ := maintenanceImageLayout(t, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, name)
		if _, err := store.WriteLayout(context.Background(), layout, desc, name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeHistoryJSONFormatsEmpty(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	nativeDefaultsImage(t, options, "empty-history")
	for _, format := range []string{"json", " json ", "{{json}}", "{{json.}}"} {
		t.Run(format, func(t *testing.T) {
			var out, errs bytes.Buffer
			if status := run(imageIOArgs(options, "history", "empty-history", "--format", format), &out, &errs); status != 0 || strings.TrimSpace(out.String()) != "[]" {
				t.Fatalf("status=%d out=%q err=%q", status, &out, &errs)
			}
		})
	}
}

func TestNativeInfoTemplateFunctions(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	for _, format := range []string{"{{json .store}}", "{{upper .store.GraphDriverName}}"} {
		var out, errs bytes.Buffer
		args := []string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "info", "--format", format}
		if code := run(args, &out, &errs); code != 0 {
			t.Fatalf("format=%q status=%d err=%s", format, code, &errs)
		}
		if strings.Contains(format, "upper") && strings.TrimSpace(out.String()) != "VFS" {
			t.Fatalf("upper output=%q", &out)
		}
		if strings.Contains(format, "json") {
			var fields map[string]any
			if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
				t.Fatal(err)
			}
			if fields["GraphRoot"] != options.GraphRoot {
				t.Fatalf("store=%v", fields)
			}
		}
	}
}

func TestNativeSaveConfiguredMultiImageAndExplicitOverride(t *testing.T) {
	module := filepath.Join(t.TempDir(), "archive.conf")
	if err := os.WriteFile(module, []byte("[engine]\nmulti_image_archive=true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = commonconfig.New(&commonconfig.Options{SetDefault: true}) })
	options := maintenanceStoreOptions(t.TempDir())
	nativeDefaultsImage(t, options, "multi-one", "multi-two")
	for _, explicitFalse := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "saved.tar")
		args := append([]string{"--module", module}, imageIOArgs(options, "save", "multi-one", "multi-two", "--output", path)...)
		if explicitFalse {
			args = append(args, "--multi-image-archive=false")
		}
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code != 0 {
			t.Fatalf("explicitFalse=%t code=%d err=%s", explicitFalse, code, &errs)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		reader := tar.NewReader(file)
		count := 0
		for {
			header, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if header.Name == "manifest.json" {
				var entries []json.RawMessage
				if err := json.NewDecoder(reader).Decode(&entries); err != nil {
					t.Fatal(err)
				}
				count = len(entries)
			}
		}
		_ = file.Close()
		want := 2
		if explicitFalse {
			want = 1
		}
		if count != want {
			t.Errorf("explicitFalse=%t archive configs=%d want=%d", explicitFalse, count, want)
		}
	}
}

func TestNativePullImplicitDockerAuthAndExplicitMissingFile(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("REGISTRY_AUTH_FILE", "")
	options := maintenanceStoreOptions(t.TempDir())
	nativeDefaultsImage(t, options, "offline-auth")
	for _, explicit := range []bool{false, true} {
		args := imageIOArgs(options, "pull", "--policy=never", "offline-auth")
		if explicit {
			args = append(args, "--authfile", filepath.Join(os.Getenv("DOCKER_CONFIG"), "config.json"))
		}
		var out, errs bytes.Buffer
		code := run(args, &out, &errs)
		if !explicit && code != 0 {
			t.Errorf("implicit missing credentials: status=%d err=%s", code, &errs)
		}
		if explicit && (code == 0 || !strings.Contains(errs.String(), "auth file")) {
			t.Errorf("explicit missing credentials: status=%d err=%s", code, &errs)
		}
	}
}

func nativeDefaultsPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	if !term.IsTerminal(int(slave.Fd())) {
		t.Fatal("not a terminal")
	}
	return master, slave
}

func TestNativeSaveLoadTerminalGuards(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	nativeDefaultsImage(t, options, "terminal-output")
	master, slave := nativeDefaultsPTY(t)
	// Drain any erroneous archive output so a failed guard cannot block the test.
	go func() { _, _ = io.Copy(io.Discard, master) }()
	var errs bytes.Buffer
	if code := run(imageIOArgs(options, "save", "terminal-output"), slave, &errs); code == 0 || !strings.Contains(errs.String(), "refusing to save to terminal") {
		t.Errorf("save status=%d err=%s", code, &errs)
	}
	inputMaster, inputSlave := nativeDefaultsPTY(t)
	// A queued canonical EOF makes a missing guard fail promptly instead of hanging.
	if _, err := inputMaster.Write([]byte{4}); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCommand()
	cmd.SetIn(inputSlave)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errs)
	cmd.SetArgs(imageIOArgs(options, "load"))
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "cannot read from terminal") {
		t.Errorf("load error=%v", err)
	}
	// Explicit files remain usable even when the command's streams are terminals.
	archive := filepath.Join(t.TempDir(), "redirected.tar")
	errs.Reset()
	if code := run(imageIOArgs(options, "save", "terminal-output", "--output", archive), slave, &errs); code != 0 {
		t.Fatalf("explicit save status=%d err=%s", code, &errs)
	}
	cmd = newRootCommand()
	cmd.SetIn(inputSlave)
	cmd.SetOut(io.Discard)
	cmd.SetErr(&errs)
	cmd.SetArgs(imageIOArgs(options, "load", "--input", archive))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("explicit load: %v", err)
	}
}

func TestNativeComponentArchiveTerminalGuards(t *testing.T) {
	master, slave := nativeDefaultsPTY(t)
	go func() { _, _ = io.Copy(io.Discard, master) }()
	var errs bytes.Buffer
	if code := run([]string{"component", "save", "absent-component"}, slave, &errs); code == 0 || !strings.Contains(errs.String(), "refusing to save to terminal") {
		t.Errorf("save status=%d err=%s", code, &errs)
	}
	inputMaster, inputSlave := nativeDefaultsPTY(t)
	if _, err := inputMaster.Write([]byte{4}); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCommand()
	cmd.SetIn(inputSlave)
	cmd.SetOut(io.Discard)
	cmd.SetErr(&errs)
	cmd.SetArgs([]string{"component", "load"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "cannot read from terminal") {
		t.Errorf("load error=%v", err)
	}
}

func TestNativeBuildImplicitDockerAuth(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native build integration")
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("REGISTRY_AUTH_FILE", "")
	options := maintenanceStoreOptions(t.TempDir())
	nativeDefaultsImage(t, options, "offline-base")
	for _, component := range []bool{false, true} {
		text := `from "offline-base" { label "check" "offline" }`
		args := []string{"build"}
		if component {
			text = `extend {}`
			args = []string{"component", "build"}
		}
		file := definitionFile(t, text)
		args = append(args, file, "--pull=never")
		var out, errs bytes.Buffer
		full := append([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs"}, args...)
		if code := run(full, &out, &errs); code != 0 {
			t.Errorf("component=%t status=%d err=%s", component, code, &errs)
		}
	}
}

func TestBuildCommandsCacheTTLControls(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native build integration")
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	options := maintenanceStoreOptions(t.TempDir())
	for _, component := range []bool{false, true} {
		text := `from "scratch" { env proof="cache-ttl"; }`
		command := []string{"build"}
		if component {
			text = `extend {}`
			command = []string{"component", "build"}
		}
		file := definitionFile(t, text)
		for _, control := range []struct {
			args  []string
			valid bool
		}{
			{[]string{"--cache-ttl=-1s"}, true},
			{[]string{"--cache-ttl=0"}, true},
			{[]string{"--cache-ttl=0", "--no-cache=false"}, false},
		} {
			args := append([]string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs"}, command...)
			args = append(args, file)
			args = append(args, control.args...)
			var out, errs bytes.Buffer
			code := run(args, &out, &errs)
			if (code == 0) != control.valid {
				t.Fatalf("component=%t args=%v status=%d err=%s", component, control.args, code, &errs)
			}
			if !control.valid && !strings.Contains(errs.String(), "cannot use --cache-ttl with duration as 0 and --no-cache=false") {
				t.Fatalf("wrong native conflict error: %s", &errs)
			}
		}
	}
}
