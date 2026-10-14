package buildah

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.podman.io/image/v5/signature/sigstore"
)

func TestParameterSignerErrorsAndPassphrases(t *testing.T) {
	dir := t.TempDir()
	keys, err := sigstore.GenerateKeyPair([]byte("correct passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	password := filepath.Join(dir, "password")
	for path, data := range map[string][]byte{key: keys.PrivateKey, password: []byte("correct passphrase\n")} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name, data, fallback, gpgFallback string
		allowed                           bool
	}{
		{name: "missing file"},
		{name: "unknown field", data: "unknown: true"},
		{name: "malformed", data: "["},
		{name: "no signing key", data: "{}"},
		{name: "missing key", data: `{"privateKeyFile":"` + filepath.Join(dir, "absent") + `"}`},
		{name: "wrong fallback", data: `{"privateKeyFile":"` + key + `"}`, fallback: "wrong"},
		{name: "fallback", data: `{"privateKeyFile":"` + key + `"}`, fallback: "correct passphrase", allowed: true},
		{name: "independent fallbacks choose Sigstore", data: `{"privateKeyFile":"` + key + `"}`, fallback: "correct passphrase", gpgFallback: "other GPG password", allowed: true},
		{name: "independent fallbacks never concatenate", data: `{"privateKeyFile":"` + key + `"}`, fallback: "wrong", gpgFallback: "correct passphrase"},
		{name: "shared explicit GPG file fallback", data: `{"privateKeyFile":"` + key + `"}`, gpgFallback: "correct passphrase", allowed: true},
		{name: "parameter passphrase overrides fallback", data: `{"privateKeyFile":"` + key + `","privateKeyPassphraseFile":"` + password + `"}`, fallback: "wrong", allowed: true},
		{name: "missing passphrase file", data: `{"privateKeyFile":"` + key + `","privateKeyPassphraseFile":"` + filepath.Join(dir, "absent") + `"}`},
		{name: "invalid Fulcio", data: `{"fulcio":{}}`},
		{name: "invalid Rekor URL", data: `{"rekorURL":":%"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "parameters.yaml")
			if test.data != "" {
				if err := os.WriteFile(path, []byte(test.data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			signers, cleanup, err := NativeSigners(path, "", []byte(test.fallback), test.gpgFallback, os.Stderr)
			defer cleanup()
			if test.allowed {
				if err != nil || len(signers) != 1 {
					t.Fatalf("signers=%d err=%v", len(signers), err)
				}
			} else if err == nil {
				t.Fatal("invalid signing parameters accepted")
			}
			if err != nil && strings.Contains(err.Error(), "correct passphrase") {
				t.Fatal("error leaked passphrase")
			}
		})
	}
}
