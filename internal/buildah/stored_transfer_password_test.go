package buildah

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	imagecli "go.podman.io/image/v5/pkg/cli"
)

func TestStoredTransferPassphrasesUseNativeFilesAndRawStagedSecrets(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "explicit")
	raw := filepath.Join(dir, "raw")
	if err := os.WriteFile(explicit, []byte("first\r\nignored"), 0600); err != nil {
		t.Fatal(err)
	}
	native, err := imagecli.ReadPassphraseFile(explicit)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("first\r\nsecond\n")
	if err := os.WriteFile(raw, secret, 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		options  StoredTransferOptions
		sigstore []byte
		gpg      string
	}{
		{"explicit Sigstore", StoredTransferOptions{SigningPassphraseFile: explicit}, []byte(native), ""},
		{"explicit GPG", StoredTransferOptions{SigningPassphraseFile: explicit, SignBy: "key"}, nil, native},
		{"explicit Sequoia", StoredTransferOptions{SigningPassphraseFile: explicit, SequoiaFingerprint: "key"}, nil, native},
		{"staged environment", StoredTransferOptions{SigstorePassphraseFile: raw}, secret, ""},
		{"independent signing modes", StoredTransferOptions{SigningPassphraseFile: explicit, SignBy: "key", SigstorePassphraseFile: raw}, secret, native},
		{"helper continuation", StoredTransferOptions{SigningPassphraseFile: explicit, SignBy: "key", sigstorePassword: &secret}, secret, native},
	} {
		t.Run(test.name, func(t *testing.T) {
			sigstore, gpg, err := storedTransferPassphrases(test.options)
			if err != nil || !bytes.Equal(sigstore, test.sigstore) || gpg != test.gpg {
				t.Fatalf("passphrase transport changed native/raw bytes: %v", err)
			}
		})
	}
}
