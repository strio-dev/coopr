package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"coopr/internal/oci"
)

func TestComponentCopyDoesNotLoadNativeImageStorage(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "storage.conf")
	if err := os.WriteFile(config, []byte("[storage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_STORAGE_CONF", config)
	t.Setenv("XDG_DATA_HOME", dir)
	var stdout, stderr bytes.Buffer
	status := runContextWithStorageNamespace(t.Context(), []string{"component", "copy", "missing:latest", "local:copy"}, &stdout, &stderr, func() error {
		t.Fatal("component copy initialized image storage namespace")
		return nil
	})
	if status == 0 || !strings.Contains(stderr.String(), "missing:latest") || strings.Contains(stderr.String(), "TOML") {
		t.Fatalf("status=%d stderr=%s", status, &stderr)
	}
}

func TestCopyCommandsRejectInvalidInputsBeforeTransfer(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"copy"}, "accepts 2 arg(s)"},
		{[]string{"copy", "sha256:" + strings.Repeat("a", 64), "docker:"}, "requires a name"},
		{[]string{"copy", "--platform", "linux/", "missing:latest", "local:copy"}, "invalid copy platform"},
		{[]string{"copy", "--sign-passphrase-file", "secret", "missing:latest", "registry:example.com/app:test"}, "requires --sign-by or --sign-by-sigstore-private-key"},
		{[]string{"copy", "--sign-by-sigstore-private-key", "cosign.key", "missing:latest", "local:copy"}, "requires a registry"},
		{[]string{"copy", "--sign-by", "0123456789ABCDEF", "--sign-by-sigstore-private-key", "cosign.key", "missing:latest", "registry:example.com/app:test"}, "mutually exclusive"},
		{[]string{"component", "copy", "sha256:" + strings.Repeat("a", 64), "docker:app:dev"}, "cannot store Coopr component"},
	} {
		var out, diagnostics bytes.Buffer
		if code := run(test.args, &out, &diagnostics); code == 0 || !strings.Contains(diagnostics.String(), test.want) {
			t.Fatalf("%v: status=%d stderr=%q", test.args, code, diagnostics.String())
		}
		if out.Len() != 0 {
			t.Fatalf("%v: failed copy wrote stdout %q", test.args, out.String())
		}
	}
}

func TestImageCopyPlatformFlagDefaultsToNativeLinux(t *testing.T) {
	command := newCopyCommand(oci.Image)
	flag := command.Flags().Lookup("platform")
	if flag == nil || flag.DefValue != "linux/"+runtime.GOARCH {
		t.Fatalf("copy platform flag = %+v, want linux/%s", flag, runtime.GOARCH)
	}
	if flag := newCopyCommand(oci.Component).Flags().Lookup("platform"); flag != nil {
		t.Fatalf("component copy unexpectedly has image platform flag: %+v", flag)
	}
	for _, name := range []string{"sign-by", "sign-by-sigstore-private-key", "sign-passphrase-file"} {
		if flag := command.Flags().Lookup(name); flag == nil {
			t.Fatalf("image copy lacks --%s", name)
		}
		if flag := newCopyCommand(oci.Component).Flags().Lookup(name); flag != nil {
			t.Fatalf("component copy unexpectedly has --%s", name)
		}
	}
}
