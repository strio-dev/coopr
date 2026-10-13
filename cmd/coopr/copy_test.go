package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	status := runContextWithStorageNamespace(t.Context(), []string{"component", "copy", "missing:latest", "copy:latest"}, &stdout, &stderr, func() error {
		t.Fatal("component copy initialized image storage namespace")
		return nil
	})
	if status == 0 || !strings.Contains(stderr.String(), "missing:latest") || strings.Contains(stderr.String(), "TOML") {
		t.Fatalf("status=%d stderr=%s", status, &stderr)
	}
}

func TestPushCommandsRejectInvalidInputsBeforeTransfer(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"copy"}, "requires at least 2 arg(s)"},
		{[]string{"push", "sha256:" + strings.Repeat("a", 64), "docker-daemon:"}, "requires a name"},
		{[]string{"push", "--platform", "linux/", "missing:latest", "destination:latest"}, "invalid"},
		{[]string{"push", "--sign-passphrase-file", "secret", "missing:latest", "docker://example.com/app:test"}, "requires --sign-by or --sign-by-sigstore-private-key"},
		{[]string{"push", "--sign-by-sigstore-private-key", "cosign.key", "missing:latest", "oci-archive:copy.tar"}, "requires a registry"},
		{[]string{"push", "--sign-by", "0123456789ABCDEF", "--sign-by-sigstore-private-key", "cosign.key", "missing:latest", "docker://example.com/app:test"}, "mutually exclusive"},
		{[]string{"component", "push", "sha256:" + strings.Repeat("a", 64), "docker-daemon:app:dev"}, "cannot store Coopr component"},
	} {
		var out, diagnostics bytes.Buffer
		if code := run(test.args, &out, &diagnostics); code == 0 || !strings.Contains(diagnostics.String(), test.want) {
			t.Fatalf("%v: status=%d stderr=%q", test.args, code, diagnostics.String())
		}
		if out.Len() != 0 {
			t.Fatalf("%v: failed transfer wrote stdout %q", test.args, out.String())
		}
	}
}

func TestImagePushPlatformFlagDefaultsToNativeLinux(t *testing.T) {
	command := newImagePushCommand()
	flag := command.Flags().Lookup("platform")
	if flag == nil || flag.DefValue != "linux/"+runtime.GOARCH {
		t.Fatalf("push platform flag = %+v, want linux/%s", flag, runtime.GOARCH)
	}
	if flag := newComponentPushCommand().Flags().Lookup("platform"); flag != nil {
		t.Fatalf("component push unexpectedly has image platform flag: %+v", flag)
	}
	for _, name := range []string{"sign-by", "sign-by-sigstore-private-key", "sign-passphrase-file"} {
		if flag := command.Flags().Lookup(name); flag == nil {
			t.Fatalf("image push lacks --%s", name)
		}
		if flag := newComponentPushCommand().Flags().Lookup(name); flag != nil {
			t.Fatalf("component push unexpectedly has --%s", name)
		}
	}
}

func TestCopyIsTagAlias(t *testing.T) {
	root := newRootCommand()
	for _, prefix := range [][]string{nil, {"image"}, {"component"}} {
		tag, _, err := root.Find(append(append([]string{}, prefix...), "tag"))
		if err != nil {
			t.Fatal(err)
		}
		copied, _, err := root.Find(append(append([]string{}, prefix...), "copy"))
		if err != nil {
			t.Fatal(err)
		}
		if tag != copied {
			t.Fatalf("%v copy and tag resolve to different commands", prefix)
		}
		if tag.Flags() != copied.Flags() {
			t.Fatal("copy flags diverge from tag")
		}
		for _, name := range []string{"platform", "authfile", "cert-dir", "tls-verify", "sign-by", "sign-by-sigstore-private-key", "sign-passphrase-file", "signature-policy"} {
			if tag.Flags().Lookup(name) != nil {
				t.Fatalf("%v tag/copy exposes transfer-only --%s", prefix, name)
			}
		}
	}
}

func TestTagAndCopyRejectTransportDestinations(t *testing.T) {
	for _, prefix := range [][]string{nil, {"image"}, {"component"}} {
		for _, command := range []string{"tag", "copy"} {
			for _, destination := range []string{"docker://example.com/app:latest", "docker-daemon:app:latest", "oci-archive:/tmp/app.tar"} {
				var out, errs bytes.Buffer
				args := append(append([]string{}, prefix...), command, "missing", destination)
				if code := run(args, &out, &errs); code == 0 || !strings.Contains(errs.String(), "invalid") {
					t.Fatalf("%v status=%d stderr=%s", args, code, &errs)
				}
				if out.Len() != 0 {
					t.Fatalf("failed tag/copy printed %q", &out)
				}
			}
		}
	}
}

func TestTagAndCopyRejectTransportSources(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	globals := []string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs"}
	for _, prefix := range [][]string{nil, {"image"}} {
		for _, command := range []string{"tag", "copy"} {
			for _, source := range []string{"docker://example.com/app:latest", "docker-daemon:app:latest", "oci-archive:/tmp/app.tar"} {
				var out, errs bytes.Buffer
				args := append(append(append([]string{}, globals...), prefix...), command, source, "destination")
				if code := run(args, &out, &errs); code != 125 || !strings.Contains(errs.String(), "unsupported transport") || !strings.Contains(errs.String(), "for looking up local images") {
					t.Fatalf("%v status=%d stderr=%s", args, code, &errs)
				}
				if out.Len() != 0 {
					t.Fatalf("failed tag/copy printed %q", &out)
				}
			}
		}
	}
}

func TestImagePushRequiresLocalSource(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	globals := []string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs"}
	for _, source := range []string{"docker://example.com/app:latest", "docker-daemon:app:latest", "oci-archive:/tmp/app.tar"} {
		var out, errs bytes.Buffer
		args := append(append([]string{}, globals...), "push", "--quiet", source, "oci-archive:"+filepath.Join(t.TempDir(), "image.tar"))
		if code := run(args, &out, &errs); code != 125 || !strings.Contains(errs.String(), "unsupported transport") || !strings.Contains(errs.String(), "for looking up local images") {
			t.Fatalf("%v status=%d stderr=%s", args, code, &errs)
		}
		if out.Len() != 0 {
			t.Fatalf("failed push printed %q", &out)
		}
	}
}
