package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/moby/sys/capability"
)

// withoutDACOverrides keeps chmod-based permission failures meaningful after
// rootless TestMain reexecs as namespace root. Capabilities are thread-local.
func withoutDACOverrides(t *testing.T) func() {
	t.Helper()
	runtime.LockOSThread()
	original, err := capability.NewPid2(0)
	if err == nil {
		err = original.Load()
	}
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	if !original.Get(capability.EFFECTIVE, capability.CAP_DAC_OVERRIDE) && !original.Get(capability.EFFECTIVE, capability.CAP_DAC_READ_SEARCH) {
		runtime.UnlockOSThread()
		return func() {}
	}
	restricted, err := capability.NewPid2(0)
	if err == nil {
		err = restricted.Load()
	}
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	restricted.Unset(capability.EFFECTIVE, capability.CAP_DAC_OVERRIDE, capability.CAP_DAC_READ_SEARCH)
	if err := restricted.Apply(capability.CAPS); err != nil {
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	return func() {
		if err := original.Apply(capability.CAPS); err != nil {
			// Leave the thread locked so Go discards it on test termination.
			t.Fatalf("restore cleanup-test capabilities: %v", err)
		}
		runtime.UnlockOSThread()
	}
}

func TestStoredTransferOptionsReportsPasswordCleanupFailure(t *testing.T) {
	defer withoutDACOverrides(t)()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("COSIGN_PASSWORD", "cleanup-test-password")
	options, cleanup, err := storedTransferOptions("image", "", Options{BuildStore: nativeTestStore(t.TempDir()), Signing: SigningOptions{SigstorePrivateKeyFile: "key"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0700); err != nil {
			t.Error(err)
		}
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("cleanup error = %v, want permission error", err)
	}
	if _, err := os.Stat(options.SigstorePassphraseFile); err != nil {
		t.Fatalf("denied cleanup unexpectedly removed passphrase: %v", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("repeated cleanup: %v", err)
	}
	if _, err := os.Stat(options.SigstorePassphraseFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passphrase remains: %v", err)
	}
}

func TestStoredTransferOptionsDoesNotRemoveUserPassphrase(t *testing.T) {
	t.Setenv("COSIGN_PASSWORD", "ignored-environment-password")
	file := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(file, []byte("user-password"), 0600); err != nil {
		t.Fatal(err)
	}
	options, cleanup, err := storedTransferOptions("image", "", Options{BuildStore: nativeTestStore(t.TempDir()), Signing: SigningOptions{SigstorePrivateKeyFile: "key", PassphraseFile: file}})
	if err != nil {
		t.Fatal(err)
	}
	if options.SigstorePassphraseFile != "" || options.SigningPassphraseFile != file {
		t.Fatalf("user passphrase options = %+v", options)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "user-password" {
		t.Fatalf("user password changed: %v", err)
	}
}
